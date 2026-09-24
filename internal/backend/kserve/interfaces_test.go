package kserve

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/giantswarm/model-manager/internal/backend"
)

// modelServer is a served model's runtime as the fixture answers it: the
// version GET /version reports and the document GET /openapi.json returns
// (empty: 404, the runtime's docs are off); failing answers 503 to both.
type modelServer struct {
	version string
	openapi string
	failing bool
	reads   int
}

// vllmServer is a vLLM of the version answering the recorded openapi.json
// document of testdata/openapi.
func vllmServer(t *testing.T, version, doc string) *modelServer {
	t.Helper()
	raw, err := os.ReadFile("testdata/openapi/" + doc)
	require.NoError(t, err)
	return &modelServer{version: version, openapi: string(raw)}
}

// serve puts a runtime behind the workload Service of the named object.
func (f *fixture) serve(name string, s *modelServer) {
	u, err := url.Parse(workloadURL(name, testServingNS))
	require.NoError(f.t, err)
	f.mu.Lock()
	f.servers[u.Host] = s
	f.mu.Unlock()
}

// RoundTrip answers the driver's reads of the served models' runtimes.
func (f *fixture) RoundTrip(req *http.Request) (*http.Response, error) {
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
	switch {
	case s.failing:
		rec.WriteHeader(http.StatusServiceUnavailable)
	case req.URL.Path == "/version":
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
	server := vllmServer(t, "0.23.0", "vllm-generate.json")
	f.serve("tiny", server)
	f.readyLLMISVC(ctx, "tiny", time.Now())

	lm := loadedOne(t, f.b)
	assert.Equal(t, &backend.Runtime{Name: "vllm", Version: "0.23.0"}, lm.Runtime)
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
	f.serve("tiny", vllmServer(t, "0.23.0", "vllm-pooling.json"))
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
	server := vllmServer(t, "0.23.0", "vllm-generate.json")
	f.serve("tiny", server)
	first := time.Now().Add(-time.Hour)
	f.readyLLMISVC(ctx, "tiny", first)

	loadedOne(t, f.b)
	loadedOne(t, f.b)
	_, err := f.b.ListModels(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, server.reads, "one read of /version and /openapi.json for the transition")

	f.setReady(ctx, "tiny", first.Add(30*time.Minute))
	server.version = "0.24.1"
	lm := loadedOne(t, f.b)
	assert.Equal(t, 4, server.reads, "a new Ready transition reads the server again")
	assert.Equal(t, "0.24.1", lm.Runtime.Version)
}

// A model that is not Ready is not read and reports neither field.
func TestNotReadyModelIsNotRead(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	server := vllmServer(t, "0.23.0", "vllm-generate.json")
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

// A vLLM before 0.16 registers every route whatever the model serves: its
// route list is no interface list.
func TestServerBeforeRouteByTaskReportsNoInterfaces(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.serve("tiny", vllmServer(t, "0.15.1", "vllm-generate.json"))
	f.readyLLMISVC(ctx, "tiny", time.Now())

	lm := loadedOne(t, f.b)
	assert.Equal(t, "0.15.1", lm.Runtime.Version)
	assert.Empty(t, lm.Interfaces)
	assert.Contains(t, lm.InterfacesReason, "vLLM 0.15.1 registers every route")
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

	server := vllmServer(t, "0.23.0", "vllm-generate.json")
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

func TestAtLeast(t *testing.T) {
	for v, want := range map[string]bool{
		"0.23.0":               true,
		"0.16.0":               true,
		"0.16rc1":              true,
		"1.0.0":                true,
		"0.15.1":               false,
		"0.11.1rc2.dev45+gabc": false,
		"":                     false,
		"dev":                  false,
	} {
		assert.Equal(t, want, atLeast(v, routeListSince), v)
	}
}

// errStatus wraps the status line of an answer other than 200.
func TestGetServerJSONNamesTheStatus(t *testing.T) {
	f := newFixture(t)
	f.serve("tiny", &modelServer{version: "0.23.0"})
	var out map[string]any
	status, err := f.b.getServerJSON(context.Background(), workloadURL("tiny", testServingNS)+"/openapi.json", &out)
	assert.Equal(t, http.StatusNotFound, status)
	assert.True(t, errors.Is(err, errStatus))
}
