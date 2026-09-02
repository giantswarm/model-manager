package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/model-manager/internal/backend"
	"github.com/giantswarm/model-manager/internal/jobs"
	"github.com/giantswarm/model-manager/internal/service"
	"github.com/giantswarm/model-manager/internal/wiring"
)

// fakeServing is a kserve-shaped backend: presets, search, fit check, node
// inventory, and a serve lifecycle whose endpoint exists only while served.
type fakeServing struct {
	*fakeBackend
	mu      sync.Mutex
	ready   map[string]bool
	presets []backend.Preset
	pulls   []backend.PullRequest
}

func newFakeServing() *fakeServing {
	fb := newFakeBackend()
	fb.caps = backend.Capabilities{Pull: true, PullProgress: true, Delete: true, Load: true, Unload: true, LoadedModels: true, Presets: true, FitCheck: true, NodeInventory: true, Search: true}
	return &fakeServing{
		fakeBackend: fb,
		ready:       map[string]bool{},
		presets:     []backend.Preset{{Name: "tiny", DisplayName: "Tiny", Model: "org/tiny", GPUs: 1, WeightsBytes: 10, OverheadBytes: 20, RequiredBytes: 30}},
	}
}

func (f *fakeServing) Name() backend.Name { return backend.NameKServe }

// GetModel resolves preset names too, as the kserve driver does.
func (f *fakeServing) GetModel(ctx context.Context, name string) (*backend.Model, error) {
	for _, p := range f.presets {
		if p.Name == name {
			name = p.Model
		}
	}
	return f.fakeBackend.GetModel(ctx, name)
}
func (f *fakeServing) Info(context.Context) backend.Info {
	return backend.Info{Backend: backend.NameKServe, Version: "serving.kserve.io/v1beta1", Healthy: true}
}
func (f *fakeServing) ListLoaded(ctx context.Context) ([]backend.LoadedModel, error) {
	loaded, err := f.fakeBackend.ListLoaded(ctx)
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range loaded {
		loaded[i].Status = "Pending"
		if f.ready[loaded[i].Name] {
			loaded[i].Status = "Ready"
		}
		loaded[i].Resource = strings.ReplaceAll(loaded[i].Name, "/", "-")
		loaded[i].Endpoint = "http://" + loaded[i].Resource + "-predictor.model-serving.svc.cluster.local"
	}
	return loaded, err
}
func (f *fakeServing) Pull(ctx context.Context, req backend.PullRequest, progress func(backend.Progress)) error {
	if req.Ref == "org/huge" {
		return fmt.Errorf("%w: 100.0 GiB weights exceed the 64.0 GiB on n1", backend.ErrUnfit)
	}
	f.mu.Lock()
	f.pulls = append(f.pulls, req)
	f.mu.Unlock()
	return f.fakeBackend.Pull(ctx, req, progress)
}
func (f *fakeServing) Load(ctx context.Context, req backend.LoadRequest) error {
	if req.Preset == "" {
		return fmt.Errorf("%w: no serving preset serves %s", backend.ErrInvalid, req.Name)
	}
	return f.fakeBackend.Load(ctx, req)
}
func (f *fakeServing) AgentEndpoint(model string) backend.AgentEndpoint {
	name := strings.ReplaceAll(model, "/", "-")
	return backend.AgentEndpoint{Provider: "OpenAI", BaseURL: "http://" + name + "-predictor.model-serving.svc.cluster.local/v1", Model: name, PlaceholderAPIKey: true, Name: name}
}
func (f *fakeServing) WaitReady(ctx context.Context, model string) error {
	for {
		f.mu.Lock()
		ok := f.ready[model]
		f.mu.Unlock()
		if ok {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Millisecond):
		}
	}
}
func (f *fakeServing) setReady(model string) {
	f.mu.Lock()
	f.ready[model] = true
	f.mu.Unlock()
}
func (f *fakeServing) RunningPulls(context.Context) ([]backend.PullRequest, error) {
	return []backend.PullRequest{{Ref: "org/adopted", Preset: "tiny", Node: "n1"}}, nil
}
func (f *fakeServing) ListPresets(context.Context) ([]backend.Preset, error) { return f.presets, nil }
func (f *fakeServing) Search(_ context.Context, query string, limit int) ([]backend.SearchResult, error) {
	return []backend.SearchResult{{ID: "org/" + query, Downloads: 1, Presets: []string{"tiny"}}, {ID: fmt.Sprintf("limit/%d", limit)}}, nil
}
func (f *fakeServing) FitCheck(_ context.Context, req backend.FitRequest) (*backend.FitResult, error) {
	if req.Model == "nobody/nothing" {
		return nil, fmt.Errorf("%w: %s is not on the hub", backend.ErrNotFound, req.Model)
	}
	fits := req.Model != "org/huge"
	return &backend.FitResult{Model: req.Model, Fits: fits, Preset: req.Preset, Node: "n1", WeightsBytes: 10, OverheadBytes: 20, RequiredBytes: 30, BudgetBytes: 64, FreeBytes: 64, Reason: "because"}, nil
}
func (f *fakeServing) ListNodes(context.Context) ([]backend.NodeInfo, error) {
	return []backend.NodeInfo{{Name: "n1", Ready: true, BudgetBytes: 64, BudgetSource: "allocatable", Cache: &backend.NodeCache{Claim: "hf-cache", MountPath: "/mnt/models", Models: 1}}}, nil
}

