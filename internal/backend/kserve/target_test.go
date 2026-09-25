package kserve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"

	"github.com/giantswarm/model-manager/internal/backend"
	"github.com/giantswarm/model-manager/internal/identity"
)

const testModelsHost = "https://models.gpu01.example"

var testTarget = backend.Target{Cluster: "gpu01", Organization: "giantswarm", APIServer: "https://api.gpu01.example:6443", CABundle: "ca", ServingNamespace: testServingNS}

// remoteFixture is the driver of an installation whose kserve backend targets
// a workload cluster, as cmd's backendBuilder wires it: the fixture's clients
// are the target reached with the caller's token; the configured clients are
// the target without a credential — the apiserver refuses every call on them
// as system:anonymous, and the test counts them.
type remoteFixture struct {
	*fixture
	anonCS  *kubefake.Clientset
	anonDyn *dynamicfake.FakeDynamicClient
}

func newRemoteFixture(t *testing.T) *remoteFixture {
	t.Helper()
	f := newFixture(t)
	anonCS := kubefake.NewSimpleClientset()
	anonDyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), llmisvcListKinds())
	deny := func(a k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: a.GetResource().Resource}, "", errors.New(`User "system:anonymous" cannot do this`))
	}
	anonCS.PrependReactor("*", "*", deny)
	anonDyn.PrependReactor("*", "*", deny)
	clientsFor := func(ctx context.Context) (kubernetes.Interface, dynamic.Interface) {
		if _, ok := identity.TokenFromContext(ctx); ok {
			return f.cs, f.dyn
		}
		return anonCS, anonDyn
	}
	for _, o := range []*backend.KServeOptions{&f.b.opts, &f.b.cfg.opts} {
		o.Clientset, o.Dynamic, o.ClientsFor, o.Target = anonCS, anonDyn, clientsFor, testTarget
	}
	f.b.cs, f.b.dyn = anonCS, anonDyn
	f.b.targetREST = f.callerTargetREST
	f.setDiscoveryOpts(context.Background(), discoveryOpts{gateway: testModelsHost})
	return &remoteFixture{fixture: f, anonCS: anonCS, anonDyn: anonDyn}
}

// assertNothingAnonymous: no call reached the target without the caller.
func (rf *remoteFixture) assertNothingAnonymous(t *testing.T) {
	t.Helper()
	assert.Empty(t, rf.anonCS.Actions(), "typed calls made without the caller's token")
	assert.Empty(t, rf.anonDyn.Actions(), "dynamic calls made without the caller's token")
}

func (rf *remoteFixture) created(resource string) []runtime.Object {
	var out []runtime.Object
	for _, a := range rf.cs.Actions() {
		if c, ok := a.(k8stesting.CreateAction); ok && a.GetResource().Resource == resource {
			out = append(out, c.GetObject())
		}
	}
	return out
}

func callerContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(identity.ContextWithToken(context.Background(), "caller-token"), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// TestRemoteTargetActsAsTheCaller: with a workload cluster as the target,
// every read and write — the discovery ConfigMap, presets, nodes, the cache
// claim, the pod-mode inventory scan, the download Job, the cache index, the
// LLMInferenceService — lands on the target with the caller's token, and the
// agents' endpoint is the target's models host with the token forwarded.
func TestRemoteTargetActsAsTheCaller(t *testing.T) {
	rf := newRemoteFixture(t)
	ctx := callerContext(t)

	info := rf.b.Info(ctx)
	assert.True(t, info.Healthy, info.Message)
	assert.Empty(t, info.Message)
	assert.Equal(t, &backend.Target{Cluster: "gpu01", Organization: "giantswarm"}, info.Target, "cluster and organization only")
	assert.Equal(t, testModelsHost, info.AgentEndpoint, "the descriptor names the target's models host")
	assert.Equal(t, testModelsHost, rf.b.cfg.settings(ctx).GatewayEndpoint, "read from the target's discovery ConfigMap")

	presets, err := rf.b.ListPresets(ctx)
	require.NoError(t, err)
	assert.NotEmpty(t, presets, "the presets are the target's")
	nodes, err := rf.b.ListNodes(ctx)
	require.NoError(t, err)
	var names []string
	for _, n := range nodes {
		names = append(names, n.Name)
	}
	assert.Contains(t, names, testGPUNode, "the nodes are the target's")

	// The pod-mode inventory: the real scan, a Job on the target read back
	// through the target apiserver.
	rf.b.scan = rf.b.scanNode
	rf.b.inv.invalidate()
	rf.completePods(ctx)
	scanOut := lineDir + "\ttiny\t100\t3\t0\t1\n" + lineEnd + "\n"
	rf.b.logs = func(context.Context, string, string, corev1.PodLogOptions) (string, error) { return scanOut, nil }
	_, err = rf.b.ListModels(ctx)
	require.NoError(t, err)
	var scanJobs int
	for _, o := range rf.created("jobs") {
		if j := o.(*batchv1.Job); len(j.Name) > len(scanPrefix) && j.Name[:len(scanPrefix)] == scanPrefix {
			scanJobs++
		}
	}
	assert.Positive(t, scanJobs, "the inventory scan pod runs on the target")

	// The download Job and the cache index.
	rf.b.scan = func(context.Context, string) ([]cacheEntry, string, error) { return nil, testCacheNode, nil }
	rf.b.inv.invalidate()
	plan := downloadPlan{Dir: "tiny"}
	rf.completeJob(ctx, plan.jobName(), "INFO start\nPROGRESS 1000\n")
	require.NoError(t, rf.b.Pull(ctx, backend.PullRequest{Ref: tinyRepo}, nil))
	var pulled bool
	for _, o := range rf.created("jobs") {
		pulled = pulled || o.(*batchv1.Job).Name == plan.jobName()
	}
	assert.True(t, pulled, "the download Job runs on the target")

	// The LLMInferenceService, and the endpoint agents on the installation
	// are wired to: the models host, the caller's token forwarded, no
	// placeholder key.
	require.NoError(t, rf.b.Load(ctx, backend.LoadRequest{Name: tinyRepo}))
	assert.NotNil(t, rf.llmisvc(ctx, "tiny"), "the LLMInferenceService is composed on the target")
	_, err = rf.b.ListModels(ctx)
	require.NoError(t, err)
	_, err = rf.cs.CoreV1().ConfigMaps(testServingNS).Get(ctx, DefaultCacheIndexConfigMap, metav1.GetOptions{})
	assert.NoError(t, err, "the cache index is recorded on the target")
	ep := rf.b.AgentEndpoint(tinyRepo)
	assert.Equal(t, testModelsHost+"/"+testServingNS+"/tiny/v1", ep.BaseURL)
	assert.True(t, ep.APIKeyPassthrough)
	assert.False(t, ep.PlaceholderAPIKey)

	rf.assertNothingAnonymous(t)
}

// TestRemoteTargetCancelledPullCleansUpAsTheCaller: the Job of a cancelled
// download is deleted with the caller's token, not anonymously.
func TestRemoteTargetCancelledPullCleansUpAsTheCaller(t *testing.T) {
	rf := newRemoteFixture(t)
	ctx, cancel := context.WithCancel(callerContext(t))
	done := make(chan error, 1)
	go func() { done <- rf.b.Pull(ctx, backend.PullRequest{Ref: tinyRepo}, nil) }()
	plan := downloadPlan{Dir: "tiny"}
	require.Eventually(t, func() bool {
		_, err := rf.cs.BatchV1().Jobs(testServingNS).Get(context.Background(), plan.jobName(), metav1.GetOptions{})
		return err == nil
	}, 2*time.Second, 5*time.Millisecond)
	cancel()
	select {
	case err := <-done:
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(3 * time.Second):
		t.Fatal("pull did not stop on cancel")
	}
	_, err := rf.cs.BatchV1().Jobs(testServingNS).Get(context.Background(), plan.jobName(), metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err), "the cancelled Job is deleted on the target: %v", err)
	rf.assertNothingAnonymous(t)
}

