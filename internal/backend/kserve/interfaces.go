package kserve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
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
	// some 200 KiB).
	serverDocLimit = 8 << 20
)

// generateRoute and poolingRoute head the two families of routes a server
// registers by the model's task: one server runs one runner, so a list that
// names both comes from a server that registers every route (vLLM before
// 0.16).
const (
	generateRoute = "/v1/chat/completions"
	poolingRoute  = "/v1/embeddings"
)

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
	// Answer is what the model said to its first request (answer.go).
	Answer firstAnswer
	// retryAt is set when the read did not reach the server or the server
	// failed; the next list after it reads again.
	retryAt time.Time
}

func (a serverAPI) read() bool { return a.Interfaces != nil }

// apiKey identifies one Ready transition of one object.
func (sv served) apiKey() string {
	return sv.Namespace + "/" + sv.Name + "/" + sv.UID + "/" + sv.ReadyAt.UTC().Format(time.RFC3339Nano)
}

// serverAPIs fills in what each Ready model's server says about itself, and
// what the model said to its first request (answer.go): from
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
	uids := make(map[string]bool, len(list))
	for i := range list {
		sv := &list[i]
		uids[sv.UID] = true
		if sv.Deleting {
			continue
		}
		if !sv.Ready {
			sv.API.Answer = b.standingFailure(sv.UID)
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
	b.forgetAsks(uids)
	if len(toRead) == 0 {
		return
	}

	var wg sync.WaitGroup
	for _, i := range toRead {
		wg.Add(1)
		go func(sv *served) {
			defer wg.Done()
			origin, why := b.serverOrigin(ctx, sv.Name, sv.Namespace)
			if why != "" {
				sv.API = serverAPI{}.retry(why)
				sv.API.Answer = firstAnswer{Waiting: why}
				return
			}
			sv.API = b.readServerAPI(ctx, origin)
			sv.API.Answer = b.askFirst(ctx, *sv, origin, sv.API)
			if sv.API.Answer.Waiting != "" && sv.API.retryAt.IsZero() {
				sv.API.retryAt = time.Now().Add(serverReadRetry)
			}
			if sv.API.Answer.Failure != "" {
				b.log.Info("served model failed its first request", "llminferenceservice", sv.Namespace+"/"+sv.Name, "path", sv.API.Answer.Path, "answer", sv.API.Answer.Failure)
			}
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
// that publishes no list, one that registers every route whatever the model
// serves, or one that could not be read reports none, with the reason —
// nothing is inferred.
func (b *Backend) readServerAPI(ctx context.Context, origin serverOrigin) serverAPI {
	ctx, cancel := context.WithTimeout(ctx, serverReadTimeout)
	defer cancel()
	api := serverAPI{Runtime: &backend.Runtime{Name: runtimeVLLM}, Interfaces: []backend.Interface{}}

	// The version is reported as the server gives it; a build from source
	// answers setuptools-scm's 0.1.dev<n>+g<commit>, so nothing is judged by
	// it.
	var version struct {
		Version string `json:"version"`
	}
	status, err := origin.getJSON(ctx, "/version", &version)
	if unreachable(status, err) {
		return api.retry(fmt.Sprintf("the model server did not answer GET /version (%v); read again after %s", err, serverReadRetry))
	}
	api.Runtime.Version = version.Version

	var doc struct {
		Paths map[string]json.RawMessage `json:"paths"`
	}
	status, err = origin.getJSON(ctx, "/openapi.json", &doc)
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
	_, generate := doc.Paths[generateRoute]
	_, pooling := doc.Paths[poolingRoute]
	if generate && pooling {
		api.Reason = fmt.Sprintf("the model server's route list names both %s and %s: a server that registers every route whatever the model serves (vLLM before 0.16), so the list does not say which interfaces the model answers", generateRoute, poolingRoute)
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

// getJSON GETs path of the server and decodes a 200 answer into out; status
// is the answer's (0 when there was none).
func (o serverOrigin) getJSON(ctx context.Context, path string, out any) (status int, err error) {
	resp, err := o.do(ctx, http.MethodGet, path, nil)
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