type servingFixture struct {
	backend *fakeServing
	wirer   *fakeWirer
	svc     *service.Service
	srv     *httptest.Server
}

func newServingFixture(t *testing.T) *servingFixture {
	t.Helper()
	fb := newFakeServing()
	fb.models["org/tiny"] = backend.Model{Name: "org/tiny", SizeBytes: 10, Preset: "tiny", Path: "tiny", Node: "n1"}
	fw := newFakeWirer()
	svc := service.New(fb, jobs.NewManager(), fw, &service.WiringInfo{Namespace: "kagent", APIVersion: "v1alpha2"}, service.Config{AutoWire: true, DefaultKeepAlive: "5m", ReconcileInterval: 5 * time.Millisecond}, nil)
	mux := http.NewServeMux()
	NewREST(svc, nil).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &servingFixture{backend: fb, wirer: fw, svc: svc, srv: srv}
}

func (f *servingFixture) do(t *testing.T, method, path string, body any) (int, map[string]any) {
	t.Helper()
	fx := &fixture{srv: f.srv}
	return fx.do(t, method, path, body)
}

func (f *servingFixture) waitJob(t *testing.T, id string) map[string]any {
	t.Helper()
	fx := &fixture{srv: f.srv}
	return fx.waitJob(t, id)
}

func TestServingBackendCapabilitiesAndReads(t *testing.T) {
	f := newServingFixture(t)
	status, body := f.do(t, http.MethodGet, Prefix+"/backend", nil)
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, "kserve", body["backend"])
	caps := body["capabilities"].(map[string]any)
	for _, flag := range []string{"presets", "fitCheck", "nodeInventory", "search", "wire"} {
		assert.Equal(t, true, caps[flag], flag)
	}

	status, body = f.do(t, http.MethodGet, Prefix+"/presets", nil)
	require.Equal(t, http.StatusOK, status)
	presets := body["presets"].([]any)
	require.Len(t, presets, 1)
	assert.Equal(t, "tiny", presets[0].(map[string]any)["name"])
	assert.EqualValues(t, 30, presets[0].(map[string]any)["requiredBytes"])

	status, body = f.do(t, http.MethodGet, Prefix+"/search?q=tiny&limit=7", nil)
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, "tiny", body["query"])
	results := body["results"].([]any)
	require.Len(t, results, 2)
	assert.Equal(t, "org/tiny", results[0].(map[string]any)["id"])
	assert.Equal(t, "limit/7", results[1].(map[string]any)["id"], "limit is passed through")
	status, body = f.do(t, http.MethodGet, Prefix+"/search", nil)
	assert.Equal(t, http.StatusBadRequest, status)
	assert.Equal(t, "invalid_request", body["error"].(map[string]any)["code"])
	status, _ = f.do(t, http.MethodGet, Prefix+"/search?q=x&limit=abc", nil)
	assert.Equal(t, http.StatusBadRequest, status)

	status, body = f.do(t, http.MethodPost, Prefix+"/models/fit-check", map[string]any{"model": "org/tiny", "preset": "tiny"})
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, true, body["fits"])
	assert.Equal(t, "n1", body["node"])
	status, body = f.do(t, http.MethodPost, Prefix+"/models/fit-check", map[string]any{"model": "org/huge"})
	require.Equal(t, http.StatusOK, status, "a negative fit check is a 200 with fits=false")
	assert.Equal(t, false, body["fits"])
	status, _ = f.do(t, http.MethodPost, Prefix+"/models/fit-check", map[string]any{"model": "nobody/nothing"})
	assert.Equal(t, http.StatusNotFound, status)
	status, body = f.do(t, http.MethodPost, Prefix+"/models/fit-check", map[string]any{"preset": "tiny"})
	require.Equal(t, http.StatusOK, status, "preset alone is enough")
	assert.Equal(t, "tiny", body["preset"])
	status, _ = f.do(t, http.MethodPost, Prefix+"/models/fit-check", map[string]any{})
	assert.Equal(t, http.StatusBadRequest, status)

	status, body = f.do(t, http.MethodGet, Prefix+"/nodes", nil)
	require.Equal(t, http.StatusOK, status)
	nodes := body["nodes"].([]any)
	require.Len(t, nodes, 1)
	n := nodes[0].(map[string]any)
	assert.Equal(t, "n1", n["name"])
	assert.Equal(t, "hf-cache", n["cache"].(map[string]any)["claim"])
}

