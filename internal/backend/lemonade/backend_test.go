package lemonade

import (
	"context"
	"encoding/json"
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
)

// fakeLemonade is an httptest stand-in for Lemonade Server's management API.
type fakeLemonade struct {
	mu     sync.Mutex
	models map[string]apiModel
	loaded map[string]*loadedModel
	// pullError makes the next streamed pull fail with this error event.
	pullError string
	pullCode  string
	// pullPlain makes pull answer one JSON document instead of a stream.
	pullPlain bool
	// starting marks a loaded model whose backend process is not ready yet.
	starting map[string]bool
	// systemInfo is the /api/v1/system-info document.
	systemInfo string
	srv        *httptest.Server
}

const testSystemInfo = `{
  "OS Version": "Linux-7.2.2-arch1-1 (Arch Linux)",
  "Physical Memory": "86.07 GB",
  "Processor": "ERROR - Processor name not found",
  "devices": {
    "amd_gpu": [{"available": true, "family": "gfx1150", "integrated": true, "name": "110500", "vram_gb": 8.0}],
    "amd_npu": {"available": true, "family": "XDNA2", "name": "AMD NPU (NPU Strix)", "power_mode": "DEFAULT"},
    "cpu": {"available": false, "cores": 0, "family": "x86_64", "name": "", "threads": 68},
    "nvidia_gpu": [{"available": false, "error": "No NVIDIA discrete GPU found", "name": ""}]
  },
  "model_storage": {"free_bytes": 539596308480, "path": "/home/lab/.cache/huggingface/hub", "total_bytes": 2015001456640, "used_bytes": 1475405148160}
}`

func newFakeLemonade(t *testing.T) *fakeLemonade {
	t.Helper()
	f := &fakeLemonade{models: map[string]apiModel{}, loaded: map[string]*loadedModel{}, starting: map[string]bool{}, systemInfo: testSystemInfo}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/health", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		out := map[string]any{"status": "ok", "version": "11.9.0", "all_models_loaded": []loadedModel{}, "max_models": map[string]int{"llm": 1}}
		loaded := []loadedModel{}
		for _, l := range f.loaded {
			loaded = append(loaded, *l)
		}
		out["all_models_loaded"] = loaded
		_ = json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("GET /api/v1/models", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		out := modelsResponse{Data: []apiModel{}}
		for _, m := range f.models {
			out.Data = append(out.Data, m)
		}
		_ = json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("GET /api/v1/system-info", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(f.systemInfo))
	})
	mux.HandleFunc("POST /api/v1/pull", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ModelName string `json:"model_name"`
			Stream    bool   `json:"stream"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if f.pullPlain {
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "success", "message": "Installed model: " + req.ModelName})
			f.add(req.ModelName, 0.5)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		send := func(event string, data any) {
			b, _ := json.Marshal(data)
			_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		}
		if f.pullError != "" {
			send("error", map[string]string{"code": f.pullCode, "error": f.pullError})
			return
		}
		if !strings.HasPrefix(req.ModelName, "user.") && !strings.Contains(req.ModelName, "GGUF") && !strings.Contains(req.ModelName, "FLM") {
			send("error", map[string]string{"code": "unknown_model", "error": "When registering a new model, the model name must include the `user` namespace, for example `user.Phi-4-Mini-GGUF`. Received: " + req.ModelName})
			return
		}
		// Two files, totals known on every event as the server reports them.
		send("progress", map[string]any{"file": "model.gguf", "file_index": 1, "total_files": 2, "bytes_downloaded": 100, "bytes_total": 300, "percent": 33, "total_download_size": 400, "cumulative_bytes_downloaded": 100})
		send("progress", map[string]any{"file": "model.gguf", "file_index": 1, "total_files": 2, "bytes_downloaded": 300, "bytes_total": 300, "percent": 100, "total_download_size": 400, "cumulative_bytes_downloaded": 300})
		send("progress", map[string]any{"file": "config.json", "file_index": 2, "total_files": 2, "bytes_downloaded": 50, "bytes_total": 100, "percent": 50, "total_download_size": 400, "cumulative_bytes_downloaded": 350})
		f.add(req.ModelName, 0.4)
		send("complete", map[string]any{"file_index": 2, "total_files": 2, "percent": 100, "total_download_size": 400})
	})
	mux.HandleFunc("POST /api/v1/load", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ModelName string `json:"model_name"`
			Pinned    bool   `json:"pinned"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		defer f.mu.Unlock()
		m, ok := f.models[req.ModelName]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "model_not_found", "message": fmt.Sprintf("Model '%s' was not found. Use 'lemonade list' or GET /api/v1/models?show_all=true to see all available models.", req.ModelName), "type": "model_not_found", "param": "model"}})
			return
		}
		l := &loadedModel{ModelName: m.ID, Checkpoint: m.Checkpoint, Type: "llm", Device: "npu", Pinned: req.Pinned, Recipe: m.Recipe, Status: "ready", BackendHealth: "ready", MaxContextWindow: m.MaxContextWindow}
		l.RecipeOptions.CtxSize = m.ContextLength
		if f.starting[m.ID] {
			l.Status, l.BackendHealth = "loading", "starting"
		}
		f.loaded[m.ID] = l
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "model_name": m.ID, "checkpoint": m.Checkpoint, "recipe": m.Recipe})
	})
	mux.HandleFunc("POST /api/v1/unload", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ModelName string `json:"model_name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		defer f.mu.Unlock()
		if _, ok := f.loaded[req.ModelName]; !ok {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "Model not loaded: " + req.ModelName})
			return
		}
		delete(f.loaded, req.ModelName)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "success", "message": "Model unloaded successfully", "model_name": req.ModelName})
	})
	mux.HandleFunc("POST /api/v1/delete", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ModelName string `json:"model_name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		defer f.mu.Unlock()
		if _, ok := f.models[req.ModelName]; !ok {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "Model not found: " + req.ModelName})
			return
		}
		delete(f.models, req.ModelName)
		delete(f.loaded, req.ModelName)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "success", "message": "Deleted model: " + req.ModelName})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// add registers a downloaded FLM chat model with tool calling.
