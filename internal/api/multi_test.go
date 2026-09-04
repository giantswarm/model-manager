package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/model-manager/internal/backend"
	"github.com/giantswarm/model-manager/internal/jobs"
	"github.com/giantswarm/model-manager/internal/service"
)

// failingBackend is a fakeBackend whose reads fail (its host server is down).
type failingBackend struct {
	*fakeBackend
}

func (f *failingBackend) ListModels(context.Context) ([]backend.Model, error) {
	return nil, fmt.Errorf("dial tcp 172.21.0.1:13305: connection refused")
}
func (f *failingBackend) ListLoaded(context.Context) ([]backend.LoadedModel, error) {
	return nil, fmt.Errorf("dial tcp 172.21.0.1:13305: connection refused")
}
func (f *failingBackend) GetModel(context.Context, string) (*backend.Model, error) {
	return nil, fmt.Errorf("dial tcp 172.21.0.1:13305: connection refused")
}
func (f *failingBackend) Info(context.Context) backend.Info {
	return backend.Info{Backend: f.Name(), Endpoint: "http://fake:13305", Healthy: false, Message: "connection refused"}
}

// multiFixture is one service over an ollama-shaped and a lemonade-shaped
// fake: "shared:1b" exists on both, "qwen3:0.6b" on ollama only,
// "qwen3-it-4b-FLM" on lemonade only.
type multiFixture struct {
	ollama, lemonade *fakeBackend
	wirer            *fakeWirer
	svc              *service.Service
	srv              *httptest.Server
}

func newMultiFixture(t *testing.T, backends ...backend.Backend) *multiFixture {
	t.Helper()
	ollama := newFakeBackend()
	ollama.models["qwen3:0.6b"] = backend.Model{Name: "qwen3:0.6b", SizeBytes: 522653767, Family: "qwen3"}
	ollama.models["shared:1b"] = backend.Model{Name: "shared:1b", SizeBytes: 1}
	lemonade := newFakeBackend()
	lemonade.name = backend.NameLemonade
	lemonade.models["qwen3-it-4b-FLM"] = backend.Model{Name: "qwen3-it-4b-FLM", SizeBytes: 3100000000, Runtime: "flm"}
	lemonade.models["shared:1b"] = backend.Model{Name: "shared:1b", SizeBytes: 2}
	if len(backends) == 0 {
		backends = []backend.Backend{ollama, lemonade}
	}
	fw := newFakeWirer()
	svc := service.New(backends, jobs.NewManager(), fw, &service.WiringInfo{Namespace: "kagent", APIVersion: "v1alpha2"}, service.Config{AutoWire: true, DefaultKeepAlive: "5m"}, nil)
	mux := http.NewServeMux()
	NewREST(svc, nil).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &multiFixture{ollama: ollama, lemonade: lemonade, wirer: fw, svc: svc, srv: srv}
}

func (f *multiFixture) do(t *testing.T, method, path string, body any) (int, map[string]any) {
	t.Helper()
	fx := &fixture{srv: f.srv}
	return fx.do(t, method, path, body)
}

func (f *multiFixture) waitJob(t *testing.T, id string) map[string]any {
	t.Helper()
	fx := &fixture{srv: f.srv}
	return fx.waitJob(t, id)
}

func modelsByBackend(list map[string]any) map[string][]string {
	out := map[string][]string{}
	for _, m := range list["models"].([]any) {
		mm := m.(map[string]any)
		out[mm["backend"].(string)] = append(out[mm["backend"].(string)], mm["name"].(string))
	}
	return out
}

