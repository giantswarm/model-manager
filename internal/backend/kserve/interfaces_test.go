package kserve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/giantswarm/model-manager/internal/backend"
)

// modelServer is a served model's runtime as the fixture answers it: the
// version GET /version reports and the document GET /openapi.json returns
// (either empty: 404 — the runtime's docs off, a server without the route);
// failing answers 503 to both. The first request (answer.go) is answered 200,
// or answerStatus with answerBody; silent drops it unanswered. asked records
// each request's path and body.
type modelServer struct {
	version      string
	openapi      string
	failing      bool
	answerStatus int
	answerBody   string
	silent       bool
	reads        int
	asked        []askedRequest
}

// askedRequest is one first request the fixture's runtime received.
type askedRequest struct {
	Path string
	Body map[string]any
}

// The openapi.json documents of testdata/openapi: generateDoc recorded from
// the llm-d CPU runtime (llm-d-cpu:v0.8.0, its vLLM a source build reporting
// devVersion) serving Qwen/Qwen2.5-0.5B-Instruct in agentlab, poolingDoc from
// the same runtime serving BAAI/bge-small-en-v1.5; both trimmed to their
// paths (the components' schemas dropped).
const (
	generateDoc = "llm-d-cpu-v0.8.0-generate.json"
	poolingDoc  = "llm-d-cpu-v0.8.0-pooling.json"
	devVersion  = "0.1.dev1+g51f799c1a"
)

// openapiDoc reads an openapi.json document of testdata/openapi.
func openapiDoc(t *testing.T, doc string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "openapi", doc)) //nolint:gosec // a recorded document under testdata, named by the test
	require.NoError(t, err)
	return raw
}

// vllmServer is a vLLM of the version answering the openapi.json document of
// testdata/openapi.
func vllmServer(t *testing.T, version, doc string) *modelServer {
	t.Helper()
	return &modelServer{version: version, openapi: string(openapiDoc(t, doc))}
}

// allRoutesServer answers the union of the generate and the pooling route
// lists: a server that registers every route whatever the model serves.
func allRoutesServer(t *testing.T) *modelServer {
	t.Helper()
	merged := map[string]any{}
	for _, doc := range []string{generateDoc, poolingDoc} {
		var d map[string]any
		require.NoError(t, json.Unmarshal(openapiDoc(t, doc), &d))
		for k, v := range d["paths"].(map[string]any) {
			merged[k] = v
		}
	}
	raw, err := json.Marshal(map[string]any{"openapi": "3.1.0", "paths": merged})
	require.NoError(t, err)
	return &modelServer{version: "0.15.1", openapi: string(raw)}
}

// serve puts a runtime behind the workload Service of the named object.
func (f *fixture) serve(name string, s *modelServer) {
	u, err := url.Parse(workloadURL(name, testServingNS))
	require.NoError(f.t, err)
	f.mu.Lock()
	f.servers[u.Host] = s
	f.mu.Unlock()
}

// RoundTrip answers the driver's reads of the served models' runtimes: at
// their workload Service, or through the Service proxy of the remote target's
// apiserver (proxyRoundTrip). dialled records each host dialled.
func (f *fixture) RoundTrip(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	f.dialled = append(f.dialled, req.URL.Host)
	f.mu.Unlock()
	if req.URL.Host == testTargetHost {
		return f.proxyRoundTrip(req)
	}
	return f.answer(req)
}

// answer is the runtime behind req's workload Service host answering it.
func (f *fixture) answer(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	s := f.servers[req.URL.Host]
	if s != nil {
		s.reads++
	}
	f.mu.Unlock()
	if s == nil {
		return nil, fmt.Errorf("dial tcp %s: i/o timeout", req.URL.Host)
	}
	rec := httptest.NewRecorder()
	if req.Method == http.MethodPost {
		var body map[string]any
		_ = json.NewDecoder(req.Body).Decode(&body)
		f.mu.Lock()
		s.asked = append(s.asked, askedRequest{Path: req.URL.Path, Body: body})
		f.mu.Unlock()
		switch {
		case s.silent:
			return nil, fmt.Errorf("read tcp %s: connection reset by peer", req.URL.Host)
		case s.answerStatus != 0:
			rec.WriteHeader(s.answerStatus)
			_, _ = io.WriteString(rec, s.answerBody)
		default:
			_, _ = io.WriteString(rec, `{"object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"OK"}}]}`)
		}
		return rec.Result(), nil
	}
	switch {
	case s.failing:
		rec.WriteHeader(http.StatusServiceUnavailable)
	case req.URL.Path == "/version" && s.version != "":
		_, _ = fmt.Fprintf(rec, `{"version":%q}`, s.version)
	case req.URL.Path == "/openapi.json" && s.openapi != "":
		_, _ = io.WriteString(rec, s.openapi)
	default:
		rec.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(rec, `{"detail":"Not Found"}`)
	}
	return rec.Result(), nil
}

