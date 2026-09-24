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
	"github.com/giantswarm/model-manager/internal/buildinfo"
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
	// gateway, when set, is the models Gateway's origin: served models are
	// routed on it (https://<gateway>/model-serving/<name>) and reached with
	// the caller's token, as the kserve driver composes from discovery.
	gateway string
	// managedBy overrides the managed-by label of a served object (default
	// model-manager's own value).
	managedBy map[string]string
	// inventory, when set, is what an inventory read (GetModel) waits for
	// before answering — a cache scan pod that outlasts the caller.
	inventory chan struct{}
}

func newFakeServing() *fakeServing {
	fb := newFakeBackend()
	fb.caps = backend.Capabilities{Pull: true, PullProgress: true, Delete: true, Load: true, Unload: true, LoadedModels: true, Presets: true, FitCheck: true, NodeInventory: true, Search: true}
	return &fakeServing{
		fakeBackend: fb,
		ready:       map[string]bool{},
		presets:     []backend.Preset{{Name: "tiny", DisplayName: "Tiny", Model: "org/tiny", GPUs: 1, WeightsBytes: 10, OverheadBytes: 20, RequiredBytes: 30}},
		managedBy:   map[string]string{},
	}
}

// endpoint is where a served model answers: its route on the models Gateway
// when one is configured, else its in-cluster predictor Service.
func (f *fakeServing) endpoint(resource string) string {
	if f.gateway != "" {
		return f.gateway + "/model-serving/" + resource
	}
	return "http://" + resource + "-predictor.model-serving.svc.cluster.local"
}

// Name is kserve unless the embedded fake was given another name (a second
// serving-shaped backend in one service).
func (f *fakeServing) Name() backend.Name {
	if f.name != "" {
		return f.name
	}
	return backend.NameKServe
}

// GetModel resolves preset names too, as the kserve driver does — after the
// cache scan an inventory read waits for, when the test holds one.
func (f *fakeServing) GetModel(ctx context.Context, name string) (*backend.Model, error) {
	f.mu.Lock()
	scan := f.inventory
	f.mu.Unlock()
	if scan != nil {
		select {
		case <-scan:
		case <-ctx.Done():
			return nil, fmt.Errorf("scan cache on any node: waiting for Job mm-scan: %w", ctx.Err())
		}
	}
	return f.fakeBackend.GetModel(ctx, f.repoOf(name))
}

// repoOf is the repository a reference names: the preset's model for a
// preset name, else the reference itself.
func (f *fakeServing) repoOf(name string) string {
	for _, p := range f.presets {
		if p.Name == name {
			return p.Model
		}
	}
	return name
}

// blockInventory holds every inventory read until block is closed.
func (f *fakeServing) blockInventory(block chan struct{}) {
	f.mu.Lock()
	f.inventory = block
	f.mu.Unlock()
}