func (f *fakeLemonade) add(id string, sizeGB float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	yes := true
	f.models[id] = apiModel{ID: id, Checkpoint: "qwen3-it:4b", Recipe: "flm", SizeGB: sizeGB, Labels: []string{"tool-calling", "chat"}, Downloaded: &yes, ContextLength: 16384, MaxContextWindow: 40960}
}

// addGGUF registers a downloaded llama.cpp model from a GGUF checkpoint.
func (f *fakeLemonade) addGGUF(id, checkpoint string, sizeGB float64, labels ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	yes := true
	f.models[id] = apiModel{ID: id, Checkpoint: checkpoint, Recipe: "llamacpp", SizeGB: sizeGB, Labels: labels, Downloaded: &yes, ContextLength: 8192}
}

func newTestBackend(t *testing.T) (*fakeLemonade, *Backend) {
	t.Helper()
	f := newFakeLemonade(t)
	b, err := New(backend.LemonadeOptions{Endpoint: f.srv.URL, AgentHost: "http://172.21.0.1:13305"})
	require.NoError(t, err)
	return f, b
}

func TestNewValidatesEndpoint(t *testing.T) {
	_, err := New(backend.LemonadeOptions{Endpoint: ""})
	require.Error(t, err)
	_, err = New(backend.LemonadeOptions{Endpoint: "172.21.0.1:13305"})
	require.Error(t, err)
	b, err := New(backend.LemonadeOptions{Endpoint: "http://172.21.0.1:13305/"})
	require.NoError(t, err)
	ep := b.AgentEndpoint("qwen3-it-4b-FLM")
	assert.Equal(t, "http://172.21.0.1:13305/api/v1", ep.BaseURL, "agent host defaults to the endpoint, plus the OpenAI-compatible path")
	assert.Equal(t, ProviderOpenAI, ep.Provider)
	assert.Equal(t, "qwen3-it-4b-FLM", ep.Model)
	assert.True(t, ep.PlaceholderAPIKey, "kagent's OpenAI provider needs a key Lemonade never checks")
	assert.Empty(t, ep.Name, "the ModelConfig is named after the model reference")

	b, err = New(backend.LemonadeOptions{Endpoint: "http://127.0.0.1:13305", AgentHost: "http://172.21.0.1:13305/api/v1"})
	require.NoError(t, err)
	assert.Equal(t, "http://172.21.0.1:13305/api/v1", b.AgentEndpoint("x").BaseURL, "an agent host that already carries the path is not doubled")
}