func TestMultiBackendDescriptors(t *testing.T) {
	f := newMultiFixture(t)

	status, body := f.do(t, http.MethodGet, Prefix+"/backends", nil)
	require.Equal(t, http.StatusOK, status)
	list := body["backends"].([]any)
	require.Len(t, list, 2)
	assert.Equal(t, "ollama", list[0].(map[string]any)["backend"], "configured order")
	assert.Equal(t, "lemonade", list[1].(map[string]any)["backend"])
	for _, item := range list {
		d := item.(map[string]any)
		assert.Equal(t, true, d["capabilities"].(map[string]any)["wire"], "each descriptor carries its own effective flags")
		assert.NotContains(t, d, "backends", "the list entries do not repeat the list")
	}

	// The compat route describes the default backend and names both.
	status, body = f.do(t, http.MethodGet, Prefix+"/backend", nil)
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, "ollama", body["backend"])
	assert.Equal(t, []any{"ollama", "lemonade"}, body["backends"])
	status, body = f.do(t, http.MethodGet, Prefix+"/backend?backend=lemonade", nil)
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, "lemonade", body["backend"])
	status, body = f.do(t, http.MethodGet, Prefix+"/backend?backend=kserve", nil)
	assert.Equal(t, http.StatusBadRequest, status)
	assert.Equal(t, "invalid_request", body["error"].(map[string]any)["code"])
	assert.Contains(t, body["error"].(map[string]any)["message"], "configured: ollama, lemonade")
}

func TestMultiBackendAggregateReads(t *testing.T) {
	f := newMultiFixture(t)

	status, body := f.do(t, http.MethodGet, Prefix+"/models", nil)
	require.Equal(t, http.StatusOK, status)
	byBackend := modelsByBackend(body)
	assert.ElementsMatch(t, []string{"qwen3:0.6b", "shared:1b"}, byBackend["ollama"])
	assert.ElementsMatch(t, []string{"qwen3-it-4b-FLM", "shared:1b"}, byBackend["lemonade"])
	assert.NotContains(t, body, "errors")

	status, body = f.do(t, http.MethodGet, Prefix+"/models?backend=lemonade", nil)
	require.Equal(t, http.StatusOK, status)
	byBackend = modelsByBackend(body)
	assert.Empty(t, byBackend["ollama"], "the filter narrows to one backend")
	assert.Len(t, byBackend["lemonade"], 2)

	status, body = f.do(t, http.MethodGet, Prefix+"/models?backend=nope", nil)
	assert.Equal(t, http.StatusBadRequest, status)
	assert.Equal(t, "invalid_request", body["error"].(map[string]any)["code"])

	// Loaded models aggregate the same way, each entry naming its backend.
	status, _ = f.do(t, http.MethodPost, Prefix+"/models/load", map[string]any{"model": "qwen3-it-4b-FLM", "keepAlive": "-1"})
	require.Equal(t, http.StatusOK, status)
	status, body = f.do(t, http.MethodGet, Prefix+"/loaded", nil)
	require.Equal(t, http.StatusOK, status)
	loaded := body["loaded"].([]any)
	require.Len(t, loaded, 1)
	assert.Equal(t, "lemonade", loaded[0].(map[string]any)["backend"])
	status, body = f.do(t, http.MethodGet, Prefix+"/loaded?backend=ollama", nil)
	require.Equal(t, http.StatusOK, status)
	assert.Empty(t, body["loaded"])
}

func TestMultiBackendOneBackendDownDoesNotBlankTheOther(t *testing.T) {
	ollama := newFakeBackend()
	ollama.models["qwen3:0.6b"] = backend.Model{Name: "qwen3:0.6b", SizeBytes: 1}
	down := &failingBackend{fakeBackend: newFakeBackend()}
	down.name = backend.NameLemonade
	f := newMultiFixture(t, ollama, down)

	status, body := f.do(t, http.MethodGet, Prefix+"/models", nil)
	require.Equal(t, http.StatusOK, status, "the readable backend's models are returned")
	assert.Len(t, body["models"], 1)
	errs := body["errors"].(map[string]any)
	assert.Contains(t, errs["lemonade"], "connection refused")
	assert.NotContains(t, errs, "ollama")

	// Filtered to the failing backend the read fails as it always did.
	status, body = f.do(t, http.MethodGet, Prefix+"/models?backend=lemonade", nil)
	assert.Equal(t, http.StatusBadGateway, status)
	assert.Equal(t, "backend_error", body["error"].(map[string]any)["code"])

	// Both down: the aggregate fails.
	both := newMultiFixture(t, &failingBackend{fakeBackend: newFakeBackend()}, down)
	status, body = both.do(t, http.MethodGet, Prefix+"/models", nil)
	assert.Equal(t, http.StatusBadGateway, status)
	assert.Contains(t, body["error"].(map[string]any)["message"], "every backend failed")

	// An unqualified reference that the healthy backend does not have and the
	// down one cannot answer for is the down backend's error, not a 404.
	status, body = f.do(t, http.MethodGet, Prefix+"/models/qwen3-it-4b-FLM", nil)
	assert.Equal(t, http.StatusBadGateway, status, body)
	// One that the healthy backend has resolves regardless.
	status, body = f.do(t, http.MethodGet, Prefix+"/models/qwen3:0.6b", nil)
	require.Equal(t, http.StatusOK, status, body)
	assert.Equal(t, "ollama", body["backend"])
}

