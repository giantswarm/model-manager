package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"

	"github.com/giantswarm/model-manager/internal/backend"
	"github.com/giantswarm/model-manager/internal/jobs"
	"github.com/giantswarm/model-manager/internal/service"
	"github.com/giantswarm/model-manager/internal/wiring"
)

// fakeBackend is an in-memory backend.Backend.
type fakeBackend struct {
	mu     sync.Mutex
	models map[string]backend.Model
	loaded map[string]bool
	// expires is the keep-alive deadline of a loaded model, as Ollama's
	// /api/ps reports it.
	expires map[string]time.Time
	caps    backend.Capabilities
	// pullBlock, when set, holds pulls until closed (for cancel tests).
	pullBlock chan struct{}
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{
		models:  map[string]backend.Model{},
		loaded:  map[string]bool{},
		expires: map[string]time.Time{},
		caps:    backend.Capabilities{Pull: true, PullProgress: true, Delete: true, Load: true, Unload: true, LoadedModels: true},
	}
}

// fakeDriverKeepAlive is the fake driver's own fallback keep-alive; it differs
// from the fixture's configured default on purpose so tests can tell which one
// the API reports.
const fakeDriverKeepAlive = "1m"

func (f *fakeBackend) Name() backend.Name                 { return backend.NameOllama }
func (f *fakeBackend) Capabilities() backend.Capabilities { return f.caps }
func (f *fakeBackend) Info(context.Context) backend.Info {
	return backend.Info{
		Backend: backend.NameOllama, Version: "fake", Endpoint: "http://fake:11434", AgentEndpoint: "http://172.21.0.1:11434", Healthy: true,
		Loading: backend.Loading{OnDemand: true, IdleEviction: true, KeepAliveDefault: fakeDriverKeepAlive, KeepAliveScope: backend.KeepAliveScopeRequest},
	}
}
func (f *fakeBackend) ListModels(context.Context) ([]backend.Model, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]backend.Model, 0, len(f.models))
	for _, m := range f.models {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
func (f *fakeBackend) GetModel(_ context.Context, name string) (*backend.Model, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if m, ok := f.models[name]; ok {
		return &m, nil
	}
	return nil, fmt.Errorf("%w: %s", backend.ErrNotFound, name)
}
func (f *fakeBackend) ListLoaded(context.Context) ([]backend.LoadedModel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []backend.LoadedModel{}
	for name := range f.loaded {
		lm := backend.LoadedModel{Name: name, SizeBytes: f.models[name].SizeBytes, Status: "loaded"}
		if exp, ok := f.expires[name]; ok {
			lm.ExpiresAt = &exp
		}
		out = append(out, lm)
	}
	return out, nil
}
func (f *fakeBackend) Pull(ctx context.Context, req backend.PullRequest, progress func(backend.Progress)) error {
	if strings.HasPrefix(req.Ref, "bad") {
		return fmt.Errorf("%w: %s", backend.ErrNotFound, req.Ref)
	}
	progress(backend.Progress{Status: "pulling", BytesCompleted: 10, BytesTotal: 100})
	if f.pullBlock != nil {
		select {
		case <-f.pullBlock:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	progress(backend.Progress{Status: "success", BytesCompleted: 100, BytesTotal: 100})
	f.mu.Lock()
	f.models[req.Ref] = backend.Model{Name: req.Ref, SizeBytes: 100, Family: "llama"}
	f.mu.Unlock()
	return nil
}
func (f *fakeBackend) Delete(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.models[name]; !ok {
		return fmt.Errorf("%w: %s", backend.ErrNotFound, name)
	}
	delete(f.models, name)
	delete(f.loaded, name)
	delete(f.expires, name)
	return nil
}
func (f *fakeBackend) Load(_ context.Context, req backend.LoadRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loaded[req.Name] = true
	// A parseable keep-alive yields a deadline as Ollama's /api/ps would
	// report it; anything else (kserve paths, -1) leaves none.
	if keepAlive, err := time.ParseDuration(req.KeepAlive); err == nil && keepAlive > 0 {
		f.expires[req.Name] = time.Now().Add(keepAlive).Truncate(time.Second)
	}
	return nil
}
func (f *fakeBackend) Unload(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.loaded, name)
	delete(f.expires, name)
	return nil
}
func (f *fakeBackend) AgentEndpoint(model string) backend.AgentEndpoint {
	return backend.AgentEndpoint{Provider: "Ollama", Host: "http://172.21.0.1:11434", Model: model}
}

// fakeWirer records ModelConfigs in memory: refs holds model-manager's own
// (keyed by model reference), foreign holds ModelConfigs created by others.
type fakeWirer struct {
	mu      sync.Mutex
	refs    map[string]wiring.ModelConfigRef
	foreign []wiring.ModelConfigRef
}

func newFakeWirer() *fakeWirer { return &fakeWirer{refs: map[string]wiring.ModelConfigRef{}} }

func (w *fakeWirer) Ensure(_ context.Context, model string, ep backend.AgentEndpoint) (*wiring.ModelConfigRef, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	name := wiring.ModelConfigName("", model)
	if ep.Name != "" {
		name = ep.Name
	}
	endpoint := ep.BaseURL
	if endpoint == "" {
		endpoint = ep.Host
	}
	ref := wiring.ModelConfigRef{Name: name, Namespace: "kagent", Provider: ep.Provider, Model: model, ProviderModel: ep.Model, Endpoint: endpoint, Ready: true, Managed: true}
	w.refs[model] = ref
	return &ref, nil
}
func (w *fakeWirer) Remove(_ context.Context, model string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.refs, model)
	return nil
}
func (w *fakeWirer) ListAll(context.Context) ([]wiring.ModelConfigRef, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := append([]wiring.ModelConfigRef(nil), w.foreign...)
	for _, r := range w.refs {
		out = append(out, r)
	}
	return out, nil
}
func (w *fakeWirer) Lookup(_ context.Context, model string) (*wiring.ModelConfigRef, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if r, ok := w.refs[model]; ok {
		return &r, nil
	}
	return nil, nil
}
func (w *fakeWirer) List(context.Context) (map[string]wiring.ModelConfigRef, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make(map[string]wiring.ModelConfigRef, len(w.refs))
	for k, v := range w.refs {
		out[k] = v
	}
	return out, nil
}

type fixture struct {
	backend *fakeBackend
	wirer   *fakeWirer
	svc     *service.Service
	srv     *httptest.Server
}

func newFixture(t *testing.T, withWirer bool) *fixture {
	t.Helper()
	fb := newFakeBackend()
	fb.models["qwen3:0.6b"] = backend.Model{Name: "qwen3:0.6b", SizeBytes: 522653767, Family: "qwen3"}
	var w wiring.Wirer
	var fw *fakeWirer
	var info *service.WiringInfo
	if withWirer {
		fw = newFakeWirer()
		w = fw
		info = &service.WiringInfo{Namespace: "kagent", APIVersion: "v1alpha2"}
	}
	svc := service.New(fb, jobs.NewManager(), w, info, service.Config{AutoWire: true, DefaultKeepAlive: "5m"}, nil)
	mux := http.NewServeMux()
	NewREST(svc, nil).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &fixture{backend: fb, wirer: fw, svc: svc, srv: srv}
}

func (f *fixture) do(t *testing.T, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(t, err)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, f.srv.URL+path, rd)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	var out map[string]any
	if resp.ContentLength != 0 {
		_ = json.NewDecoder(resp.Body).Decode(&out)
	}
	return resp.StatusCode, out
}