func TestInfoAndCapabilities(t *testing.T) {
	_, b := newTestBackend(t)
	info := b.Info(context.Background())
	assert.True(t, info.Healthy)
	assert.Equal(t, "11.9.0", info.Version)
	assert.Equal(t, backend.NameLemonade, info.Backend)
	assert.Equal(t, "http://172.21.0.1:13305/api/v1", info.AgentEndpoint, "the base URL agents dial, as written into ModelConfigs")
	assert.NotEqual(t, info.Endpoint, info.AgentEndpoint, "model-manager and agents dial different addresses here")
	assert.Equal(t, backend.Loading{OnDemand: true, IdleEviction: false}, info.Loading,
		"lemonade loads on the first request and keeps a model until unloaded or displaced; there is no keep-alive")
	caps := b.Capabilities()
	assert.True(t, caps.Pull && caps.PullProgress && caps.Delete && caps.Load && caps.Unload && caps.LoadedModels)
	assert.True(t, caps.NodeInventory, "the proxied host is reported as a node")
	assert.False(t, caps.Presets || caps.FitCheck || caps.Search, "kserve-only flags stay off")
	assert.False(t, caps.Wire, "wire is decided by the service")
}

func TestInfoUnhealthy(t *testing.T) {
	b, err := New(backend.LemonadeOptions{Endpoint: "http://127.0.0.1:1", Timeout: time.Second})
	require.NoError(t, err)
	info := b.Info(context.Background())
	assert.False(t, info.Healthy)
	assert.NotEmpty(t, info.Message)
	assert.Equal(t, "http://127.0.0.1:1/api/v1", info.AgentEndpoint, "agent endpoint is reported even when Lemonade is down")
	assert.True(t, info.Loading.OnDemand, "load semantics are a property of the driver, not of its health")
}

func TestListAndGetModels(t *testing.T) {
	f, b := newTestBackend(t)
	f.add("qwen3-it-4b-FLM", 3.1)
	f.addGGUF("Qwen3-0.6B-GGUF", "unsloth/Qwen3-0.6B-GGUF:Q4_0", 0.38, "reasoning")
	f.addGGUF("nomic-embed-text-v1-GGUF", "nomic-ai/nomic-embed-text-v1-GGUF:Q4_K_S", 0.1, "embeddings")
	ctx := context.Background()

	models, err := b.ListModels(ctx)
	require.NoError(t, err)
	require.Len(t, models, 3)
	byName := map[string]backend.Model{}
	for _, m := range models {
		byName[m.Name] = m
	}
	flm := byName["qwen3-it-4b-FLM"]
	assert.Equal(t, int64(3_100_000_000), flm.SizeBytes, "size is decimal GB")
	assert.Equal(t, "flm", flm.Runtime, "the recipe is the runtime: FastFlowLM on the NPU")
	assert.Equal(t, int64(16384), flm.ContextLength)
	assert.Equal(t, []string{"completion", "tools"}, flm.Capabilities, "chat -> completion first, tool-calling -> tools")
	assert.Empty(t, flm.Format, "an FLM checkpoint names no weights format")
	assert.Empty(t, flm.Quantization)
	assert.Nil(t, flm.Downloaded, "downloads only, like ollama")

	gguf := byName["Qwen3-0.6B-GGUF"]
	assert.Equal(t, "gguf", gguf.Format)
	assert.Equal(t, "Q4_0", gguf.Quantization)
	assert.Equal(t, "llamacpp", gguf.Runtime)
	assert.Equal(t, []string{"completion", "thinking"}, gguf.Capabilities, "a llama.cpp model without a deployment label is a chat model; reasoning -> thinking")

	embed := byName["nomic-embed-text-v1-GGUF"]
	assert.Equal(t, []string{"embedding"}, embed.Capabilities, "an embeddings model is not a completion model")
	assert.Equal(t, "Q4_K_S", embed.Quantization)

	m, err := b.GetModel(ctx, "qwen3-it-4b-flm")
	require.NoError(t, err, "a reference that matches one id case-insensitively is accepted")
	assert.Equal(t, "qwen3-it-4b-FLM", m.Name, "the canonical id is returned")

	_, err = b.GetModel(ctx, "nope-1b")
	require.ErrorIs(t, err, backend.ErrNotFound)
	_, err = b.GetModel(ctx, "")
	require.ErrorIs(t, err, backend.ErrInvalid)
}

