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

func ptr[T any](v T) *T { return &v }

// thinkingModels are a model with Ollama's thinking capability and one
// without, as /api/tags reports them.
func thinkingModels(fb *fakeBackend) {
	fb.models["qwen3.5:2b"] = backend.Model{Name: "qwen3.5:2b", SizeBytes: 10, ContextLength: 262144, Capabilities: []string{"completion", "vision", "tools", "thinking"}}
	fb.models["smollm2:135m"] = backend.Model{Name: "smollm2:135m", SizeBytes: 10, ContextLength: 8192, Capabilities: []string{"completion"}}
}

// TestWiredThinkOnlyForThinkingModels: the ModelConfig of a model with the
// thinking capability carries the backend's configured think, reported as
// modelConfig.think; a model without the capability gets none, since Ollama
// refuses think true for it; no think configured writes none.
func TestWiredThinkOnlyForThinkingModels(t *testing.T) {
	f := newFixture(t, true)
	thinkingModels(f.backend)
	wire := func(model string) map[string]any {
		t.Helper()
		status, body := f.do(t, http.MethodPost, Prefix+"/models/wire", map[string]any{"model": model})
		require.Equal(t, http.StatusOK, status, body)
		return body["modelConfig"].(map[string]any)
	}

	for _, think := range []bool{false, true} {
		f.backend.think = ptr(think)
		assert.Equal(t, think, wire("qwen3.5:2b")["think"], "a thinking model runs at the configured think")
		assert.NotContains(t, wire("smollm2:135m"), "think", "a model without the capability gets none")
	}
	_, body := f.do(t, http.MethodGet, Prefix+"/models/qwen3.5:2b", nil)
	assert.Equal(t, true, body["modelConfig"].(map[string]any)["think"], "the model reports the effective think next to the window")

	f.backend.think = nil
	assert.NotContains(t, wire("qwen3.5:2b"), "think", "no think configured: none is written")
}

// TestReconcilerRefreshesThink: a thinking model's ModelConfig written
// without think (before the setting existed) or with a hand-set one is
// re-wired to the configured think; a model without the capability, whose
// ModelConfig carries none, is left alone.
func TestReconcilerRefreshesThink(t *testing.T) {
	fb := newFakeBackend()
	fb.contextLength, fb.think = 32768, ptr(false)
	thinkingModels(fb)
	fb.models["qwen3:0.6b"] = backend.Model{Name: "qwen3:0.6b", SizeBytes: 10, ContextLength: 40960, Capabilities: []string{"completion", "tools", "thinking"}}
	fw := newFakeWirer()
	unset := wiring.ModelConfigRef{Name: "qwen3-5-2b", Namespace: "kagent", Provider: "Ollama", Model: "qwen3.5:2b", Managed: true, Backend: backend.NameOllama, ContextLength: 32768}
	handSet := wiring.ModelConfigRef{Name: "qwen3-0-6b", Namespace: "kagent", Provider: "Ollama", Model: "qwen3:0.6b", Managed: true, Backend: backend.NameOllama, ContextLength: 32768, Think: ptr(true)}
	// Message marks the refs Ensure must not rewrite (it would clear it).
	plain := wiring.ModelConfigRef{Name: "smollm2-135m", Namespace: "kagent", Provider: "Ollama", Model: "smollm2:135m", Managed: true, Backend: backend.NameOllama, ContextLength: 8192, Message: "untouched"}
	for _, r := range []wiring.ModelConfigRef{unset, handSet, plain} {
		fw.refs[refKey(r.Backend, r.Model)] = r
	}
	stop := runService(t, fb, fw)

	thinkOff := func(model string) func() bool {
		return func() bool {
			r, _ := fw.get(backend.NameOllama, model)
			return r.Think != nil && !*r.Think
		}
	}
	require.Eventually(t, thinkOff("qwen3.5:2b"), 5*time.Second, 5*time.Millisecond, "a ModelConfig without think is re-wired to the configured one")
	require.Eventually(t, thinkOff("qwen3:0.6b"), 5*time.Second, 5*time.Millisecond, "a hand-set think is re-wired to the configured one")
	stop()
	r, _ := fw.get(backend.NameOllama, "smollm2:135m")
	assert.Equal(t, "untouched", r.Message, "a model without the capability needs no think")
}

// TestReconcilerDoesNotLoopWhereKagentLacksThink: on a kagent whose
// ModelConfig has no spec.ollama.think the field is pruned from every write;
// the reconciler compares against what can be written, so a ModelConfig that
// is otherwise up to date is never re-wired.
func TestReconcilerDoesNotLoopWhereKagentLacksThink(t *testing.T) {
	fb := newFakeBackend()
	fb.contextLength, fb.think = 32768, ptr(false)
	thinkingModels(fb)
	fw := newFakeWirer()
	fw.noThink = true
	current := wiring.ModelConfigRef{Name: "qwen3-5-2b", Namespace: "kagent", Provider: "Ollama", Model: "qwen3.5:2b", Managed: true, Backend: backend.NameOllama, ContextLength: 32768, Message: "untouched"}
	stale := wiring.ModelConfigRef{Name: "smollm2-135m", Namespace: "kagent", Provider: "Ollama", Model: "smollm2:135m", Managed: true, Backend: backend.NameOllama, ContextLength: 4096}
	for _, r := range []wiring.ModelConfigRef{current, stale} {
		fw.refs[refKey(r.Backend, r.Model)] = r
	}
	stop := runService(t, fb, fw)

	require.Eventually(t, func() bool {
		r, _ := fw.get(backend.NameOllama, "smollm2:135m")
		return r.ContextLength == 8192
	}, 5*time.Second, 5*time.Millisecond, "the reconciler runs: a stale window is refreshed")
	time.Sleep(50 * time.Millisecond) // ten more passes
	stop()
	r, _ := fw.get(backend.NameOllama, "qwen3.5:2b")
	assert.Equal(t, "untouched", r.Message, "a ModelConfig that carries what can be written is not re-wired")
	fw.mu.Lock()
	defer fw.mu.Unlock()
	assert.Equal(t, 1, fw.ensures, "one write in all the passes: the stale window's")
}

// runService runs the service's reconciler over fb and fw every 5 ms until
// the returned stop is called (or the test ends).
func runService(t *testing.T, fb *fakeBackend, fw *fakeWirer) (stop func()) {
	t.Helper()
	svc := service.New([]backend.Backend{fb}, jobs.NewManager(), fw, &service.WiringInfo{Namespace: "kagent"}, service.Config{AutoWire: true, ReconcileInterval: 5 * time.Millisecond}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { svc.Run(ctx); close(done) }()
	var stopped bool
	stop = func() {
		if !stopped {
			stopped = true
			cancel()
			<-done
		}
	}
	t.Cleanup(stop)
	return stop
}