func TestServingReadsAreUnsupportedOnOllama(t *testing.T) {
	f := newFixture(t, true)
	for _, path := range []string{Prefix + "/presets", Prefix + "/search?q=x", Prefix + "/nodes"} {
		status, body := f.do(t, http.MethodGet, path, nil)
		assert.Equal(t, http.StatusNotImplemented, status, path)
		assert.Equal(t, "unsupported", body["error"].(map[string]any)["code"], path)
	}
	status, body := f.do(t, http.MethodPost, Prefix+"/models/fit-check", map[string]any{"model": "x"})
	assert.Equal(t, http.StatusNotImplemented, status)
	assert.Equal(t, "unsupported", body["error"].(map[string]any)["code"])
}

func TestServingPullRefusesWireAndUnfit(t *testing.T) {
	f := newServingFixture(t)
	// wire=true makes no sense on a serve-lifecycle backend.
	status, body := f.do(t, http.MethodPost, Prefix+"/models/pull", map[string]any{"model": "org/new", "wire": true})
	assert.Equal(t, http.StatusBadRequest, status)
	assert.Contains(t, body["error"].(map[string]any)["message"], "wired when loaded")

	// Default: no wiring after the pull; preset/node reach the backend.
	status, body = f.do(t, http.MethodPost, Prefix+"/models/pull", map[string]any{"model": "org/new", "preset": "tiny", "node": "n1"})
	require.Equal(t, http.StatusAccepted, status, body)
	job := body["job"].(map[string]any)
	assert.Equal(t, false, job["wire"])
	done := f.waitJob(t, job["id"].(string))
	assert.Equal(t, "succeeded", done["phase"])
	assert.Nil(t, done["result"])
	assert.Empty(t, f.wirer.refs, "kserve models are wired when served, not when pulled")
	f.backend.mu.Lock()
	require.Len(t, f.backend.pulls, 1)
	assert.Equal(t, backend.PullRequest{Ref: "org/new", Preset: "tiny", Node: "n1"}, f.backend.pulls[0])
	f.backend.mu.Unlock()

	// A refused fit surfaces as a failed job with the explanation.
	status, body = f.do(t, http.MethodPost, Prefix+"/models/pull", map[string]any{"model": "org/huge"})
	require.Equal(t, http.StatusAccepted, status)
	done = f.waitJob(t, body["job"].(map[string]any)["id"].(string))
	assert.Equal(t, "failed", done["phase"])
	assert.Contains(t, done["error"], "does not fit")
}

