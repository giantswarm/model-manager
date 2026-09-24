package kserve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/giantswarm/model-manager/internal/backend"
)

// The API interfaces a served model answers are read from the running
// server, never declared: once each time an LLMInferenceService turns Ready,
// the driver asks the workload Service for GET /version (the runtime's
// version) and GET /openapi.json (the routes it registered). Since vLLM
// 0.16.0 the OpenAI-compatible server registers a family of routes only when
// the model's supported tasks include it — a generate model gets chat
// completions, completions, Responses and Messages, a pooling model
// embeddings and no chat route — so the route list is the interface list.

const (
	// runtimeVLLM is the runtime the llm-d template starts.
	runtimeVLLM = "vllm"
	// serverReadTimeout bounds both reads of one server together.
	serverReadTimeout = 5 * time.Second
	// serverReadRetry is how long a read that did not reach the server stands
	// before the next list reads again; a read that got an answer stands until
	// the model turns Ready anew.
	serverReadRetry = time.Minute
	// serverDocLimit caps what is read of an answer (vLLM's openapi.json is
	// some 300 KiB).
	serverDocLimit = 8 << 20
)

// routeListSince is the first vLLM release that registers routes by the
// model's task; an older one registers every route whatever the model does.
var routeListSince = [2]int{0, 16}

// interfaceRoutes maps a registered route onto the interface it serves, in
// the order interfaces are reported. The legacy /v1/completions comes with
// chat completions and is not reported on its own.
var interfaceRoutes = []backend.Interface{
	{Type: backend.InterfaceCompletions, Path: "/v1/chat/completions"},
	{Type: backend.InterfaceResponses, Path: "/v1/responses"},
	{Type: backend.InterfaceMessages, Path: "/v1/messages"},
	{Type: backend.InterfaceAnthropicTokenCount, Path: "/v1/messages/count_tokens"},
	{Type: backend.InterfaceEmbeddings, Path: "/v1/embeddings"},
}

// serverAPI is what one read of a Ready model's server found. The zero value
// is "not read": the model is not Ready, or the read has not run.
type serverAPI struct {
	Runtime *backend.Runtime
	// Interfaces is non-nil once read; empty with Reason when the read found
	// none.
	Interfaces []backend.Interface
	Reason     string
	// retryAt is set when the read did not reach the server or the server
	// failed; the next list after it reads again.
	retryAt time.Time
}

func (a serverAPI) read() bool { return a.Interfaces != nil }

// apiKey identifies one Ready transition of one object.
func (sv served) apiKey() string {
	return sv.Namespace + "/" + sv.Name + "/" + sv.UID + "/" + sv.ReadyAt.UTC().Format(time.RFC3339Nano)
}

// serverAPIs fills in what each Ready model's server says about itself: from
// the driver's memory for a transition already read, else by reading the
// servers concurrently. Entries of objects no longer listed are forgotten.
func (b *Backend) serverAPIs(ctx context.Context, list []served) {
	now := time.Now()
	b.apiMu.Lock()
	if b.apiCache == nil {
		b.apiCache = map[string]serverAPI{}
	}
	var toRead []int
	keep := make(map[string]bool, len(list))
	for i := range list {
		sv := &list[i]
		if !sv.Ready || sv.Deleting {
			continue
		}
		k := sv.apiKey()
		keep[k] = true
		if a, ok := b.apiCache[k]; ok && (a.retryAt.IsZero() || now.Before(a.retryAt)) {
			sv.API = a
			continue
		}
		toRead = append(toRead, i)
	}
	for k := range b.apiCache {
		if !keep[k] {
			delete(b.apiCache, k)
		}
	}
	b.apiMu.Unlock()
	if len(toRead) == 0 {
		return
	}

	var wg sync.WaitGroup
	for _, i := range toRead {
		wg.Add(1)
		go func(sv *served) {
			defer wg.Done()
			sv.API = b.readServerAPI(ctx, workloadURL(sv.Name, sv.Namespace))
			if sv.API.Reason != "" {
				b.log.Info("served model reports no API interfaces", "llminferenceservice", sv.Namespace+"/"+sv.Name, "reason", sv.API.Reason)
			}
		}(&list[i])
	}
	wg.Wait()
	b.apiMu.Lock()
	for _, i := range toRead {
		b.apiCache[list[i].apiKey()] = list[i].API
	}
	b.apiMu.Unlock()
}