// readyLLMISVC loads the preset and has KServe report the object Ready,
// its Ready condition turned at readyAt.
func (f *fixture) readyLLMISVC(ctx context.Context, preset string, readyAt time.Time) {
	f.t.Helper()
	obj := f.pendingLLMISVC(ctx, preset)
	f.setReady(ctx, obj.GetName(), readyAt)
}

// setReady turns the object's Ready condition True at readyAt.
func (f *fixture) setReady(ctx context.Context, name string, readyAt time.Time) {
	f.t.Helper()
	llmisvcs := f.dyn.Resource(llmisvcGVR).Namespace(testServingNS)
	obj, err := llmisvcs.Get(ctx, name, metav1.GetOptions{})
	require.NoError(f.t, err)
	obj.Object["status"] = map[string]any{"conditions": []any{
		map[string]any{"type": "Ready", "status": "True", "lastTransitionTime": readyAt.UTC().Format(time.RFC3339)},
	}}
	_, err = llmisvcs.Update(ctx, obj, metav1.UpdateOptions{})
	require.NoError(f.t, err)
}

func loadedOne(t *testing.T, b *Backend) backend.LoadedModel {
	t.Helper()
	loaded, err := b.ListLoaded(context.Background())
	require.NoError(t, err)
	require.Len(t, loaded, 1)
	return loaded[0]
}

// A generate model's server registered chat completions, completions,
// Responses and Messages with count_tokens: those are its interfaces, in
// agentgateway's format names, with the runtime's version.
func TestReadyModelReportsTheInterfacesItsServerRegistered(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.serve("tiny", vllmServer(t, devVersion, generateDoc))
	f.readyLLMISVC(ctx, "tiny", time.Now())

	lm := loadedOne(t, f.b)
	assert.Equal(t, &backend.Runtime{Name: "vllm", Version: devVersion}, lm.Runtime, "the version as the server reports it; a source build's says nothing and judges nothing")
	assert.Equal(t, []backend.Interface{
		{Type: backend.InterfaceCompletions, Path: "/v1/chat/completions"},
		{Type: backend.InterfaceResponses, Path: "/v1/responses"},
		{Type: backend.InterfaceMessages, Path: "/v1/messages"},
		{Type: backend.InterfaceAnthropicTokenCount, Path: "/v1/messages/count_tokens"},
	}, lm.Interfaces)
	assert.Empty(t, lm.InterfacesReason)
}

// A pooling model's server registered embeddings and no chat route.
func TestPoolingModelReportsEmbeddingsOnly(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.serve("tiny", vllmServer(t, devVersion, poolingDoc))
	f.readyLLMISVC(ctx, "tiny", time.Now())

	lm := loadedOne(t, f.b)
	assert.Equal(t, []backend.Interface{{Type: backend.InterfaceEmbeddings, Path: "/v1/embeddings"}}, lm.Interfaces)
	assert.Empty(t, lm.InterfacesReason)
}

// The server is read once per Ready transition: later lists answer from the
// driver's memory, a new transition reads again.
func TestServerIsReadOncePerReadyTransition(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	server := vllmServer(t, "0.23.0", generateDoc)
	f.serve("tiny", server)
	first := time.Now().Add(-time.Hour)
	f.readyLLMISVC(ctx, "tiny", first)

	loadedOne(t, f.b)
	loadedOne(t, f.b)
	_, err := f.b.ListModels(ctx)
	require.NoError(t, err)
	assert.Equal(t, 3, server.reads, "one read of /version and /openapi.json and one first request for the transition")

	f.setReady(ctx, "tiny", first.Add(30*time.Minute))
	server.version = "0.24.1"
	lm := loadedOne(t, f.b)
	assert.Equal(t, 6, server.reads, "a new Ready transition reads the server and asks it again")
	assert.Equal(t, "0.24.1", lm.Runtime.Version)
}