func (f *fixture) waitJob(t *testing.T, id string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		status, job := f.do(t, http.MethodGet, Prefix+"/jobs/"+id, nil)
		require.Equal(t, http.StatusOK, status)
		switch job["phase"] {
		case string(jobs.PhaseSucceeded), string(jobs.PhaseFailed), string(jobs.PhaseCancelled):
			return job
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("job did not finish")
	return nil
}

func TestBackendEndpoint(t *testing.T) {
	f := newFixture(t, true)
	status, body := f.do(t, http.MethodGet, Prefix+"/backend", nil)
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, "ollama", body["backend"])
	assert.Equal(t, true, body["healthy"])
	assert.Equal(t, "http://fake:11434", body["endpoint"])
	assert.Equal(t, "http://172.21.0.1:11434", body["agentEndpoint"], "the host agents dial is reported next to the one model-manager dials")
	loading := body["loading"].(map[string]any)
	assert.Equal(t, true, loading["onDemand"], "ollama loads a model on the first request naming it")
	assert.Equal(t, true, loading["idleEviction"], "ollama evicts idle models on its own")
	assert.Equal(t, "request", loading["keepAliveScope"], "every request re-arms the keep-alive")
	assert.Equal(t, "5m", loading["keepAliveDefault"], "the configured --default-keep-alive is reported, not the driver's fallback")
	caps := body["capabilities"].(map[string]any)
	assert.Equal(t, true, caps["pull"])
	assert.Equal(t, true, caps["wire"])
	assert.Equal(t, false, caps["presets"])
	wiringInfo := body["wiring"].(map[string]any)
	assert.Equal(t, "kagent", wiringInfo["namespace"])
	assert.Equal(t, true, wiringInfo["autoWire"])

	f2 := newFixture(t, false)
	_, body = f2.do(t, http.MethodGet, Prefix+"/backend", nil)
	assert.Equal(t, false, body["capabilities"].(map[string]any)["wire"], "no Kubernetes access -> wire=false")
	assert.Nil(t, body["wiring"])
}