// TestRemoteTargetDetachedWork: a background rescan runs as the caller who
// caused it, past the call's cancellation; without a caller it is skipped
// with the reason, never run anonymously.
func TestRemoteTargetDetachedWork(t *testing.T) {
	rf := newRemoteFixture(t)
	got := rf.b.refreshInventory(context.Background())
	assert.False(t, got.Refreshing)
	assert.Contains(t, got.Reason, "no background cache scan on the remote target gpu01 without a caller")
	rf.assertNothingAnonymous(t)

	type scanCall struct {
		caller    bool
		cancelled bool
	}
	scans := make(chan scanCall, 4)
	rf.b.scan = func(ctx context.Context, node string) ([]cacheEntry, string, error) {
		_, caller := identity.TokenFromContext(ctx)
		scans <- scanCall{caller: caller, cancelled: ctx.Err() != nil}
		return nil, node, nil
	}
	ctx, cancel := context.WithCancel(callerContext(t))
	got = rf.b.refreshInventory(ctx)
	cancel()
	assert.True(t, got.Refreshing, got.Reason)
	select {
	case s := <-scans:
		assert.True(t, s.caller, "the background scan presents the caller's token")
		assert.False(t, s.cancelled, "and outlives the call that caused it")
	case <-time.After(3 * time.Second):
		t.Fatal("no background scan")
	}
	rf.assertNothingAnonymous(t)
}

// TestRemoteTargetRefusalIsNotRetriedAnonymously: a caller the target does
// not accept gets the target's refusal; the credential-less client is never
// tried behind it (it would mask the 401 with an anonymous 403).
func TestRemoteTargetRefusalIsNotRetriedAnonymously(t *testing.T) {
	rf := newRemoteFixture(t)
	cs, dyn := unauthorizedClients()
	clientsFor := func(ctx context.Context) (kubernetes.Interface, dynamic.Interface) {
		if _, ok := identity.TokenFromContext(ctx); ok {
			return cs, dyn
		}
		return rf.anonCS, rf.anonDyn
	}
	rf.b.opts.ClientsFor, rf.b.cfg.opts.ClientsFor = clientsFor, clientsFor
	rf.resetSettings()

	info := rf.b.Info(callerContext(t))
	assert.False(t, info.Healthy)
	assert.Contains(t, info.Message, "token expired")
	assert.NotContains(t, info.Message, "system:anonymous")
	rf.assertNothingAnonymous(t)
}

// TestRemoteTargetWithoutGateway: a target whose discovery document enables
// no models Gateway says so — agents on the installation cannot reach a model
// served on the target's cluster-local address.
func TestRemoteTargetWithoutGateway(t *testing.T) {
	rf := newRemoteFixture(t)
	ctx := callerContext(t)
	rf.setDiscoveryOpts(ctx, discoveryOpts{})
	info := rf.b.Info(ctx)
	assert.Empty(t, info.AgentEndpoint)
	assert.Contains(t, info.Message, "the discovery document on gpu01 enables no models Gateway")
	rf.assertNothingAnonymous(t)
}

func TestNewRefusesDaemonSetInventoryForARemoteTarget(t *testing.T) {
	opts := backend.KServeOptions{
		Clientset:     kubefake.NewSimpleClientset(),
		Dynamic:       dynamicfake.NewSimpleDynamicClient(runtime.NewScheme()),
		InventoryMode: InventoryModeDaemonSet,
		Target:        testTarget,
	}
	_, err := New(opts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "kserve target gpu01: inventory mode daemonset dials the cache agents' pod IPs")

	opts.Target = backend.Target{Cluster: backend.TargetLocal}
	_, err = New(opts)
	assert.NoError(t, err, "the daemonset inventory stays for the local cluster")
}