// readServerAPI reads the runtime version and the route list of the server
// at origin. The interfaces are the known routes the list names; a server
// that publishes no list, one older than the route-by-task release, or one
// that could not be read reports none, with the reason — nothing is inferred.
func (b *Backend) readServerAPI(ctx context.Context, origin string) serverAPI {
	ctx, cancel := context.WithTimeout(ctx, serverReadTimeout)
	defer cancel()
	api := serverAPI{Runtime: &backend.Runtime{Name: runtimeVLLM}, Interfaces: []backend.Interface{}}

	var version struct {
		Version string `json:"version"`
	}
	status, err := b.getServerJSON(ctx, origin+"/version", &version)
	if unreachable(status, err) {
		return api.retry(fmt.Sprintf("the model server did not answer GET /version (%v); read again after %s", err, serverReadRetry))
	}
	if err != nil {
		api.Reason = fmt.Sprintf("the model server reports no version (GET /version: %v), so its route list cannot be told from a vLLM before %d.%d, which registers every route whatever the model serves", err, routeListSince[0], routeListSince[1])
		return api
	}
	api.Runtime.Version = version.Version
	if !atLeast(version.Version, routeListSince) {
		api.Reason = fmt.Sprintf("vLLM %s registers every route whatever the model serves (routes follow the model's task since %d.%d), so its route list does not say which interfaces the model answers", version.Version, routeListSince[0], routeListSince[1])
		return api
	}

	var doc struct {
		Paths map[string]json.RawMessage `json:"paths"`
	}
	status, err = b.getServerJSON(ctx, origin+"/openapi.json", &doc)
	switch {
	case unreachable(status, err):
		return api.retry(fmt.Sprintf("the model server did not answer GET /openapi.json (%v); read again after %s", err, serverReadRetry))
	case status == http.StatusNotFound:
		api.Reason = "the model server publishes no route list (GET /openapi.json answered 404: a runtime started with --disable-fastapi-docs), so the interfaces it answers are unknown"
		return api
	case err != nil:
		api.Reason = fmt.Sprintf("the model server's route list is unreadable (GET /openapi.json: %v)", err)
		return api
	}
	for _, i := range interfaceRoutes {
		if _, ok := doc.Paths[i.Path]; ok {
			api.Interfaces = append(api.Interfaces, i)
		}
	}
	if len(api.Interfaces) == 0 {
		api.Reason = fmt.Sprintf("the model server's route list (%d routes) names none of the API interfaces the platform knows", len(doc.Paths))
	}
	return api
}

// retry marks a read that did not reach the server (or found it failing):
// no interfaces, the reason, and a read again after serverReadRetry.
func (a serverAPI) retry(reason string) serverAPI {
	a.Reason, a.retryAt = reason, time.Now().Add(serverReadRetry)
	return a
}

// errStatus is a server's answer other than 200.
var errStatus = errors.New("unexpected status")

// getServerJSON GETs url and decodes a 200 answer into out; status is the
// answer's (0 when there was none).
func (b *Backend) getServerJSON(ctx context.Context, url string, out any) (status int, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	resp, err := b.serverHTTP.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, serverDocLimit))
		return resp.StatusCode, fmt.Errorf("%w %s", errStatus, resp.Status)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, serverDocLimit)).Decode(out); err != nil {
		return resp.StatusCode, fmt.Errorf("decode: %w", err)
	}
	return resp.StatusCode, nil
}

// unreachable reports a read that got no answer, or a server failing to give
// one: worth reading again, unlike an answer that says what the server is.
func unreachable(status int, err error) bool {
	return err != nil && (status == 0 || status >= 500)
}

// atLeast reports whether a vLLM version string ("0.23.0",
// "0.11.1rc2.dev45+gabc") is at least major.minor.
func atLeast(version string, min [2]int) bool {
	parts := strings.SplitN(version, ".", 3)
	if len(parts) < 2 {
		return false
	}
	major, err1 := strconv.Atoi(parts[0])
	minor, err2 := strconv.Atoi(leadingDigits(parts[1]))
	if err1 != nil || err2 != nil {
		return false
	}
	return major > min[0] || (major == min[0] && minor >= min[1])
}

// leadingDigits is the run of digits s starts with ("16rc1" → "16").
func leadingDigits(s string) string {
	for i, r := range s {
		if r < '0' || r > '9' {
			return s[:i]
		}
	}
	return s
}