func TestMultiBackendResolution(t *testing.T) {
	f := newMultiFixture(t)

	// Unique on one backend: routed there without naming it.
	status, body := f.do(t, http.MethodGet, Prefix+"/models/qwen3-it-4b-FLM", nil)
	require.Equal(t, http.StatusOK, status, body)
	assert.Equal(t, "lemonade", body["backend"])
	status, body = f.do(t, http.MethodPost, Prefix+"/models/load", map[string]any{"model": "qwen3:0.6b"})
	require.Equal(t, http.StatusOK, status, body)
	assert.Equal(t, "ollama", body["backend"])
	assert.Equal(t, "ollama", body["running"].(map[string]any)["backend"])
	assert.Equal(t, "ollama", body["modelConfig"].(map[string]any)["backend"], "load auto-wires on the resolved backend")

	// On both: the unqualified request is refused, the qualified one routed.
	status, body = f.do(t, http.MethodGet, Prefix+"/models/shared:1b", nil)
	assert.Equal(t, http.StatusConflict, status)
	assert.Equal(t, "conflict", body["error"].(map[string]any)["code"])
	assert.Contains(t, body["error"].(map[string]any)["message"], "exists on ollama, lemonade; name the backend")
	for _, path := range []string{"/models/load", "/models/unload", "/models/wire"} {
		status, _ = f.do(t, http.MethodPost, Prefix+path, map[string]any{"model": "shared:1b"})
		assert.Equal(t, http.StatusConflict, status, path)
	}
	status, _ = f.do(t, http.MethodDelete, Prefix+"/models/shared:1b", nil)
	assert.Equal(t, http.StatusConflict, status)

	status, body = f.do(t, http.MethodGet, Prefix+"/models/shared:1b?backend=lemonade", nil)
	require.Equal(t, http.StatusOK, status, body)
	assert.Equal(t, "lemonade", body["backend"])
	assert.EqualValues(t, 2, body["sizeBytes"])
	status, body = f.do(t, http.MethodPost, Prefix+"/models/wire", map[string]any{"model": "shared:1b", "backend": "ollama"})
	require.Equal(t, http.StatusOK, status, body)
	assert.Equal(t, "ollama", body["backend"])
	status, body = f.do(t, http.MethodPost, Prefix+"/models/wire", map[string]any{"model": "shared:1b", "backend": "lemonade"})
	require.Equal(t, http.StatusOK, status, body)
	_, ok := f.wirer.get(backend.NameOllama, "shared:1b")
	assert.True(t, ok)
	_, ok = f.wirer.get(backend.NameLemonade, "shared:1b")
	assert.True(t, ok, "one ModelConfig per (backend, model)")

	// Unwire without a backend consults the wired ModelConfigs: two owners is
	// a conflict, one owner is unambiguous.
	status, body = f.do(t, http.MethodPost, Prefix+"/models/unwire", map[string]any{"model": "shared:1b"})
	assert.Equal(t, http.StatusConflict, status, body)
	status, body = f.do(t, http.MethodPost, Prefix+"/models/unwire", map[string]any{"model": "shared:1b", "backend": "ollama"})
	require.Equal(t, http.StatusOK, status, body)
	assert.Equal(t, "ollama", body["backend"])
	status, body = f.do(t, http.MethodPost, Prefix+"/models/unwire", map[string]any{"model": "shared:1b"})
	require.Equal(t, http.StatusOK, status, body)
	assert.Equal(t, "lemonade", body["backend"], "the remaining owner is found through its ModelConfig")
	_, ok = f.wirer.get(backend.NameLemonade, "shared:1b")
	assert.False(t, ok)

	// Delete qualified removes from that backend only.
	status, body = f.do(t, http.MethodDelete, Prefix+"/models/shared:1b?backend=lemonade", nil)
	require.Equal(t, http.StatusOK, status, body)
	assert.Equal(t, "lemonade", body["backend"])
	status, body = f.do(t, http.MethodGet, Prefix+"/models/shared:1b", nil)
	require.Equal(t, http.StatusOK, status, "unique again: no backend needed")
	assert.Equal(t, "ollama", body["backend"])

	// Nowhere: 404 naming the backends asked.
	status, body = f.do(t, http.MethodGet, Prefix+"/models/nope:1b", nil)
	assert.Equal(t, http.StatusNotFound, status)
	assert.Contains(t, body["error"].(map[string]any)["message"], "none of the backends (ollama, lemonade)")
	// An unknown backend on a mutation is invalid, whatever the model.
	status, _ = f.do(t, http.MethodPost, Prefix+"/models/load", map[string]any{"model": "qwen3:0.6b", "backend": "kserve"})
	assert.Equal(t, http.StatusBadRequest, status)
}