func TestServingLoadWiresOnReadyAndUnloadUnwires(t *testing.T) {
	f := newServingFixture(t)

	// The backend needs a preset; the service passes the model's own.
	status, body := f.do(t, http.MethodPost, Prefix+"/models/load", map[string]any{"model": "org/tiny"})
	require.Equal(t, http.StatusOK, status, body)
	assert.Equal(t, true, body["loaded"])
	assert.Equal(t, "Pending", body["running"].(map[string]any)["status"])
	assert.Nil(t, body["modelConfig"], "not wired before ready")

	// A load job follows readiness.
	status, list := f.do(t, http.MethodGet, Prefix+"/jobs", nil)
	require.Equal(t, http.StatusOK, status)
	var loadJob map[string]any
	for _, j := range list["jobs"].([]any) {
		jm := j.(map[string]any)
		if jm["type"] == "load" && jm["model"] == "org/tiny" {
			loadJob = jm
		}
	}
	require.NotNil(t, loadJob, list)
	assert.Equal(t, "running", loadJob["phase"])
	assert.Equal(t, true, loadJob["wire"])

	f.backend.setReady("org/tiny")
	done := f.waitJob(t, loadJob["id"].(string))
	assert.Equal(t, "succeeded", done["phase"], done)
	result := done["result"].(map[string]any)
	assert.Equal(t, "org-tiny", result["name"])
	assert.Equal(t, "OpenAI", result["provider"])

	status, body = f.do(t, http.MethodGet, Prefix+"/models/org/tiny", nil)
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, "Ready", body["running"].(map[string]any)["status"])
	assert.Equal(t, "org-tiny", body["modelConfig"].(map[string]any)["name"])

	// Loading a preset by name alone works too.
	status, body = f.do(t, http.MethodPost, Prefix+"/models/load", map[string]any{"preset": "tiny"})
	require.Equal(t, http.StatusOK, status, body)

	// Unload deletes the endpoint and the ModelConfig with it.
	status, _ = f.do(t, http.MethodPost, Prefix+"/models/unload", map[string]any{"model": "org/tiny"})
	require.Equal(t, http.StatusOK, status)
	assert.Empty(t, f.wirer.refs, "unload unwires on a serve-lifecycle backend")
	status, body = f.do(t, http.MethodGet, Prefix+"/models/org/tiny", nil)
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, false, body["loaded"])
	assert.Nil(t, body["modelConfig"])
}

func TestServingRunAdoptsPullsAndReconcilesWiring(t *testing.T) {
	f := newServingFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go f.svc.Run(ctx)

	// Adopted pull shows up as a job and finishes through the backend.
	var adopted map[string]any
	require.Eventually(t, func() bool {
		_, list := f.do(t, http.MethodGet, Prefix+"/jobs", nil)
		for _, j := range list["jobs"].([]any) {
			jm := j.(map[string]any)
			if jm["type"] == "pull" && jm["model"] == "org/adopted" {
				adopted = jm
				return true
			}
		}
		return false
	}, 2*time.Second, 5*time.Millisecond)
	done := f.waitJob(t, adopted["id"].(string))
	assert.Equal(t, "succeeded", done["phase"])

	// A model served and ready without a load job (created out of band) gets
	// wired by the reconciler.
	f.backend.fakeBackend.mu.Lock()
	f.backend.loaded["org/tiny"] = true
	f.backend.fakeBackend.mu.Unlock()
	f.backend.setReady("org/tiny")
	require.Eventually(t, func() bool {
		ref, _ := f.wirer.Lookup(ctx, "org/tiny")
		return ref != nil
	}, 2*time.Second, 5*time.Millisecond)
	ref, _ := f.wirer.Lookup(ctx, "org/tiny")
	assert.Equal(t, "OpenAI", ref.Provider)
}

func TestServingMCPTools(t *testing.T) {
	fb := newFakeServing()
	fb.models["org/tiny"] = backend.Model{Name: "org/tiny", SizeBytes: 10, Preset: "tiny"}
	fw := newFakeWirer()
	svc := service.New(fb, jobs.NewManager(), fw, &service.WiringInfo{Namespace: "kagent"}, service.Config{AutoWire: true}, nil)
	srv := NewMCPServer(svc, "test")

	out, isErr := callTool(t, srv, ToolListPresets, nil)
	require.False(t, isErr, out)
	assert.Contains(t, out, `"name": "tiny"`)

	out, isErr = callTool(t, srv, ToolSearchModels, map[string]any{argQuery: "tiny", argLimit: 3})
	require.False(t, isErr, out)
	assert.Contains(t, out, `"org/tiny"`)
	assert.Contains(t, out, `"limit/3"`)
	out, isErr = callTool(t, srv, ToolSearchModels, map[string]any{})
	assert.True(t, isErr, out)

	out, isErr = callTool(t, srv, ToolCheckFit, map[string]any{argModel: "org/huge"})
	require.False(t, isErr, out)
	assert.Contains(t, out, `"fits": false`)
	out, isErr = callTool(t, srv, ToolCheckFit, map[string]any{argPreset: "tiny"})
	require.False(t, isErr, out)
	assert.Contains(t, out, `"fits": true`)

	out, isErr = callTool(t, srv, ToolListNodes, nil)
	require.False(t, isErr, out)
	assert.Contains(t, out, `"name": "n1"`)

	out, isErr = callTool(t, srv, ToolLoadModel, map[string]any{argPreset: "tiny"})
	require.False(t, isErr, out)
	assert.Contains(t, out, `"loaded": true`)
	out, isErr = callTool(t, srv, ToolLoadModel, map[string]any{})
	assert.True(t, isErr, out)
	assert.Contains(t, out, "invalid_request")

	out, isErr = callTool(t, srv, ToolPullModel, map[string]any{argModel: "org/huge"})
	require.False(t, isErr, out, "the job is accepted; the fit refusal fails the job")
	out, isErr = callTool(t, srv, ToolPullModel, map[string]any{argModel: "org/x", argWire: true})
	assert.True(t, isErr, out)
	assert.Contains(t, out, "wired when loaded")

	// The ollama-shaped fake answers unsupported for the kserve tools.
	plain := NewMCPServer(service.New(newFakeBackend(), jobs.NewManager(), nil, nil, service.Config{}, nil), "test")
	for _, tool := range []string{ToolListPresets, ToolListNodes} {
		out, isErr = callTool(t, plain, tool, nil)
		assert.True(t, isErr, tool)
		assert.Contains(t, out, "unsupported", tool)
	}
}