// Stop is the kserve driver's unload: the served object is found by its
// repository or preset name — never through the inventory — and the answer
// says the inventory is rescanned in the background.
func (f *fakeServing) Stop(ctx context.Context, name string) (*backend.UnloadResult, error) {
	repo := f.repoOf(name)
	f.fakeBackend.mu.Lock()
	served := f.loaded[repo]
	f.fakeBackend.mu.Unlock()
	if !served {
		return nil, fmt.Errorf("%w: no LLMInferenceService serves %s", backend.ErrNotFound, name)
	}
	if err := f.Unload(ctx, repo); err != nil {
		return nil, err
	}
	return &backend.UnloadResult{Model: repo, Inventory: backend.InventoryRefresh{Refreshing: true}}, nil
}
func (f *fakeServing) Info(context.Context) backend.Info {
	return backend.Info{Backend: f.Name(), Version: "serving.kserve.io/v1beta1", Healthy: true}
}
func (f *fakeServing) ListLoaded(ctx context.Context) ([]backend.LoadedModel, error) {
	loaded, err := f.fakeBackend.ListLoaded(ctx)
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range loaded {
		loaded[i].ExpiresAt = nil // kserve knows no keep-alive
		loaded[i].Status = "Pending"
		if f.ready[loaded[i].Name] {
			// Like the kserve driver, a Ready model reports what its server
			// registered.
			loaded[i].Status = "Ready"
			loaded[i].Runtime = &backend.Runtime{Name: "vllm", Version: "0.23.0"}
			loaded[i].Interfaces = []backend.Interface{{Type: backend.InterfaceCompletions, Path: "/v1/chat/completions"}, {Type: backend.InterfaceMessages, Path: "/v1/messages"}}
		}
		loaded[i].Resource = strings.ReplaceAll(loaded[i].Name, "/", "-")
		loaded[i].Endpoint = f.endpoint(loaded[i].Resource)
		loaded[i].ManagedBy = wiring.ManagedByValue
		if by, ok := f.managedBy[loaded[i].Name]; ok {
			loaded[i].ManagedBy = by
		}
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
	// Like the kserve driver: the first sample says where the download lands
	// — the named node, else the one the fit check picked (n1 here).
	node := req.Node
	if node == "" {
		node = "n1"
	}
	progress(backend.Progress{Status: "download job created", Node: node, Preset: req.Preset})
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
	f.mu.Lock()
	routed := f.gateway != ""
	f.mu.Unlock()
	return backend.AgentEndpoint{Provider: "OpenAI", BaseURL: f.endpoint(name) + "/v1", Model: name, APIKeyPassthrough: routed, PlaceholderAPIKey: !routed, Name: name}
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
	return []backend.NodeInfo{
		{Name: "n1", Ready: true, Eligible: true, BudgetBytes: 64, BudgetSource: "allocatable", Cache: &backend.NodeCache{Claim: "hf-cache", MountPath: "/mnt/models", Models: 1}},
		{Name: "gpu2", Ready: true, Eligible: false, EligibilityReason: "cache claim hf-cache is pinned to n1", BudgetBytes: 128, BudgetSource: "gpu-labels"},
	}, nil
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
	svc := service.New([]backend.Backend{fb}, jobs.NewManager(), fw, &service.WiringInfo{Namespace: "kagent", APIVersion: wiring.DefaultAPIVersion}, service.Config{AutoWire: true, DefaultKeepAlive: "5m", ReconcileInterval: 5 * time.Millisecond}, nil)
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
	loading := body["loading"].(map[string]any)
	assert.Equal(t, false, loading["onDemand"], "a stopped LLMInferenceService does not come back on request")
	assert.Equal(t, false, loading["idleEviction"], "a running LLMInferenceService stays until unloaded")
	assert.NotContains(t, loading, "keepAliveDefault", "no keep-alive on kserve")
	assert.NotContains(t, loading, "keepAliveScope", "no keep-alive on kserve")
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
	assert.Equal(t, "kserve", body["backend"], "the fit result names its backend")
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
	require.Len(t, nodes, 2)
	n := nodes[0].(map[string]any)
	assert.Equal(t, "n1", n["name"])
	assert.Equal(t, "kserve", n["backend"], "every node names its backend")
	assert.Equal(t, true, n["eligible"])
	assert.NotContains(t, n, "eligibilityReason", "no reason on an eligible node")
	assert.Equal(t, "hf-cache", n["cache"].(map[string]any)["claim"])
	other := nodes[1].(map[string]any)
	assert.Equal(t, false, other["eligible"])
	assert.Equal(t, "cache claim hf-cache is pinned to n1", other["eligibilityReason"])
}

// A Ready model's runtime and interfaces are on its loaded entry and on the
// model's running entry; a preset states no interfaces.
func TestServingReportsTheInterfacesOfAReadyModel(t *testing.T) {
	f := newServingFixture(t)
	status, body := f.do(t, http.MethodPost, Prefix+"/models/load", map[string]any{"model": "org/tiny"})
	require.Equal(t, http.StatusOK, status, body)
	status, body = f.do(t, http.MethodGet, Prefix+"/loaded", nil)
	require.Equal(t, http.StatusOK, status)
	pending := body["loaded"].([]any)[0].(map[string]any)
	assert.NotContains(t, pending, "interfaces", "a model that is not Ready was not read")
	assert.NotContains(t, pending, "runtime")

	f.backend.setReady("org/tiny")
	status, body = f.do(t, http.MethodGet, Prefix+"/loaded", nil)
	require.Equal(t, http.StatusOK, status)
	lm := body["loaded"].([]any)[0].(map[string]any)
	assert.Equal(t, map[string]any{"name": "vllm", "version": "0.23.0"}, lm["runtime"])
	assert.Equal(t, []any{
		map[string]any{"type": "Completions", "path": "/v1/chat/completions"},
		map[string]any{"type": "Messages", "path": "/v1/messages"},
	}, lm["interfaces"])

	status, body = f.do(t, http.MethodGet, Prefix+"/models", nil)
	require.Equal(t, http.StatusOK, status)
	running := body["models"].([]any)[0].(map[string]any)["running"].(map[string]any)
	assert.Equal(t, lm["interfaces"], running["interfaces"], "list_models shows a loaded model's interfaces")

	status, body = f.do(t, http.MethodGet, Prefix+"/presets", nil)
	require.Equal(t, http.StatusOK, status)
	assert.NotContains(t, body["presets"].([]any)[0].(map[string]any), "interfaces", "a preset states no interfaces")
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
	assert.Equal(t, "tiny", job["preset"], "the 202 echoes what the request named")
	assert.Equal(t, "n1", job["node"])
	done := f.waitJob(t, job["id"].(string))
	assert.Equal(t, "succeeded", done["phase"])
	assert.Nil(t, done["result"])
	assert.Equal(t, "tiny", done["preset"])
	assert.Equal(t, "n1", done["node"])
	assert.Zero(t, f.wirer.count(), "kserve models are wired when served, not when pulled")
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

func TestServingPullJobCarriesTheNodeTheBackendPicked(t *testing.T) {
	f := newServingFixture(t)
	// The request left the node open: the job carries no node until the
	// backend's fit check picked one, then names it — in GET /jobs too, so a
	// client that did not issue the pull can place the download.
	status, body := f.do(t, http.MethodPost, Prefix+"/models/pull", map[string]any{"model": "org/open"})
	require.Equal(t, http.StatusAccepted, status, body)
	job := body["job"].(map[string]any)
	assert.NotContains(t, job, "node")
	assert.NotContains(t, job, "preset")
	done := f.waitJob(t, job["id"].(string))
	assert.Equal(t, "succeeded", done["phase"])
	assert.Equal(t, "n1", done["node"], "the node the backend picked")
	assert.NotContains(t, done, "preset", "none named, none resolved by this fake")

	status, list := f.do(t, http.MethodGet, Prefix+"/jobs", nil)
	require.Equal(t, http.StatusOK, status)
	var listed map[string]any
	for _, j := range list["jobs"].([]any) {
		if jm := j.(map[string]any); jm["id"] == job["id"] {
			listed = jm
		}
	}
	require.NotNil(t, listed)
	assert.Equal(t, "n1", listed["node"])
}

func TestServingLoadWiresInTheCallAndUnloadUnwires(t *testing.T) {
	f := newServingFixture(t)

	// The backend needs a preset; the service passes the model's own.
	status, body := f.do(t, http.MethodPost, Prefix+"/models/load", map[string]any{"model": "org/tiny"})
	require.Equal(t, http.StatusOK, status, body)
	assert.Equal(t, true, body["loaded"])
	running := body["running"].(map[string]any)
	assert.Equal(t, "Pending", running["status"])
	assert.Equal(t, "org-tiny", running["resource"], "the answer names the serving object the load created")
	// The ModelConfig is created in the same call, before the model is
	// ready: the answer's wiring says so and modelConfig carries it.
	wiringOut := body["wiring"].(map[string]any)
	assert.Equal(t, true, wiringOut["wired"])
	assert.Equal(t, service.WiredOnLoad, wiringOut["reason"])
	assert.Equal(t, "org-tiny", wiringOut["modelConfig"].(map[string]any)["name"], "named after the serving object")
	assert.Equal(t, "org-tiny", body["modelConfig"].(map[string]any)["name"])
	ref, ok := f.wirer.get(backend.NameKServe, "org/tiny")
	require.True(t, ok, "the ModelConfig exists while the model is still Pending")
	assert.True(t, ref.Managed)

	// A load job follows readiness, naming the object it follows.
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
	assert.Equal(t, "org-tiny", loadJob["resource"])

	f.backend.setReady("org/tiny")
	done := f.waitJob(t, loadJob["id"].(string))
	assert.Equal(t, "succeeded", done["phase"], done)
	result := done["result"].(map[string]any)
	assert.Equal(t, "org-tiny", result["name"])
	assert.Equal(t, "OpenAI", result["provider"])
	assert.Equal(t, 1, f.wirer.count(), "the job refreshed the ModelConfig the load wired; it did not create a second one")

	status, body = f.do(t, http.MethodGet, Prefix+"/models/org/tiny", nil)
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, "Ready", body["running"].(map[string]any)["status"])
	assert.Equal(t, "org-tiny", body["modelConfig"].(map[string]any)["name"])

	// The loaded list carries the ModelConfig too, and — wired already —
	// reports no wiring of its own.
	status, list = f.do(t, http.MethodGet, Prefix+"/loaded", nil)
	require.Equal(t, http.StatusOK, status)
	entry := list["loaded"].([]any)[0].(map[string]any)
	assert.Equal(t, "org-tiny", entry["modelConfig"].(map[string]any)["name"])
	assert.Nil(t, entry["wiring"])

	// Loading a preset by name alone works too.
	status, body = f.do(t, http.MethodPost, Prefix+"/models/load", map[string]any{"preset": "tiny"})
	require.Equal(t, http.StatusOK, status, body)

	// Unload deletes the endpoint and the ModelConfig with it.
	status, _ = f.do(t, http.MethodPost, Prefix+"/models/unload", map[string]any{"model": "org/tiny"})
	require.Equal(t, http.StatusOK, status)
	assert.Zero(t, f.wirer.count(), "unload unwires on a serve-lifecycle backend")
	status, body = f.do(t, http.MethodGet, Prefix+"/models/org/tiny", nil)
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, false, body["loaded"])
	assert.Nil(t, body["modelConfig"])
}