// testTargetHost is the host of testTarget's apiserver.
const testTargetHost = "api.gpu01.example:6443"

// proxiedRequest is one request the remote target's Service proxy received:
// its path and the token it carried.
type proxiedRequest struct {
	Path  string
	Token string
}

// proxyRoundTrip is the remote target's apiserver answering its Service
// proxy: a request carrying the caller's token to
// /api/v1/namespaces/<ns>/services/http:<service>:<port>/proxy/<path> is
// answered by the runtime behind that Service; any other is refused as the
// apiserver refuses it, and every one when proxyDenied.
func (f *fixture) proxyRoundTrip(req *http.Request) (*http.Response, error) {
	token := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
	f.mu.Lock()
	f.proxied = append(f.proxied, proxiedRequest{Path: req.URL.Path, Token: token})
	denied := f.proxyDenied
	f.mu.Unlock()
	parts := strings.SplitN(strings.TrimPrefix(req.URL.Path, "/api/v1/namespaces/"), "/", 5)
	if len(parts) < 4 || parts[1] != "services" || parts[3] != "proxy" || !strings.HasPrefix(parts[2], "http:") {
		return statusAnswer(http.StatusNotFound, "the server could not find the requested resource"), nil
	}
	if token != "caller-token" || denied {
		return statusAnswer(http.StatusForbidden, fmt.Sprintf(`services "%s" is forbidden: User "caller" cannot get resource "services/proxy" in API group "" in the namespace %q`, strings.Split(parts[2], ":")[1], parts[0])), nil
	}
	svc := strings.Split(strings.TrimPrefix(parts[2], "http:"), ":")
	inner := req.Clone(req.Context())
	inner.URL = &url.URL{Scheme: "http", Host: svc[0] + "." + parts[0] + ".svc.cluster.local:" + svc[1], Path: "/"}
	if len(parts) == 5 {
		inner.URL.Path += parts[4]
	}
	return f.answer(inner)
}