func TestListModelsSkipsCatalogEntries(t *testing.T) {
	f, b := newTestBackend(t)
	f.add("qwen3-it-4b-FLM", 3.1)
	no := false
	f.mu.Lock()
	f.models["Gemma-3-4b-it-GGUF"] = apiModel{ID: "Gemma-3-4b-it-GGUF", Recipe: "llamacpp", SizeGB: 3.6, Downloaded: &no}
	f.mu.Unlock()
	models, err := b.ListModels(context.Background())
	require.NoError(t, err)
	require.Len(t, models, 1, "a catalog entry that is not downloaded is not a model here")
	_, err = b.GetModel(context.Background(), "Gemma-3-4b-it-GGUF")
	require.ErrorIs(t, err, backend.ErrNotFound)
}

func TestPullAggregatesProgress(t *testing.T) {
	f, b := newTestBackend(t)
	ctx := context.Background()
	var samples []backend.Progress
	err := b.Pull(ctx, backend.PullRequest{Ref: "Qwen3-0.6B-GGUF"}, func(p backend.Progress) { samples = append(samples, p) })
	require.NoError(t, err)
	require.Len(t, samples, 4, "three progress events and the completion")
	var lastCompleted int64
	for _, s := range samples {
		assert.GreaterOrEqual(t, s.BytesCompleted, lastCompleted, "progress must not go backwards: %+v", samples)
		lastCompleted = s.BytesCompleted
		assert.Equal(t, int64(400), s.BytesTotal, "the whole download, not the current file")
	}
	assert.Equal(t, "downloading model.gguf (1/2)", samples[0].Status)
	assert.Equal(t, int64(100), samples[0].BytesCompleted)
	assert.Equal(t, "downloading config.json (2/2)", samples[2].Status)
	assert.Equal(t, int64(350), samples[2].BytesCompleted)
	last := samples[len(samples)-1]
	assert.Equal(t, "success", last.Status)
	assert.Equal(t, int64(400), last.BytesCompleted)

	m, err := b.GetModel(ctx, "Qwen3-0.6B-GGUF")
	require.NoError(t, err)
	assert.Equal(t, int64(400_000_000), m.SizeBytes)
	_ = f
}

func TestPullWithoutStreamSupport(t *testing.T) {
	f, b := newTestBackend(t)
	f.pullPlain = true
	var samples []backend.Progress
	err := b.Pull(context.Background(), backend.PullRequest{Ref: "Qwen3-0.6B-GGUF"}, func(p backend.Progress) { samples = append(samples, p) })
	require.NoError(t, err, "a server answering one JSON document instead of a stream still completes the pull")
	require.Len(t, samples, 1)
	assert.Equal(t, "success", samples[0].Status)
}

func TestPullErrors(t *testing.T) {
	f, b := newTestBackend(t)
	err := b.Pull(context.Background(), backend.PullRequest{Ref: "nope-1b"}, nil)
	require.ErrorIs(t, err, backend.ErrNotFound, "a name that is not in the catalog cannot be installed: %v", err)
	assert.Contains(t, err.Error(), "user")

	f.pullError, f.pullCode = "Failed to download model: connection reset", "download_failed"
	err = b.Pull(context.Background(), backend.PullRequest{Ref: "Qwen3-0.6B-GGUF"}, nil)
	require.Error(t, err)
	assert.NotErrorIs(t, err, backend.ErrNotFound)
	var apiErr *APIError
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, "download_failed", apiErr.Code)

	err = b.Pull(context.Background(), backend.PullRequest{Ref: " "}, nil)
	require.ErrorIs(t, err, backend.ErrInvalid)
}