func TestMultiBackendPullGoesToTheNamedOrDefaultBackend(t *testing.T) {
	f := newMultiFixture(t)

	// Unqualified: the default (first configured) backend.
	status, body := f.do(t, http.MethodPost, Prefix+"/models/pull", map[string]any{"model": "new:1b"})
	require.Equal(t, http.StatusAccepted, status, body)
	job := body["job"].(map[string]any)
	assert.Equal(t, "ollama", job["backend"])
	done := f.waitJob(t, job["id"].(string))
	assert.Equal(t, "succeeded", done["phase"])
	assert.Equal(t, "ollama", done["result"].(map[string]any)["backend"], "wired on the backend it was pulled on")
	_, ok := f.ollama.GetModel(context.Background(), "new:1b")
	assert.NoError(t, ok)

	// Named: that backend, as its own job even for the same reference.
	status, body = f.do(t, http.MethodPost, Prefix+"/models/pull", map[string]any{"model": "new:1b", "backend": "lemonade"})
	require.Equal(t, http.StatusAccepted, status, body)
	other := body["job"].(map[string]any)
	assert.Equal(t, "lemonade", other["backend"])
	assert.NotEqual(t, job["id"], other["id"], "the same reference on another backend is another job")
	done = f.waitJob(t, other["id"].(string))
	assert.Equal(t, "succeeded", done["phase"])
	_, err := f.lemonade.GetModel(context.Background(), "new:1b")
	assert.NoError(t, err)

	// Jobs list all, or per backend.
	status, body = f.do(t, http.MethodGet, Prefix+"/jobs", nil)
	require.Equal(t, http.StatusOK, status)
	assert.Len(t, body["jobs"], 2)
	status, body = f.do(t, http.MethodGet, Prefix+"/jobs?backend=lemonade", nil)
	require.Equal(t, http.StatusOK, status)
	require.Len(t, body["jobs"], 1)
	assert.Equal(t, other["id"], body["jobs"].([]any)[0].(map[string]any)["id"])
	status, _ = f.do(t, http.MethodGet, Prefix+"/jobs?backend=nope", nil)
	assert.Equal(t, http.StatusBadRequest, status)

	// A backend without pull refuses, whether named or the default.
	f.lemonade.caps.Pull = false
	status, body = f.do(t, http.MethodPost, Prefix+"/models/pull", map[string]any{"model": "x:1", "backend": "lemonade"})
	assert.Equal(t, http.StatusNotImplemented, status)
	assert.Contains(t, body["error"].(map[string]any)["message"], "pull on lemonade")
}