// A model routed on the models Gateway is wired with the caller's token
// forwarded from the load call on: the backend knows the routed address at
// compose time, so nothing waits for KServe to publish it.
func TestServingLoadWiresARoutedModelWithPassthroughBeforeReady(t *testing.T) {
	f := newServingFixture(t)
	f.backend.mu.Lock()
	f.backend.gateway = "https://models.example.com"
	f.backend.mu.Unlock()

	status, body := f.do(t, http.MethodPost, Prefix+"/models/load", map[string]any{"model": "org/tiny"})
	require.Equal(t, http.StatusOK, status, body)
	assert.Equal(t, "Pending", body["running"].(map[string]any)["status"], "not ready yet")
	wiringOut := body["wiring"].(map[string]any)
	assert.Equal(t, true, wiringOut["wired"])
	assert.Equal(t, service.WiredOnLoad, wiringOut["reason"])
	mc := wiringOut["modelConfig"].(map[string]any)
	assert.Equal(t, true, mc["apiKeyPassthrough"], "the Gateway admits the person's token and nothing else")
	assert.Nil(t, mc["apiKeySecret"])
	assert.Equal(t, "https://models.example.com/model-serving/org-tiny/v1", mc["endpoint"])
}

// A served model model-manager manages that has no ModelConfig — a load that
// ran before this rule, or a wiring that failed — is wired by the next read
// of the loaded list, as the caller, and the entry says so. Objects someone
// else manages are theirs to wire.
func TestServingReadWiresAManagedServedModelWithoutModelConfig(t *testing.T) {
	f := newServingFixture(t)
	f.backend.models["org/theirs"] = backend.Model{Name: "org/theirs", SizeBytes: 10, Preset: "tiny"}
	f.backend.fakeBackend.mu.Lock()
	f.backend.loaded["org/tiny"] = true
	f.backend.loaded["org/theirs"] = true
	f.backend.fakeBackend.mu.Unlock()
	f.backend.mu.Lock()
	f.backend.managedBy["org/theirs"] = "backstage"
	f.backend.mu.Unlock()
	assert.Zero(t, f.wirer.count(), "nothing wired yet")

	status, body := f.do(t, http.MethodGet, Prefix+"/loaded", nil)
	require.Equal(t, http.StatusOK, status, body)
	byName := map[string]map[string]any{}
	for _, e := range body["loaded"].([]any) {
		em := e.(map[string]any)
		byName[em["name"].(string)] = em
	}
	ours := byName["org/tiny"]
	require.NotNil(t, ours, body)
	wiringOut := ours["wiring"].(map[string]any)
	assert.Equal(t, true, wiringOut["wired"])
	assert.Equal(t, service.WiredOnRead, wiringOut["reason"])
	assert.Equal(t, "org-tiny", wiringOut["modelConfig"].(map[string]any)["name"])
	assert.Equal(t, "org-tiny", ours["modelConfig"].(map[string]any)["name"])
	ref, ok := f.wirer.get(backend.NameKServe, "org/tiny")
	require.True(t, ok)
	assert.True(t, ref.Managed)

	theirs := byName["org/theirs"]
	require.NotNil(t, theirs, body)
	assert.Nil(t, theirs["wiring"], "an object someone else manages is not wired by a read")
	assert.Nil(t, theirs["modelConfig"])
	_, ok = f.wirer.get(backend.NameKServe, "org/theirs")
	assert.False(t, ok)

	// The next read finds the ModelConfig and reports no wiring of its own.
	status, body = f.do(t, http.MethodGet, Prefix+"/loaded", nil)
	require.Equal(t, http.StatusOK, status)
	for _, e := range body["loaded"].([]any) {
		em := e.(map[string]any)
		if em["name"] == "org/tiny" {
			assert.Equal(t, "org-tiny", em["modelConfig"].(map[string]any)["name"])
			assert.Nil(t, em["wiring"])
		}
	}
	assert.Equal(t, 1, f.wirer.count())
}