// A model that is not Ready is not read and reports neither field.
func TestNotReadyModelIsNotRead(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	server := vllmServer(t, "0.23.0", generateDoc)
	f.serve("tiny", server)
	f.pendingLLMISVC(ctx, "tiny")

	lm := loadedOne(t, f.b)
	assert.Nil(t, lm.Runtime)
	assert.Nil(t, lm.Interfaces)
	assert.Empty(t, lm.InterfacesReason)
	assert.Zero(t, server.reads)
}

// A runtime started with --disable-fastapi-docs publishes no route list:
// no interfaces, the reason, nothing inferred from the version.
func TestServerWithoutRouteListReportsNoInterfaces(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.serve("tiny", &modelServer{version: "0.23.0"})
	f.readyLLMISVC(ctx, "tiny", time.Now())

	lm := loadedOne(t, f.b)
	assert.Equal(t, "0.23.0", lm.Runtime.Version)
	assert.NotNil(t, lm.Interfaces)
	assert.Empty(t, lm.Interfaces)
	assert.Contains(t, lm.InterfacesReason, "--disable-fastapi-docs")
}

// A server that registers every route whatever the model serves (vLLM
// before 0.16) names chat completions and embeddings side by side: its route
// list is no interface list.
func TestServerWithEveryRouteReportsNoInterfaces(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.serve("tiny", allRoutesServer(t))
	f.readyLLMISVC(ctx, "tiny", time.Now())

	lm := loadedOne(t, f.b)
	assert.Equal(t, "0.15.1", lm.Runtime.Version)
	assert.NotNil(t, lm.Interfaces)
	assert.Empty(t, lm.Interfaces)
	assert.Contains(t, lm.InterfacesReason, "registers every route")
}

// A server that answers no /version still reports the interfaces its route
// list names; the version stays empty.
func TestServerWithoutVersionReportsItsInterfaces(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	server := vllmServer(t, "", generateDoc)
	f.serve("tiny", server)
	f.readyLLMISVC(ctx, "tiny", time.Now())

	lm := loadedOne(t, f.b)
	assert.Equal(t, &backend.Runtime{Name: "vllm"}, lm.Runtime)
	assert.Len(t, lm.Interfaces, 4)
}

// A server the driver cannot reach — a network policy that drops the read,
// a runtime failing — reports no interfaces with the reason, and is read
// again after a minute rather than at the next transition.
func TestUnreachableServerIsReadAgainLater(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.readyLLMISVC(ctx, "tiny", time.Now())

	lm := loadedOne(t, f.b)
	assert.Empty(t, lm.Interfaces)
	assert.Contains(t, lm.InterfacesReason, "did not answer GET /version")
	assert.Contains(t, lm.InterfacesReason, "i/o timeout")

	server := vllmServer(t, "0.23.0", generateDoc)
	server.failing = true
	f.serve("tiny", server)
	lm = loadedOne(t, f.b)
	assert.Zero(t, server.reads, "the failed read stands for a minute")

	f.b.apiMu.Lock()
	for k, a := range f.b.apiCache {
		a.retryAt = time.Now().Add(-time.Second)
		f.b.apiCache[k] = a
	}
	f.b.apiMu.Unlock()
	lm = loadedOne(t, f.b)
	assert.Equal(t, 1, server.reads)
	assert.Contains(t, lm.InterfacesReason, "503 Service Unavailable")

	server.failing = false
	f.b.apiMu.Lock()
	for k, a := range f.b.apiCache {
		a.retryAt = time.Now().Add(-time.Second)
		f.b.apiCache[k] = a
	}
	f.b.apiMu.Unlock()
	lm = loadedOne(t, f.b)
	assert.Len(t, lm.Interfaces, 4)
	assert.Empty(t, lm.InterfacesReason)
}

// errStatus wraps the status line of an answer other than 200.
func TestGetServerJSONNamesTheStatus(t *testing.T) {
	f := newFixture(t)
	f.serve("tiny", &modelServer{version: "0.23.0"})
	var out map[string]any
	origin, why := f.b.serverOrigin(context.Background(), "tiny", testServingNS)
	require.Empty(t, why)
	status, err := origin.getJSON(context.Background(), "/openapi.json", &out)
	assert.Equal(t, http.StatusNotFound, status)
	assert.True(t, errors.Is(err, errStatus))
}