func TestServingDedupesPortalWiredModelConfigs(t *testing.T) {
	f := newServingFixture(t)
	// The portal already wired the served model: same predictor host, same
	// served model name, not managed by model-manager.
	portal := wiring.ModelConfigRef{Name: "org-tiny", Namespace: "kagent", Provider: "OpenAI", Model: "org-tiny", ProviderModel: "org-tiny", Endpoint: "http://org-tiny-predictor.model-serving.svc.cluster.local/v1", Ready: true, Managed: false}
	f.wirer.mu.Lock()
	f.wirer.foreign = append(f.wirer.foreign, portal)
	f.wirer.mu.Unlock()

	status, body := f.do(t, http.MethodPost, Prefix+"/models/load", map[string]any{"model": "org/tiny"})
	require.Equal(t, http.StatusOK, status, body)
	f.backend.setReady("org/tiny")
	var loadJob map[string]any
	_, list := f.do(t, http.MethodGet, Prefix+"/jobs", nil)
	for _, j := range list["jobs"].([]any) {
		jm := j.(map[string]any)
		if jm["type"] == "load" {
			loadJob = jm
		}
	}
	require.NotNil(t, loadJob)
	done := f.waitJob(t, loadJob["id"].(string))
	assert.Equal(t, "succeeded", done["phase"], done)
	result := done["result"].(map[string]any)
	assert.Equal(t, "org-tiny", result["name"])
	assert.Equal(t, false, result["managed"], "the portal's ModelConfig is reported, not replaced")
	assert.Empty(t, f.wirer.refs, "no duplicate ModelConfig was created")

	// The model view shows the portal's ModelConfig through the endpoint join.
	status, body = f.do(t, http.MethodGet, Prefix+"/models/org/tiny", nil)
	require.Equal(t, http.StatusOK, status)
	mc := body["modelConfig"].(map[string]any)
	assert.Equal(t, "org-tiny", mc["name"])
	assert.Equal(t, false, mc["managed"])

	// Unwire / unload never delete a ModelConfig model-manager did not create.
	status, _ = f.do(t, http.MethodPost, Prefix+"/models/unwire", map[string]any{"model": "org/tiny"})
	require.Equal(t, http.StatusOK, status)
	status, _ = f.do(t, http.MethodPost, Prefix+"/models/unload", map[string]any{"model": "org/tiny"})
	require.Equal(t, http.StatusOK, status)
	f.wirer.mu.Lock()
	assert.Len(t, f.wirer.foreign, 1)
	f.wirer.mu.Unlock()

	// Without the portal's ModelConfig, model-manager names its own after the
	// served resource (the InferenceService name).
	f.wirer.mu.Lock()
	f.wirer.foreign = nil
	f.wirer.mu.Unlock()
	status, _ = f.do(t, http.MethodPost, Prefix+"/models/load", map[string]any{"model": "org/tiny"})
	require.Equal(t, http.StatusOK, status)
	require.Eventually(t, func() bool {
		f.wirer.mu.Lock()
		defer f.wirer.mu.Unlock()
		r, ok := f.wirer.refs["org/tiny"]
		return ok && r.Name == "org-tiny" && r.Managed
	}, 2*time.Second, 5*time.Millisecond)
}
