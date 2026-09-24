package backend

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestFitTo: the window is capped at the model's own context length, and
// think is kept only for a model with the thinking capability — Ollama
// refuses think true for any other model.
func TestFitTo(t *testing.T) {
	off := false
	ep := AgentEndpoint{Provider: "Ollama", ContextLength: 32768, Think: &off}
	thinking := Model{ContextLength: 262144, Capabilities: []string{"completion", "tools", CapabilityThinking}}
	plain := Model{ContextLength: 8192, Capabilities: []string{"completion"}}

	got := ep.FitTo(thinking)
	assert.Equal(t, int64(32768), got.ContextLength, "a larger model window keeps the configured one")
	assert.Equal(t, &off, got.Think, "a thinking model keeps think")

	got = ep.FitTo(plain)
	assert.Equal(t, int64(8192), got.ContextLength, "capped at the model's own")
	assert.Nil(t, got.Think, "a model without the capability gets none")

	got = ep.FitTo(Model{})
	assert.Equal(t, int64(32768), got.ContextLength, "an unknown model caps nothing")
	assert.Nil(t, got.Think, "an unknown model gets no think")
	assert.Equal(t, &off, ep.Think, "the endpoint itself is not changed")
}