func TestOpenAPIServed(t *testing.T) {
	f := newFixture(t, false)
	resp, err := http.Get(f.srv.URL + Prefix + "/openapi.yaml")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "yaml")

	// The document must parse: an unquoted `key: value` inside a flow mapping
	// silently breaks every client that reads the served spec.
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	var doc struct {
		Components struct {
			Schemas map[string]struct {
				Required   []string                  `json:"required"`
				Properties map[string]map[string]any `json:"properties"`
			} `json:"schemas"`
		} `json:"components"`
	}
	require.NoError(t, yaml.Unmarshal(body, &doc), "served openapi.yaml is not valid YAML")
	assert.Contains(t, doc.Components.Schemas["Backend"].Required, "loading", "loading is required on Backend")
	assert.Contains(t, doc.Components.Schemas["Loading"].Properties, "keepAliveScope")
	assert.Contains(t, doc.Components.Schemas, "NodeInfo")
}

func TestPullFlowWiresAndEnrichesInventory(t *testing.T) {
	f := newFixture(t, true)
	status, body := f.do(t, http.MethodPost, Prefix+"/models/pull", map[string]any{"model": "smollm2:135m"})
	require.Equal(t, http.StatusAccepted, status, body)
	assert.Equal(t, true, body["created"])
	job := body["job"].(map[string]any)
	assert.Equal(t, "smollm2:135m", job["model"])
	assert.Equal(t, true, job["wire"])
	assert.NotContains(t, job, "node", "ollama has no placement")
	assert.NotContains(t, job, "preset")

	// Re-posting joins the running/finished job or starts a fresh one; either way 202.
	status, again := f.do(t, http.MethodPost, Prefix+"/models/pull", map[string]any{"model": "smollm2:135m"})
	require.Equal(t, http.StatusAccepted, status, again)

	done := f.waitJob(t, job["id"].(string))
	assert.Equal(t, "succeeded", done["phase"])
	assert.EqualValues(t, 100, done["percent"])
	assert.EqualValues(t, 100, done["bytesTotal"])
	result := done["result"].(map[string]any)
	assert.Equal(t, "smollm2-135m", result["name"])
	assert.Equal(t, "Ollama", result["provider"])

	status, list := f.do(t, http.MethodGet, Prefix+"/models", nil)
	require.Equal(t, http.StatusOK, status)
	models := list["models"].([]any)
	var pulled map[string]any
	for _, m := range models {
		mm := m.(map[string]any)
		if mm["name"] == "smollm2:135m" {
			pulled = mm
		}
	}
	require.NotNil(t, pulled)
	assert.Equal(t, false, pulled["loaded"])
	assert.Equal(t, "smollm2-135m", pulled["modelConfig"].(map[string]any)["name"])

	status, one := f.do(t, http.MethodGet, Prefix+"/models/smollm2:135m", nil)
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, "smollm2:135m", one["name"])

	status, jl := f.do(t, http.MethodGet, Prefix+"/jobs", nil)
	require.Equal(t, http.StatusOK, status)
	assert.NotEmpty(t, jl["jobs"])
}