// statusAnswer is an apiserver's metav1.Status answer.
func statusAnswer(code int32, message string) *http.Response {
	rec := httptest.NewRecorder()
	rec.Header().Set("Content-Type", "application/json")
	rec.WriteHeader(int(code))
	_ = json.NewEncoder(rec).Encode(metav1.Status{TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"}, Status: metav1.StatusFailure, Code: code, Message: message})
	return rec.Result()
}

// callerTargetREST is the target's core/v1 REST client as kube.NewForTarget
// builds it for a call: the caller's token when ctx carries one, the
// fixture's transport underneath.
func (f *fixture) callerTargetREST(ctx context.Context) *rest.RESTClient {
	cfg := &rest.Config{Host: testTarget.APIServer, Transport: f}
	if token, ok := identity.TokenFromContext(ctx); ok {
		cfg.BearerToken = token
	}
	cs, err := kubernetes.NewForConfig(cfg)
	require.NoError(f.t, err)
	rc, _ := cs.CoreV1().RESTClient().(*rest.RESTClient)
	return rc
}

// readyTiny loads the tiny preset on the target as the caller and has
// KServe report the object Ready there.
func (rf *remoteFixture) readyTiny(ctx context.Context) {
	rf.t.Helper()
	require.NoError(rf.t, rf.b.Load(ctx, backend.LoadRequest{Name: tinyRepo}))
	rf.setReady(ctx, "tiny", time.Now())
}

// assertNoClusterLocalDial: no runtime request dialled a cluster-local name,
// which resolves only inside the target cluster.
func (f *fixture) assertNoClusterLocalDial(t *testing.T) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, h := range f.dialled {
		assert.NotContains(t, h, ".svc.cluster.local", "a cluster-local name dialled for a remote target")
	}
}

// TestRemoteTargetReadsTheServerThroughTheProxy: on a workload-cluster
// target, the server reads and the first request go through the target
// apiserver's Service proxy as the caller; the model turns Ready with its
// runtime and interfaces, and no cluster-local name is dialled.
func TestRemoteTargetReadsTheServerThroughTheProxy(t *testing.T) {
	rf := newRemoteFixture(t)
	ctx := callerContext(t)
	server := vllmServer(t, devVersion, generateDoc)
	rf.serve("tiny", server)
	rf.readyTiny(ctx)

	loaded, err := rf.b.ListLoaded(ctx)
	require.NoError(t, err)
	require.Len(t, loaded, 1)
	lm := loaded[0]
	assert.Equal(t, statusReady, lm.Status, lm.Message)
	assert.Equal(t, &backend.Runtime{Name: "vllm", Version: devVersion}, lm.Runtime)
	assert.Len(t, lm.Interfaces, 4)
	require.Len(t, server.asked, 1)
	assert.Equal(t, "/v1/chat/completions", server.asked[0].Path)

	proxy := "/api/v1/namespaces/" + testServingNS + "/services/http:tiny-kserve-workload-svc:8000/proxy"
	assert.Equal(t, []proxiedRequest{
		{Path: proxy + "/version", Token: "caller-token"},
		{Path: proxy + "/openapi.json", Token: "caller-token"},
		{Path: proxy + "/v1/chat/completions", Token: "caller-token"},
	}, rf.proxied)
	rf.assertNoClusterLocalDial(t)
	rf.assertNothingAnonymous(t)
}

// TestRemoteTargetProxyRefusal: a caller the target does not allow
// services/proxy is refused there; the serve keeps routing with the target's
// refusal, nothing is dialled around the proxy, and the model is not Ready.
func TestRemoteTargetProxyRefusal(t *testing.T) {
	rf := newRemoteFixture(t)
	ctx := callerContext(t)
	server := vllmServer(t, devVersion, generateDoc)
	rf.serve("tiny", server)
	rf.proxyDenied = true
	rf.readyTiny(ctx)

	loaded, err := rf.b.ListLoaded(ctx)
	require.NoError(t, err)
	require.Len(t, loaded, 1)
	lm := loaded[0]
	assert.Equal(t, statusNotReady, lm.Status)
	assert.Equal(t, backend.PhaseRouting, lm.Phase)
	assert.Contains(t, lm.Message, "the target gpu01 refused the caller 403 Forbidden")
	assert.Contains(t, lm.Message, `cannot get resource "services/proxy"`)
	assert.Zero(t, server.reads, "the runtime is never reached")
	rf.assertNoClusterLocalDial(t)
	rf.assertNothingAnonymous(t)
}

// TestRemoteTargetServerReadWithoutACaller: a list without the caller's
// token reads no server on the target, anonymously or otherwise.
func TestRemoteTargetServerReadWithoutACaller(t *testing.T) {
	rf := newRemoteFixture(t)
	rf.serve("tiny", vllmServer(t, devVersion, generateDoc))
	rf.readyTiny(callerContext(t))
	list := []served{{Name: "tiny", Namespace: testServingNS, Ready: true}}
	rf.b.serverAPIs(context.Background(), list)
	assert.Contains(t, list[0].API.Answer.Waiting, "no model server read on the remote target gpu01 without a caller")
	assert.Empty(t, rf.dialled)
}

// TestLocalTargetDialsTheWorkloadService: the local cluster reads its
// runtimes at the workload Service, never through an apiserver proxy.
func TestLocalTargetDialsTheWorkloadService(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.serve("tiny", vllmServer(t, devVersion, generateDoc))
	f.readyLLMISVC(ctx, "tiny", time.Now())
	lm := loadedOne(t, f.b)
	assert.Equal(t, statusReady, lm.Status, lm.Message)
	host := "tiny-kserve-workload-svc." + testServingNS + ".svc.cluster.local:8000"
	assert.Equal(t, []string{host, host, host}, f.dialled)
	assert.Empty(t, f.proxied)
}