// A model-manager restart between load and Ready drops the in-memory load
// job. The model stays wired — the load wrote the ModelConfig — and a model
// whose ModelConfig is missing all the same is wired by the caller's next
// read, so an agent created on it answers once the model serves.
func TestServingRestartDuringColdStartLeavesAWiredModel(t *testing.T) {
	f := newServingFixture(t)
	status, body := f.do(t, http.MethodPost, Prefix+"/models/load", map[string]any{"model": "org/tiny"})
	require.Equal(t, http.StatusOK, status, body)
	_, ok := f.wirer.get(backend.NameKServe, "org/tiny")
	require.True(t, ok, "wired by the load")

	// The process restarts: a new service over the same cluster state (the
	// backend's objects, the ModelConfigs) with an empty job table.
	restarted := service.New([]backend.Backend{f.backend}, jobs.NewManager(), f.wirer, &service.WiringInfo{Namespace: "kagent"}, service.Config{AutoWire: true, CallerOnly: true}, nil)
	mux := http.NewServeMux()
	NewREST(restarted, nil).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	after := &fixture{srv: srv}
	_, list := after.do(t, http.MethodGet, Prefix+"/jobs", nil)
	assert.Empty(t, list["jobs"], "the load job did not survive the restart")

	status, body = after.do(t, http.MethodGet, Prefix+"/loaded", nil)
	require.Equal(t, http.StatusOK, status, body)
	entry := body["loaded"].([]any)[0].(map[string]any)
	assert.Equal(t, "Pending", entry["status"], "still starting")
	assert.Equal(t, "org-tiny", entry["modelConfig"].(map[string]any)["name"], "wired although no job watches the model")
	assert.Nil(t, entry["wiring"], "nothing to mend")

	// A served model whose ModelConfig is missing all the same (wired by a
	// job that died before this rule) is mended by the read.
	require.NoError(t, f.wirer.Remove(context.Background(), backend.NameKServe, "org/tiny"))
	status, body = after.do(t, http.MethodGet, Prefix+"/loaded", nil)
	require.Equal(t, http.StatusOK, status, body)
	entry = body["loaded"].([]any)[0].(map[string]any)
	wiringOut := entry["wiring"].(map[string]any)
	assert.Equal(t, true, wiringOut["wired"])
	assert.Equal(t, service.WiredOnRead, wiringOut["reason"])
	_, ok = f.wirer.get(backend.NameKServe, "org/tiny")
	assert.True(t, ok)

	// Ready: the first process's load job — alive in this test, dead in the
	// restart it stands for — refreshes the ModelConfig and ends; waiting for
	// it keeps its refresh from landing behind the unload below. Still one
	// ModelConfig; unload removes it.
	f.backend.setReady("org/tiny")
	_, list = f.do(t, http.MethodGet, Prefix+"/jobs", nil)
	for _, j := range list["jobs"].([]any) {
		if jm := j.(map[string]any); jm["type"] == "load" && jm["model"] == "org/tiny" {
			f.waitJob(t, jm["id"].(string))
		}
	}
	status, body = after.do(t, http.MethodGet, Prefix+"/models/org/tiny", nil)
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, "Ready", body["running"].(map[string]any)["status"])
	assert.Equal(t, "org-tiny", body["modelConfig"].(map[string]any)["name"])
	assert.Equal(t, 1, f.wirer.count())
	status, _ = after.do(t, http.MethodPost, Prefix+"/models/unload", map[string]any{"model": "org/tiny"})
	require.Equal(t, http.StatusOK, status)
	assert.Zero(t, f.wirer.count(), "unload still unwires")
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
	assert.Equal(t, "n1", adopted["node"], "read back from the download Job's annotations")
	assert.Equal(t, "tiny", adopted["preset"])
	done := f.waitJob(t, adopted["id"].(string))
	assert.Equal(t, "succeeded", done["phase"])
	assert.Equal(t, "n1", done["node"])
	assert.Equal(t, "tiny", done["preset"])

	// A model served and ready without a load job (created out of band) gets
	// wired by the reconciler.
	f.backend.fakeBackend.mu.Lock()
	f.backend.loaded["org/tiny"] = true
	f.backend.fakeBackend.mu.Unlock()
	f.backend.setReady("org/tiny")
	require.Eventually(t, func() bool {
		ref, _ := f.wirer.Lookup(ctx, backend.NameKServe, "org/tiny")
		return ref != nil
	}, 2*time.Second, 5*time.Millisecond)
	ref, _ := f.wirer.Lookup(ctx, backend.NameKServe, "org/tiny")
	assert.Equal(t, "OpenAI", ref.Provider)
}