func TestPullWithoutWire(t *testing.T) {
	f := newFixture(t, true)
	status, body := f.do(t, http.MethodPost, Prefix+"/models/pull", map[string]any{"model": "tiny:1m", "wire": false})
	require.Equal(t, http.StatusAccepted, status)
	done := f.waitJob(t, body["job"].(map[string]any)["id"].(string))
	assert.Equal(t, "succeeded", done["phase"])
	assert.Nil(t, done["result"])
	assert.Empty(t, f.wirer.refs)
}

func TestPullFailureAndValidation(t *testing.T) {
	f := newFixture(t, true)
	status, body := f.do(t, http.MethodPost, Prefix+"/models/pull", map[string]any{"model": "bad:1b"})
	require.Equal(t, http.StatusAccepted, status)
	done := f.waitJob(t, body["job"].(map[string]any)["id"].(string))
	assert.Equal(t, "failed", done["phase"])
	assert.Contains(t, done["error"], "not found")

	status, body = f.do(t, http.MethodPost, Prefix+"/models/pull", map[string]any{"model": ""})
	assert.Equal(t, http.StatusBadRequest, status)
	assert.Equal(t, "invalid_request", body["error"].(map[string]any)["code"])

	req, _ := http.NewRequest(http.MethodPost, f.srv.URL+Prefix+"/models/pull", strings.NewReader("{not json"))
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)

	status, _ = f.do(t, http.MethodGet, Prefix+"/jobs/nope", nil)
	assert.Equal(t, http.StatusNotFound, status)
}

func TestCancelJob(t *testing.T) {
	f := newFixture(t, false)
	f.backend.pullBlock = make(chan struct{})
	status, body := f.do(t, http.MethodPost, Prefix+"/models/pull", map[string]any{"model": "slow:1b"})
	require.Equal(t, http.StatusAccepted, status)
	id := body["job"].(map[string]any)["id"].(string)
	status, _ = f.do(t, http.MethodDelete, Prefix+"/jobs/"+id, nil)
	require.Equal(t, http.StatusOK, status)
	done := f.waitJob(t, id)
	assert.Equal(t, "cancelled", done["phase"])
	close(f.backend.pullBlock)
}

