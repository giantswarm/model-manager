package ollama

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/model-manager/internal/backend"
)

// fakeOllama is an httptest stand-in for the Ollama management API.
type fakeOllama struct {
	mu        sync.Mutex
	models    map[string]apiModel
	loaded    map[string]bool
	keepAlive map[string]any
	pullError string
	embedOnly map[string]bool
	// cpuOnly makes /api/ps report size_vram 0 (no accelerator in use).
	cpuOnly bool
	srv     *httptest.Server
}

func newFakeOllama(t *testing.T) *fakeOllama {
	t.Helper()
	f := &fakeOllama{
		models:    map[string]apiModel{},
		loaded:    map[string]bool{},
		keepAlive: map[string]any{},
		embedOnly: map[string]bool{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/version", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"version": "0.33.2"})
	})
	mux.HandleFunc("GET /api/tags", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		out := modelsResponse{Models: []apiModel{}}
		for _, m := range f.models {
			out.Models = append(out.Models, m)
		}
		_ = json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("GET /api/ps", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		out := modelsResponse{Models: []apiModel{}}
		for name := range f.loaded {
			m := f.models[name]
			if !f.cpuOnly {
				m.SizeVRAM = m.Size
			}
			exp := time.Now().Add(5 * time.Minute)
			m.ExpiresAt = &exp
			out.Models = append(out.Models, m)
		}
		_ = json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("POST /api/pull", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		enc := json.NewEncoder(w)
		w.Header().Set("Content-Type", "application/x-ndjson")
		if f.pullError != "" {
			_ = enc.Encode(pullEvent{Error: f.pullError})
			return
		}
		_ = enc.Encode(pullEvent{Status: "pulling manifest"})
		_ = enc.Encode(pullEvent{Status: "pulling aaa", Digest: "sha256:aaa", Total: 100})
		_ = enc.Encode(pullEvent{Status: "pulling aaa", Digest: "sha256:aaa", Total: 100, Completed: 50})
		_ = enc.Encode(pullEvent{Status: "pulling bbb", Digest: "sha256:bbb", Total: 300, Completed: 0})
		_ = enc.Encode(pullEvent{Status: "pulling aaa", Digest: "sha256:aaa", Total: 100, Completed: 100})
		_ = enc.Encode(pullEvent{Status: "pulling bbb", Digest: "sha256:bbb", Total: 300, Completed: 300})
		_ = enc.Encode(pullEvent{Status: "verifying sha256 digest"})
		_ = enc.Encode(pullEvent{Status: "writing manifest"})
		f.mu.Lock()
		name := canonical(req.Model)
		f.models[name] = apiModel{Name: name, Model: name, Size: 400, Digest: "sha256:deadbeef", ModifiedAt: time.Now(),
			Details: apiDetails{Format: "gguf", Family: "llama", ParameterSize: "134.52M", QuantizationLevel: "Q8_0"}, Capabilities: []string{"completion"}}
		f.mu.Unlock()
		_ = enc.Encode(pullEvent{Status: "success"})
	})
	mux.HandleFunc("DELETE /api/delete", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		defer f.mu.Unlock()
		name := canonical(req.Model)
		if _, ok := f.models[name]; !ok {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": fmt.Sprintf("model '%s' not found", req.Model)})
			return
		}
		delete(f.models, name)
		delete(f.loaded, name)
		w.WriteHeader(http.StatusOK)
	})
	keepAliveHandler := func(embed bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			var req struct {
				Model     string `json:"model"`
				KeepAlive any    `json:"keep_alive"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			f.mu.Lock()
			defer f.mu.Unlock()
			name := canonical(req.Model)
			if _, ok := f.models[name]; !ok {
				w.WriteHeader(http.StatusNotFound)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": fmt.Sprintf("model '%s' not found", req.Model)})
				return
			}
			if f.embedOnly[name] && !embed {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": fmt.Sprintf("%q does not support generate; use embed", req.Model)})
				return
			}
			f.keepAlive[name] = req.KeepAlive
			if n, ok := req.KeepAlive.(float64); ok && n == 0 {
				delete(f.loaded, name)
				_ = json.NewEncoder(w).Encode(map[string]any{"model": name, "done": true, "done_reason": "unload"})
				return
			}
			f.loaded[name] = true
			_ = json.NewEncoder(w).Encode(map[string]any{"model": name, "done": true, "done_reason": "load"})
		}
	}
	mux.HandleFunc("POST /api/generate", keepAliveHandler(false))
	mux.HandleFunc("POST /api/embed", keepAliveHandler(true))
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeOllama) add(name string, size int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.models[name] = apiModel{Name: name, Model: name, Size: size, Digest: "sha256:" + name, ModifiedAt: time.Now(),
		Details:      apiDetails{Format: "gguf", Family: "qwen3", ParameterSize: "751.63M", QuantizationLevel: "Q4_K_M", ContextLength: 40960},
		Capabilities: []string{"completion", "tools"}}
}

func newTestBackend(t *testing.T) (*fakeOllama, *Backend) {
	t.Helper()
	f := newFakeOllama(t)
	b, err := New(backend.OllamaOptions{Endpoint: f.srv.URL, AgentHost: "http://172.21.0.1:11434"})
	require.NoError(t, err)
	return f, b
}

func TestNewValidatesEndpoint(t *testing.T) {
	_, err := New(backend.OllamaOptions{Endpoint: ""})
	require.Error(t, err)
	_, err = New(backend.OllamaOptions{Endpoint: "172.21.0.1:11434"})
	require.Error(t, err)
	b, err := New(backend.OllamaOptions{Endpoint: "http://172.21.0.1:11434/"})
	require.NoError(t, err)
	assert.Equal(t, "http://172.21.0.1:11434", b.AgentEndpoint("x").Host, "agent host defaults to the endpoint")
	assert.Equal(t, "Ollama", b.AgentEndpoint("x").Provider)
}

func TestInfoAndCapabilities(t *testing.T) {
	_, b := newTestBackend(t)
	info := b.Info(context.Background())
	assert.True(t, info.Healthy)
	assert.Equal(t, "0.33.2", info.Version)
	assert.Equal(t, backend.NameOllama, info.Backend)
	assert.Equal(t, "http://172.21.0.1:11434", info.AgentEndpoint, "the host agents dial, as written into ModelConfigs")
	assert.NotEqual(t, info.Endpoint, info.AgentEndpoint, "model-manager and agents dial different addresses here")
	assert.Equal(t, backend.Loading{OnDemand: true, IdleEviction: true, KeepAliveDefault: DefaultKeepAlive, KeepAliveScope: backend.KeepAliveScopeRequest}, info.Loading,
		"ollama loads on the first request, evicts idle models, and every request re-arms the keep-alive")
	caps := b.Capabilities()
	assert.True(t, caps.Pull && caps.PullProgress && caps.Delete && caps.Load && caps.Unload && caps.LoadedModels)
	assert.True(t, caps.NodeInventory, "the proxied host is reported as a node")
	assert.False(t, caps.Presets || caps.FitCheck || caps.Search, "kserve-only flags stay off")
	assert.False(t, caps.Wire, "wire is decided by the service")
}

func TestInfoUnhealthy(t *testing.T) {
	b, err := New(backend.OllamaOptions{Endpoint: "http://127.0.0.1:1", Timeout: time.Second})
	require.NoError(t, err)
	info := b.Info(context.Background())
	assert.False(t, info.Healthy)
	assert.NotEmpty(t, info.Message)
	assert.Equal(t, "http://127.0.0.1:1", info.AgentEndpoint, "agent endpoint defaults to the endpoint and is reported even when Ollama is down")
	assert.True(t, info.Loading.OnDemand && info.Loading.IdleEviction, "load semantics are a property of the driver, not of its health")
}

func TestListAndGetModels(t *testing.T) {
	f, b := newTestBackend(t)
	f.add("qwen3:0.6b", 522653767)
	f.add("gemma3:latest", 3338801804)
	ctx := context.Background()

	models, err := b.ListModels(ctx)
	require.NoError(t, err)
	require.Len(t, models, 2)
	byName := map[string]backend.Model{}
	for _, m := range models {
		byName[m.Name] = m
	}
	assert.Equal(t, int64(522653767), byName["qwen3:0.6b"].SizeBytes)
	assert.Equal(t, "qwen3", byName["qwen3:0.6b"].Family)
	assert.Equal(t, "Q4_K_M", byName["qwen3:0.6b"].Quantization)
	assert.Equal(t, int64(40960), byName["qwen3:0.6b"].ContextLength)
	assert.Equal(t, []string{"completion", "tools"}, byName["qwen3:0.6b"].Capabilities)

	m, err := b.GetModel(ctx, "gemma3")
	require.NoError(t, err, "a missing tag means :latest")
	assert.Equal(t, "gemma3:latest", m.Name)

	_, err = b.GetModel(ctx, "nope:1b")
	require.ErrorIs(t, err, backend.ErrNotFound)
	_, err = b.GetModel(ctx, "")
	require.ErrorIs(t, err, backend.ErrInvalid)
}

func TestPullAggregatesLayerProgress(t *testing.T) {
	f, b := newTestBackend(t)
	ctx := context.Background()
	var samples []backend.Progress
	err := b.Pull(ctx, backend.PullRequest{Ref: "smollm2:135m"}, func(p backend.Progress) { samples = append(samples, p) })
	require.NoError(t, err)
	require.NotEmpty(t, samples)
	// Totals are the sum over layers, completed climbs monotonically to total.
	var maxTotal, lastCompleted int64
	for _, s := range samples {
		assert.GreaterOrEqual(t, s.BytesCompleted, lastCompleted, "progress must not go backwards: %+v", samples)
		lastCompleted = s.BytesCompleted
		if s.BytesTotal > maxTotal {
			maxTotal = s.BytesTotal
		}
	}
	assert.Equal(t, int64(400), maxTotal)
	last := samples[len(samples)-1]
	assert.Equal(t, "success", last.Status)
	assert.Equal(t, int64(400), last.BytesCompleted)

	m, err := b.GetModel(ctx, "smollm2:135m")
	require.NoError(t, err)
	assert.Equal(t, int64(400), m.SizeBytes)
	_ = f
}

func TestPullErrors(t *testing.T) {
	f, b := newTestBackend(t)
	f.pullError = "pull model manifest: file does not exist"
	err := b.Pull(context.Background(), backend.PullRequest{Ref: "nope:1b"}, nil)
	require.ErrorIs(t, err, backend.ErrNotFound)

	err = b.Pull(context.Background(), backend.PullRequest{Ref: " "}, nil)
	require.ErrorIs(t, err, backend.ErrInvalid)
}

func TestLoadUnloadAndListLoaded(t *testing.T) {
	f, b := newTestBackend(t)
	f.add("qwen3:0.6b", 10)
	ctx := context.Background()

	require.NoError(t, b.Load(ctx, backend.LoadRequest{Name: "qwen3:0.6b", KeepAlive: "10m"}))
	assert.Equal(t, "10m", f.keepAlive["qwen3:0.6b"])
	loaded, err := b.ListLoaded(ctx)
	require.NoError(t, err)
	require.Len(t, loaded, 1)
	assert.Equal(t, "qwen3:0.6b", loaded[0].Name)
	assert.NotNil(t, loaded[0].ExpiresAt)
	assert.Equal(t, int64(10), loaded[0].VRAMBytes)

	require.NoError(t, b.Load(ctx, backend.LoadRequest{Name: "qwen3:0.6b"}))
	assert.Equal(t, DefaultKeepAlive, f.keepAlive["qwen3:0.6b"], "empty keepAlive falls back to the default")

	err = b.Load(ctx, backend.LoadRequest{Name: "qwen3:0.6b", KeepAlive: "0"})
	require.ErrorIs(t, err, backend.ErrInvalid, "keepAlive 0 is an unload, not a load")

	require.NoError(t, b.Unload(ctx, "qwen3:0.6b"))
	assert.EqualValues(t, 0, f.keepAlive["qwen3:0.6b"])
	loaded, err = b.ListLoaded(ctx)
	require.NoError(t, err)
	assert.Empty(t, loaded)

	err = b.Load(ctx, backend.LoadRequest{Name: "missing:1b"})
	require.ErrorIs(t, err, backend.ErrNotFound)
}

func TestLoadEmbeddingModelFallsBackToEmbed(t *testing.T) {
	f, b := newTestBackend(t)
	f.add("nomic-embed-text:latest", 10)
	f.embedOnly["nomic-embed-text:latest"] = true
	require.NoError(t, b.Load(context.Background(), backend.LoadRequest{Name: "nomic-embed-text"}))
	assert.True(t, f.loaded["nomic-embed-text:latest"])
}

func TestDelete(t *testing.T) {
	f, b := newTestBackend(t)
	f.add("qwen3:0.6b", 10)
	ctx := context.Background()
	require.NoError(t, b.Delete(ctx, "qwen3:0.6b"))
	_, err := b.GetModel(ctx, "qwen3:0.6b")
	require.ErrorIs(t, err, backend.ErrNotFound)
	err = b.Delete(ctx, "qwen3:0.6b")
	require.ErrorIs(t, err, backend.ErrNotFound)
	var apiErr *APIError
	require.True(t, errors.As(err, &apiErr) || errors.Is(err, backend.ErrNotFound))
}

func TestCanonical(t *testing.T) {
	assert.Equal(t, "gemma3:latest", canonical("gemma3"))
	assert.Equal(t, "gemma3:4b", canonical("gemma3:4b"))
	assert.Equal(t, "hf.co/org/repo:Q4_K_M", canonical("hf.co/org/repo:Q4_K_M"))
	assert.Equal(t, "hf.co/org/repo:latest", canonical("hf.co/org/repo"))
	assert.True(t, sameRef("gemma3", "gemma3:latest"))
	assert.False(t, sameRef("gemma3:4b", "gemma3:latest"))
}