func TestServingMCPTools(t *testing.T) {
	fb := newFakeServing()
	fb.models["org/tiny"] = backend.Model{Name: "org/tiny", SizeBytes: 10, Preset: "tiny"}
	fw := newFakeWirer()
	svc := service.New([]backend.Backend{fb}, jobs.NewManager(), fw, &service.WiringInfo{Namespace: "kagent"}, service.Config{AutoWire: true}, nil)
	srv := NewMCPServer(svc, buildinfo.Info{Version: "test"})

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
	assert.Contains(t, out, `"wired": true`, "the ModelConfig is created in the load call")
	assert.Contains(t, out, `"reason": "wired on load"`)
	out, isErr = callTool(t, srv, ToolLoadModel, map[string]any{})
	assert.True(t, isErr, out)
	assert.Contains(t, out, "invalid_request")

	out, isErr = callTool(t, srv, ToolUnloadModel, map[string]any{argModel: "org/tiny"})
	require.False(t, isErr, out)
	assert.Contains(t, out, `"status": "Terminating"`)
	assert.Contains(t, out, `"refreshing": true`, "the answer says the inventory is rescanned in the background")
	assert.Contains(t, out, "the cache inventory is rescanned in the background")
	out, isErr = callTool(t, srv, ToolUnloadModel, map[string]any{argModel: "org/tiny"})
	assert.True(t, isErr, out)
	assert.Contains(t, out, "not_found", "nothing serves it any more")

	out, isErr = callTool(t, srv, ToolPullModel, map[string]any{argModel: "org/huge"})
	require.False(t, isErr, out, "the job is accepted; the fit refusal fails the job")
	out, isErr = callTool(t, srv, ToolPullModel, map[string]any{argModel: "org/x", argWire: true})
	assert.True(t, isErr, out)
	assert.Contains(t, out, "wired when loaded")

	// The ollama-shaped fake answers unsupported for the kserve tools.
	plain := NewMCPServer(service.New([]backend.Backend{newFakeBackend()}, jobs.NewManager(), nil, nil, service.Config{}, nil), buildinfo.Info{Version: "test"})
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
	assert.Zero(t, f.wirer.count(), "no duplicate ModelConfig was created")

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
	// served resource (the LLMInferenceService name).
	f.wirer.mu.Lock()
	f.wirer.foreign = nil
	f.wirer.mu.Unlock()
	status, _ = f.do(t, http.MethodPost, Prefix+"/models/load", map[string]any{"model": "org/tiny"})
	require.Equal(t, http.StatusOK, status)
	require.Eventually(t, func() bool {
		r, ok := f.wirer.get(backend.NameKServe, "org/tiny")
		return ok && r.Name == "org-tiny" && r.Managed
	}, 2*time.Second, 5*time.Millisecond)
}