func TestLoadUnloadAndListLoaded(t *testing.T) {
	f, b := newTestBackend(t)
	f.add("qwen3-it-4b-FLM", 3.1)
	ctx := context.Background()

	require.NoError(t, b.Load(ctx, backend.LoadRequest{Name: "qwen3-it-4b-FLM", KeepAlive: "5m"}))
	loaded, err := b.ListLoaded(ctx)
	require.NoError(t, err)
	require.Len(t, loaded, 1)
	assert.Equal(t, "qwen3-it-4b-FLM", loaded[0].Name)
	assert.Equal(t, int64(3_100_000_000), loaded[0].SizeBytes, "the catalog size; Lemonade reports no per-model memory")
	assert.Equal(t, int64(16384), loaded[0].ContextLength)
	assert.Equal(t, "loaded", loaded[0].Status)
	assert.Equal(t, "npu", loaded[0].Device)
	assert.Equal(t, "172.21.0.1", loaded[0].Node)
	assert.False(t, loaded[0].Pinned, "a keep-alive duration means nothing to Lemonade")
	assert.Nil(t, loaded[0].ExpiresAt, "no idle timer")

	require.NoError(t, b.Load(ctx, backend.LoadRequest{Name: "qwen3-it-4b-FLM", KeepAlive: KeepAliveForever}))
	loaded, err = b.ListLoaded(ctx)
	require.NoError(t, err)
	require.Len(t, loaded, 1)
	assert.True(t, loaded[0].Pinned, "-1 pins the model against slot eviction")

	require.NoError(t, b.Unload(ctx, "qwen3-it-4b-FLM"))
	loaded, err = b.ListLoaded(ctx)
	require.NoError(t, err)
	assert.Empty(t, loaded)
	require.NoError(t, b.Unload(ctx, "qwen3-it-4b-FLM"), "unloading a model that is not loaded is a no-op")

	err = b.Load(ctx, backend.LoadRequest{Name: "missing-1b"})
	require.ErrorIs(t, err, backend.ErrNotFound)
	err = b.Load(ctx, backend.LoadRequest{Name: ""})
	require.ErrorIs(t, err, backend.ErrInvalid)
}

func TestListLoadedWhileStarting(t *testing.T) {
	f, b := newTestBackend(t)
	f.add("qwen3-it-4b-FLM", 3.1)
	f.starting["qwen3-it-4b-FLM"] = true
	ctx := context.Background()
	require.NoError(t, b.Load(ctx, backend.LoadRequest{Name: "qwen3-it-4b-FLM"}))
	loaded, err := b.ListLoaded(ctx)
	require.NoError(t, err)
	require.Len(t, loaded, 1)
	assert.Equal(t, "loading", loaded[0].Status, "Lemonade's own state until the backend process answers")
}

func TestDelete(t *testing.T) {
	f, b := newTestBackend(t)
	f.add("qwen3-it-4b-FLM", 3.1)
	ctx := context.Background()
	require.NoError(t, b.Load(ctx, backend.LoadRequest{Name: "qwen3-it-4b-FLM"}))
	require.NoError(t, b.Delete(ctx, "qwen3-it-4b-FLM"))
	_, err := b.GetModel(ctx, "qwen3-it-4b-FLM")
	require.ErrorIs(t, err, backend.ErrNotFound)
	loaded, err := b.ListLoaded(ctx)
	require.NoError(t, err)
	assert.Empty(t, loaded, "Lemonade unloads before deleting")
	err = b.Delete(ctx, "qwen3-it-4b-FLM")
	require.ErrorIs(t, err, backend.ErrNotFound, "422 'Model not found' is a not-found")
	err = b.Delete(ctx, "")
	require.ErrorIs(t, err, backend.ErrInvalid)
}

func TestCapabilitiesOf(t *testing.T) {
	cases := []struct {
		labels []string
		recipe string
		want   []string
	}{
		{[]string{"tool-calling", "chat"}, "flm", []string{"completion", "tools"}},
		{[]string{"vision", "tool-calling", "chat"}, "flm", []string{"completion", "vision", "tools"}},
		{[]string{"reasoning", "hot"}, "llamacpp", []string{"completion", "thinking"}},
		{[]string{"embeddings"}, "llamacpp", []string{"embedding"}},
		{[]string{"reranking"}, "llamacpp", []string{"reranking"}},
		{[]string{"vision"}, "sd-cpp", []string{"vision"}},
		{[]string{"image", "edit"}, "sd-cpp", []string{"image", "edit"}},
		{nil, "whispercpp", nil},
		{nil, "vllm", []string{"completion"}},
		{[]string{"chat", "coding", "experimental"}, "llamacpp", []string{"completion", "coding"}},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, capabilitiesOf(tc.labels, tc.recipe), "labels %v recipe %s", tc.labels, tc.recipe)
	}
}

