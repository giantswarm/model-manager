package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/model-manager/internal/backend"
	"github.com/giantswarm/model-manager/internal/jobs"
	"github.com/giantswarm/model-manager/internal/service"
	"github.com/giantswarm/model-manager/internal/wiring"
)

// TestWiredContextLengthIsCappedAtTheModels: the ModelConfig asks for the
// backend's configured window, or the model's own context length when that
// is smaller, and a load pre-warms at the same window, so the first agent
// turn does not reload the model.
func TestWiredContextLengthIsCappedAtTheModels(t *testing.T) {
	f := newFixture(t, true)
	f.backend.contextLength = 32768
	f.backend.models["qwen3:0.6b"] = backend.Model{Name: "qwen3:0.6b", SizeBytes: 10, ContextLength: 40960}
	f.backend.models["smollm2:135m"] = backend.Model{Name: "smollm2:135m", SizeBytes: 10, ContextLength: 8192}
	f.backend.models["unreported:1b"] = backend.Model{Name: "unreported:1b", SizeBytes: 10}

	for model, want := range map[string]float64{"qwen3:0.6b": 32768, "smollm2:135m": 8192, "unreported:1b": 32768} {
		status, body := f.do(t, http.MethodPost, Prefix+"/models/load", map[string]any{"model": model})
		require.Equal(t, http.StatusOK, status, body)
		assert.Equal(t, want, body["modelConfig"].(map[string]any)["contextLength"], "%s: the window agents run at", model)
		assert.Equal(t, int64(want), f.backend.loadContext[model], "%s: loaded at the agents' window", model)
	}
	_, body := f.do(t, http.MethodGet, Prefix+"/models/smollm2:135m", nil)
	assert.Equal(t, float64(8192), body["contextLength"], "the model's own context length stays next to the ModelConfig's")
	assert.Equal(t, float64(8192), body["modelConfig"].(map[string]any)["contextLength"])

	f.backend.contextLength = 0
	status, body := f.do(t, http.MethodPost, Prefix+"/models/wire", map[string]any{"model": "qwen3:0.6b"})
	require.Equal(t, http.StatusOK, status, body)
	assert.Nil(t, body["modelConfig"].(map[string]any)["contextLength"], "no window configured: none is written")
}

// TestReconcilerRefreshesStaleContextLengths: a ModelConfig written before
// the window was configured, or under another value, is re-wired to the
// configured one; an up-to-date one and one whose model is gone are left
// alone.
func TestReconcilerRefreshesStaleContextLengths(t *testing.T) {
	fb := newFakeBackend()
	fb.contextLength = 32768
	fb.models["qwen3:0.6b"] = backend.Model{Name: "qwen3:0.6b", SizeBytes: 10, ContextLength: 40960}
	fb.models["smollm2:135m"] = backend.Model{Name: "smollm2:135m", SizeBytes: 10, ContextLength: 8192}
	fw := newFakeWirer()
	stale := wiring.ModelConfigRef{Name: "qwen3-0-6b", Namespace: "kagent", Provider: "Ollama", Model: "qwen3:0.6b", Managed: true, Backend: backend.NameOllama}
	// Message marks the refs Ensure must not rewrite (it would clear it).
	current := wiring.ModelConfigRef{Name: "smollm2-135m", Namespace: "kagent", Provider: "Ollama", Model: "smollm2:135m", Managed: true, Backend: backend.NameOllama, ContextLength: 8192, Message: "untouched"}
	gone := wiring.ModelConfigRef{Name: "gone-1b", Namespace: "kagent", Provider: "Ollama", Model: "gone:1b", Managed: true, Backend: backend.NameOllama, ContextLength: 4096, Message: "untouched"}
	for _, r := range []wiring.ModelConfigRef{stale, current, gone} {
		fw.refs[refKey(r.Backend, r.Model)] = r
	}
	svc := service.New([]backend.Backend{fb}, jobs.NewManager(), fw, &service.WiringInfo{Namespace: "kagent"}, service.Config{AutoWire: true, ReconcileInterval: 5 * time.Millisecond}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { svc.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	require.Eventually(t, func() bool {
		r, _ := fw.get(backend.NameOllama, "qwen3:0.6b")
		return r.ContextLength == 32768
	}, 5*time.Second, 5*time.Millisecond, "the stale ModelConfig is re-wired to the configured window")
	cancel()
	<-done
	r, _ := fw.get(backend.NameOllama, "smollm2:135m")
	assert.Equal(t, "untouched", r.Message, "an up-to-date ModelConfig is not re-wired")
	r, _ = fw.get(backend.NameOllama, "gone:1b")
	assert.Equal(t, "untouched", r.Message, "a ModelConfig whose model is gone is left to unwire")
}