func TestLoadUnloadDelete(t *testing.T) {
	f := newFixture(t, true)
	status, body := f.do(t, http.MethodPost, Prefix+"/models/load", map[string]any{"model": "qwen3:0.6b", "keepAlive": "10m"})
	require.Equal(t, http.StatusOK, status, body)
	assert.Equal(t, true, body["loaded"])
	running := body["running"].(map[string]any)
	assert.Equal(t, "qwen3:0.6b", running["name"], "load answers with the loaded entry")
	expiresAt, err := time.Parse(time.RFC3339, running["expiresAt"].(string))
	require.NoError(t, err, "the keep-alive deadline is on the load response, so a client can show 'until …' right away")
	assert.WithinDuration(t, time.Now().Add(10*time.Minute), expiresAt, time.Minute)
	assert.Equal(t, "qwen3-0-6b", body["modelConfig"].(map[string]any)["name"], "load auto-wires")

	status, body = f.do(t, http.MethodGet, Prefix+"/loaded", nil)
	require.Equal(t, http.StatusOK, status)
	assert.Len(t, body["loaded"], 1)

	status, body = f.do(t, http.MethodPost, Prefix+"/models/unload", map[string]any{"model": "qwen3:0.6b"})
	require.Equal(t, http.StatusOK, status, body)
	assert.Equal(t, false, body["loaded"])
	_, body = f.do(t, http.MethodGet, Prefix+"/loaded", nil)
	assert.Empty(t, body["loaded"])

	status, _ = f.do(t, http.MethodPost, Prefix+"/models/load", map[string]any{"model": "missing:1b"})
	assert.Equal(t, http.StatusNotFound, status)

	status, body = f.do(t, http.MethodDelete, Prefix+"/models/qwen3:0.6b", nil)
	require.Equal(t, http.StatusOK, status, body)
	assert.Equal(t, true, body["deleted"])
	assert.Equal(t, true, body["unwired"])
	assert.Empty(t, f.wirer.refs, "delete unwires by default")
	status, _ = f.do(t, http.MethodGet, Prefix+"/models/qwen3:0.6b", nil)
	assert.Equal(t, http.StatusNotFound, status)
}

func TestWireUnwireAndDisabled(t *testing.T) {
	f := newFixture(t, true)
	status, body := f.do(t, http.MethodPost, Prefix+"/models/wire", map[string]any{"model": "qwen3:0.6b"})
	require.Equal(t, http.StatusOK, status, body)
	assert.Equal(t, "qwen3-0-6b", body["modelConfig"].(map[string]any)["name"])
	status, _ = f.do(t, http.MethodPost, Prefix+"/models/wire", map[string]any{"model": "missing:1b"})
	assert.Equal(t, http.StatusNotFound, status)
	status, body = f.do(t, http.MethodPost, Prefix+"/models/unwire", map[string]any{"model": "qwen3:0.6b"})
	require.Equal(t, http.StatusOK, status, body)
	assert.Nil(t, body["modelConfig"])
	assert.Empty(t, f.wirer.refs)

	nf := newFixture(t, false)
	status, body = nf.do(t, http.MethodPost, Prefix+"/models/wire", map[string]any{"model": "qwen3:0.6b"})
	assert.Equal(t, http.StatusNotImplemented, status)
	assert.Equal(t, "unsupported", body["error"].(map[string]any)["code"])
	status, _ = nf.do(t, http.MethodPost, Prefix+"/models/pull", map[string]any{"model": "x:1", "wire": true})
	assert.Equal(t, http.StatusNotImplemented, status, "explicit wire=true without Kubernetes access is refused")
	status, body = nf.do(t, http.MethodPost, Prefix+"/models/pull", map[string]any{"model": "x:1"})
	assert.Equal(t, http.StatusAccepted, status)
	assert.Equal(t, false, body["job"].(map[string]any)["wire"], "autoWire silently degrades without Kubernetes access")
}

func TestUnsupportedCapability(t *testing.T) {
	f := newFixture(t, false)
	f.backend.caps.Load = false
	f.backend.caps.LoadedModels = false
	status, body := f.do(t, http.MethodPost, Prefix+"/models/load", map[string]any{"model": "qwen3:0.6b"})
	assert.Equal(t, http.StatusNotImplemented, status)
	assert.Equal(t, "unsupported", body["error"].(map[string]any)["code"])
	status, _ = f.do(t, http.MethodGet, Prefix+"/loaded", nil)
	assert.Equal(t, http.StatusNotImplemented, status)
}