func TestCheckpointDerivations(t *testing.T) {
	assert.Equal(t, "gguf", formatOf("unsloth/Qwen3-0.6B-GGUF:Q4_0"))
	assert.Equal(t, "Q4_0", quantizationOf("unsloth/Qwen3-0.6B-GGUF:Q4_0"))
	assert.Equal(t, "Q4_K_M", quantizationOf("ggml-org/gemma-3-4b-it-GGUF:Q4_K_M"))
	assert.Equal(t, "UD-Q4_K_XL", quantizationOf("unsloth/Qwen3-30B-A3B-GGUF:UD-Q4_K_XL"))
	assert.Equal(t, "", quantizationOf("Serveurperso/ACE-Step-1.5-GGUF:acestep-v15-xl-sft-Q8_0.gguf"), "a file name is not a quantization")
	assert.Equal(t, "safetensors", formatOf("stabilityai/sd-turbo:sd_turbo.safetensors"))
	assert.Equal(t, "", quantizationOf("stabilityai/sd-turbo:sd_turbo.safetensors"))
	assert.Equal(t, "", formatOf("qwen3-it:4b"), "an FLM checkpoint")
	assert.Equal(t, "", quantizationOf("qwen3-it:4b"), "no repository, no variant")
	assert.Equal(t, "onnx", formatOf("amd/Llama-3.2-1B-Instruct-awq-g128-int4-asym-fp16-onnx-hybrid"))
	assert.Equal(t, int64(0), gbToBytes(0))
	assert.Equal(t, int64(380_000_000), gbToBytes(0.38))
}

func TestMapErr(t *testing.T) {
	nf := mapErr(&APIError{Status: http.StatusNotFound, Code: "model_not_found", Message: "Model 'x' was not found"}, "x")
	require.ErrorIs(t, nf, backend.ErrNotFound)
	require.ErrorIs(t, mapErr(&APIError{Status: http.StatusUnprocessableEntity, Message: "Model not found: x"}, "x"), backend.ErrNotFound)
	require.ErrorIs(t, mapErr(&APIError{Status: http.StatusOK, Code: "unknown_model", Message: "must include the user namespace"}, "x"), backend.ErrNotFound)
	require.ErrorIs(t, mapErr(&APIError{Status: http.StatusBadRequest, Message: "recipe 'llamacpp' cannot serve 'classification'"}, "x"), backend.ErrInvalid)
	other := mapErr(&APIError{Status: http.StatusInternalServerError, Message: "backend crashed"}, "x")
	assert.NotErrorIs(t, other, backend.ErrNotFound)
	assert.NotErrorIs(t, other, backend.ErrInvalid)
	require.NoError(t, mapErr(nil, "x"))
	plain := fmt.Errorf("dial tcp: connection refused")
	assert.Equal(t, plain, mapErr(plain, "x"))
}

func TestParseErrorBody(t *testing.T) {
	code, msg := parseErrorBody([]byte(`{"error":"Model not loaded: x"}`))
	assert.Equal(t, "", code)
	assert.Equal(t, "Model not loaded: x", msg)
	code, msg = parseErrorBody([]byte(`{"error":{"code":"model_not_found","message":"Model 'x' was not found.","type":"model_not_found"}}`))
	assert.Equal(t, "model_not_found", code)
	assert.Equal(t, "Model 'x' was not found.", msg)
	code, msg = parseErrorBody([]byte(`{"error":{"message":"Model x has not been found","type":"not_found"}}`))
	assert.Equal(t, "not_found", code, "type stands in for a missing code")
	assert.Equal(t, "Model x has not been found", msg)
	code, msg = parseErrorBody([]byte(`{"status":"error","message":"Failed to load"}`))
	assert.Equal(t, "", code)
	assert.Equal(t, "Failed to load", msg)
	code, msg = parseErrorBody([]byte(`{"code":"unknown_model","error":"must include the user namespace"}`))
	assert.Equal(t, "unknown_model", code)
	assert.Equal(t, "must include the user namespace", msg)
	_, msg = parseErrorBody([]byte(`not json`))
	assert.Equal(t, "", msg)
}
