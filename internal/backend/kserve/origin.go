package kserve

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
)

// A served model's runtime is read (interfaces.go) and asked its first
// request (answer.go) at its workload Service. On the local cluster that is
// the Service's cluster-local name, dialled directly. On a remote target the
// name resolves only inside the target cluster, so the driver goes through
// the target apiserver's Service proxy with the caller's client, like every
// other call on the target: a caller the target does not allow
// services/proxy in the serving namespace is refused there, and the read
// waits for one who is.

// serverOrigin is where a served model's runtime is reached, and with which
// client.
type serverOrigin struct {
	url    string
	client *http.Client
	// target names the remote cluster whose apiserver proxies the requests:
	// a 401 or 403 is its refusal of the caller, not the runtime's answer.
	target string
}

// serverOrigin is the origin of the workload Service of the named
// LLMInferenceService, or why it cannot be reached for ctx.
func (b *Backend) serverOrigin(ctx context.Context, name, namespace string) (serverOrigin, string) {
	if b.opts.Target.Local() {
		return serverOrigin{url: workloadURL(name, namespace), client: b.serverHTTP}, ""
	}
	if why := b.callerless(ctx, "model server read"); why != "" {
		return serverOrigin{}, why
	}
	rc := b.targetREST(ctx)
	if rc == nil || rc.Client == nil {
		return serverOrigin{}, fmt.Sprintf("no REST client for the remote target %s to reach the model server through its Service proxy", b.opts.Target.Cluster)
	}
	return serverOrigin{url: rc.Get().AbsPath(workloadProxyPath(name, namespace)).URL().String(), client: rc.Client, target: b.opts.Target.Cluster}, ""
}

// callerREST is the core/v1 REST client of the call's clients: toward a
// remote target, the caller's.
func (b *Backend) callerREST(ctx context.Context) *rest.RESTClient {
	rc, _ := b.k8s(ctx).CoreV1().RESTClient().(*rest.RESTClient)
	return rc
}

// workloadProxyPath is the apiserver path proxying to the workload Service
// of the named LLMInferenceService.
func workloadProxyPath(name, namespace string) string {
	return "/api/v1/namespaces/" + namespace + "/services/http:" + workloadService(name) + ":" + strconv.Itoa(llmisvcWorkloadPort) + "/proxy"
}

// do sends one request to the runtime; body, when set, is JSON. The target
// apiserver's refusal of the caller is an error, as no answer of the
// runtime's.
func (o serverOrigin) do(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, o.url+path, r)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := o.client.Do(req)
	if err != nil || o.target == "" || (resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden) {
		return resp, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, answerBodyLimit))
	message := strings.TrimSpace(string(raw))
	var status metav1.Status
	if json.Unmarshal(raw, &status) == nil && status.Message != "" {
		message = status.Message
	}
	return nil, fmt.Errorf("the target %s refused the caller %s: %s", o.target, resp.Status, message)
}