func TestMultiBackendServingReadsAggregateAcrossCapableBackends(t *testing.T) {
	// ollama + kserve: presets, search and nodes come from the backends that
	// offer them; the fit check needs no backend while only one offers it.
	ollama := newFakeBackend()
	ollama.models["qwen3:0.6b"] = backend.Model{Name: "qwen3:0.6b", SizeBytes: 1}
	kserve := newFakeServing()
	kserve.models["org/tiny"] = backend.Model{Name: "org/tiny", SizeBytes: 10, Preset: "tiny", Path: "tiny", Node: "n1"}
	f := newMultiFixture(t, ollama, kserve)

	status, body := f.do(t, http.MethodGet, Prefix+"/presets", nil)
	require.Equal(t, http.StatusOK, status, body)
	presets := body["presets"].([]any)
	require.Len(t, presets, 1)
	assert.Equal(t, "kserve", presets[0].(map[string]any)["backend"])
	assert.NotContains(t, body, "errors", "a backend without the capability is not a failure")

	status, body = f.do(t, http.MethodGet, Prefix+"/nodes", nil)
	require.Equal(t, http.StatusOK, status, body)
	nodes := body["nodes"].([]any)
	require.Len(t, nodes, 2)
	assert.Equal(t, "kserve", nodes[0].(map[string]any)["backend"])
	status, body = f.do(t, http.MethodGet, Prefix+"/nodes?backend=ollama", nil)
	assert.Equal(t, http.StatusNotImplemented, status, "named backend without the capability")
	assert.Equal(t, "unsupported", body["error"].(map[string]any)["code"])

	status, body = f.do(t, http.MethodPost, Prefix+"/models/fit-check", map[string]any{"model": "org/tiny"})
	require.Equal(t, http.StatusOK, status, body)
	assert.Equal(t, "kserve", body["backend"])
	status, _ = f.do(t, http.MethodPost, Prefix+"/models/fit-check", map[string]any{"model": "org/tiny", "backend": "ollama"})
	assert.Equal(t, http.StatusNotImplemented, status)

	// Two backends offering fit checks need the backend named.
	two := newMultiFixture(t, newFakeServing(), func() backend.Backend {
		k := newFakeServing()
		k.name = backend.NameLemonade
		return k
	}())
	status, body = two.do(t, http.MethodPost, Prefix+"/models/fit-check", map[string]any{"model": "org/tiny"})
	assert.Equal(t, http.StatusConflict, status, body)
	assert.Contains(t, body["error"].(map[string]any)["message"], "name the backend")

	// Search aggregates the hubs of the capable backends.
	status, body = f.do(t, http.MethodGet, Prefix+"/search?q=tiny", nil)
	require.Equal(t, http.StatusOK, status)
	assert.Len(t, body["results"], 2)

	// The compat descriptor is the default backend's, so an older portal on
	// this installation renders ollama's controls — the list names kserve.
	status, body = f.do(t, http.MethodGet, Prefix+"/backend", nil)
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, "ollama", body["backend"])
	assert.Equal(t, false, body["capabilities"].(map[string]any)["presets"])
	assert.Equal(t, []any{"ollama", "kserve"}, body["backends"])
}

func TestMultiBackendMCPTools(t *testing.T) {
	f := newMultiFixture(t)
	srv := NewMCPServer(f.svc, "test")

	out, isErr := callTool(t, srv, ToolListBackends, nil)
	require.False(t, isErr, out)
	assert.Contains(t, out, `"backend": "ollama"`)
	assert.Contains(t, out, `"backend": "lemonade"`)

	out, isErr = callTool(t, srv, ToolGetBackend, map[string]any{argBackend: "lemonade"})
	require.False(t, isErr, out)
	assert.True(t, strings.Contains(out, `"backend": "lemonade"`), out)

	out, isErr = callTool(t, srv, ToolGetModel, map[string]any{argModel: "shared:1b"})
	assert.True(t, isErr)
	assert.Contains(t, out, "conflict")
	out, isErr = callTool(t, srv, ToolGetModel, map[string]any{argModel: "shared:1b", argBackend: "lemonade"})
	require.False(t, isErr, out)
	assert.Contains(t, out, `"backend": "lemonade"`)

	out, isErr = callTool(t, srv, ToolPullModel, map[string]any{argModel: "new:1b", argBackend: "lemonade"})
	require.False(t, isErr, out)
	assert.Contains(t, out, `"backend": "lemonade"`)

	out, isErr = callTool(t, srv, ToolListModels, map[string]any{argBackend: "ollama"})
	require.False(t, isErr, out)
	assert.NotContains(t, out, "qwen3-it-4b-FLM")
	out, isErr = callTool(t, srv, ToolListModels, map[string]any{argBackend: "nope"})
	assert.True(t, isErr)
	assert.Contains(t, out, "invalid_request")

	out, isErr = callTool(t, srv, ToolDeleteModel, map[string]any{argModel: "qwen3-it-4b-FLM"})
	require.False(t, isErr, out)
	assert.Contains(t, out, `"backend": "lemonade"`)
}