// unload on a serving backend whose inventory read would outlast the caller
// (a cache scan pod that cannot start): the serving object is deleted, the
// ModelConfig unwired and the call answered within the caller's deadline —
// the backend is asked to stop by the reference, no inventory read stands
// in between — and the answer says the inventory is rescanned in the
// background (giantswarm/model-manager#119).
func TestServingUnloadAnswersWithinDeadlineWithASlowInventory(t *testing.T) {
	f := newServingFixture(t)
	status, body := f.do(t, http.MethodPost, Prefix+"/models/load", map[string]any{"model": "org/tiny"})
	require.Equal(t, http.StatusOK, status, body)
	_, ok := f.wirer.get(backend.NameKServe, "org/tiny")
	require.True(t, ok, "wired on load")

	// Every inventory read now waits for a scan that never finishes within
	// the caller's deadline.
	block := make(chan struct{})
	defer close(block)
	f.backend.blockInventory(block)
	const deadline = 500 * time.Millisecond
	client := &http.Client{Timeout: deadline}
	start := time.Now()
	status, body = f.doWith(t, client, http.MethodPost, Prefix+"/models/unload", map[string]any{"model": "org/tiny"})
	require.Equal(t, http.StatusOK, status, body)
	assert.Less(t, time.Since(start), deadline, "answered within the deadline")
	assert.Equal(t, "kserve", body["backend"])
	assert.Equal(t, "org/tiny", body["model"])
	assert.Equal(t, false, body["loaded"])
	assert.Equal(t, true, body["inventory"].(map[string]any)["refreshing"], "the answer says what follows")
	assert.Zero(t, f.wirer.count(), "unwired within the call")
	loaded, err := f.backend.ListLoaded(context.Background())
	require.NoError(t, err)
	assert.Empty(t, loaded, "the serving object is gone")

	// Nothing serves it any more: not found, still without an inventory read.
	status, body = f.doWith(t, client, http.MethodPost, Prefix+"/models/unload", map[string]any{"model": "org/tiny"})
	assert.Equal(t, http.StatusNotFound, status, body)
	// The reads that need the inventory still wait for it: the deadline is
	// theirs to spend.
	_, err = client.Get(f.srv.URL + Prefix + "/models/org/tiny")
	assert.Error(t, err, "an inventory read waits for the scan")
}

// doWith is do with the caller's HTTP client (its timeout is the caller's
// deadline).
func (f *servingFixture) doWith(t *testing.T, client *http.Client, method, path string, body any) (int, map[string]any) {
	t.Helper()
	fx := &fixture{srv: f.srv}
	return fx.doWith(t, client, method, path, body)
}
