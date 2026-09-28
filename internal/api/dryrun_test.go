package api

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/model-manager/internal/backend"
	"github.com/giantswarm/model-manager/internal/buildinfo"
	"github.com/giantswarm/model-manager/internal/jobs"
	"github.com/giantswarm/model-manager/internal/service"
)

func TestWireAndUnwireDryRunWriteNothing(t *testing.T) {
	fb := newFakeBackend()
	fb.models["qwen3:0.6b"] = backend.Model{Name: "qwen3:0.6b"}
	fw := newFakeWirer()
	svc := service.New([]backend.Backend{fb}, jobs.NewManager(), fw, &service.WiringInfo{Namespace: "kagent"}, service.Config{}, nil)
	srv := NewMCPServer(svc, buildinfo.Info{Version: "test"})

	text, isErr := callTool(t, srv, ToolWireModel, map[string]any{"model": "qwen3:0.6b", "dryRun": true})
	require.False(t, isErr, text)
	var plan map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &plan))
	assert.Equal(t, true, plan["dryRun"])
	assert.Equal(t, "qwen3-0-6b", plan["modelConfig"])
	manifests, _ := plan["manifests"].([]any)
	require.Len(t, manifests, 1)
	assert.Equal(t, "ModelConfig", manifests[0].(map[string]any)["kind"])
	assert.Zero(t, fw.count(), "the dry run wrote no ModelConfig")

	text, isErr = callTool(t, srv, ToolUnwireModel, map[string]any{"model": "qwen3:0.6b", "dryRun": true})
	require.False(t, isErr, text)
	require.NoError(t, json.Unmarshal([]byte(text), &plan))
	assert.Empty(t, plan["manifests"], "nothing wired, nothing to remove")

	text, isErr = callTool(t, srv, ToolWireModel, map[string]any{"model": "qwen3:0.6b"})
	require.False(t, isErr, text)
	text, isErr = callTool(t, srv, ToolUnwireModel, map[string]any{"model": "qwen3:0.6b", "dryRun": true})
	require.False(t, isErr, text)
	require.NoError(t, json.Unmarshal([]byte(text), &plan))
	assert.Len(t, plan["manifests"], 1)
	assert.Equal(t, 1, fw.count(), "the unwire dry run removed nothing")
}

func TestGitOpsOwnedAnswersItsOwnCode(t *testing.T) {
	status, code := statusFor(fmt.Errorf("%w: ModelConfig kagent/x is applied from git", backend.ErrGitOpsOwned))
	assert.Equal(t, 409, status)
	assert.Equal(t, "gitops_owned", code)
}
