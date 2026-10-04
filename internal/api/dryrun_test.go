package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/giantswarm/gitops-commit/commit"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/giantswarm/model-manager/internal/backend"
	"github.com/giantswarm/model-manager/internal/buildinfo"
	"github.com/giantswarm/model-manager/internal/gitops"
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

func TestCommitModeRefusals(t *testing.T) {
	fb := newFakeBackend()
	fb.models["qwen3:0.6b"] = backend.Model{Name: "qwen3:0.6b"}
	svc := service.New([]backend.Backend{fb}, jobs.NewManager(), newFakeWirer(), &service.WiringInfo{Namespace: "kagent"}, service.Config{}, nil)
	srv := NewMCPServer(svc, buildinfo.Info{Version: "test"})

	text, isErr := callTool(t, srv, ToolWireModel, map[string]any{"model": "qwen3:0.6b", "mode": "commit"})
	require.True(t, isErr)
	assert.Contains(t, text, "unsupported: ")
	assert.Contains(t, text, "github.enabled")

	text, isErr = callTool(t, srv, ToolWireModel, map[string]any{"model": "qwen3:0.6b", "mode": "live"})
	require.True(t, isErr)
	assert.Contains(t, text, "invalid_request")

	text, _ = callTool(t, srv, ToolGetBackend, nil)
	assert.Contains(t, text, `"commit": false`)

	svc.WithCommitter(gitops.NewCommitter("", func(string) (gitops.Remote, error) { return gitops.FakeRemote{Fake: commit.NewFake()}, nil }, func(context.Context) dynamic.Interface { return nil }))
	text, _ = callTool(t, srv, ToolGetBackend, nil)
	assert.Contains(t, text, `"commit": true`)

	text, isErr = callTool(t, srv, ToolUnwireModel, map[string]any{"model": "qwen3:0.6b", "mode": "commit"})
	require.False(t, isErr, text)
	assert.Contains(t, text, "nothing is wired: nothing to commit")

	text, isErr = callTool(t, srv, ToolWireModel, map[string]any{"model": "qwen3:0.6b", "mode": "commit", "dryRun": true})
	require.True(t, isErr)
	assert.Contains(t, text, "auth_required: ")
	assert.Contains(t, text, "core_auth_login server=model-manager")
}

func TestOperationalDryRunsDoNothing(t *testing.T) {
	fb := newFakeBackend()
	fb.models["qwen3:0.6b"] = backend.Model{Name: "qwen3:0.6b"}
	fw := newFakeWirer()
	svc := service.New([]backend.Backend{fb}, jobs.NewManager(), fw, &service.WiringInfo{Namespace: "kagent"}, service.Config{AutoWire: true}, nil)
	srv := NewMCPServer(svc, buildinfo.Info{Version: "test"})

	for tool, args := range map[string]map[string]any{
		ToolPullModel:   {"model": "smollm2:135m", "dryRun": true},
		ToolLoadModel:   {"model": "qwen3:0.6b", "dryRun": true},
		ToolUnloadModel: {"model": "qwen3:0.6b", "dryRun": true},
		ToolDeleteModel: {"model": "qwen3:0.6b", "dryRun": true},
	} {
		text, isErr := callTool(t, srv, tool, args)
		require.False(t, isErr, "%s: %s", tool, text)
		var plan map[string]any
		require.NoError(t, json.Unmarshal([]byte(text), &plan))
		assert.Equal(t, true, plan["dryRun"], tool)
	}
	assert.Contains(t, fb.models, "qwen3:0.6b", "the delete dry run deleted nothing")
	assert.NotContains(t, fb.models, "smollm2:135m", "the pull dry run pulled nothing")
	assert.Empty(t, fb.loaded, "the load dry run loaded nothing")
	assert.Zero(t, fw.count(), "no dry run wired anything")

	text, _ := callTool(t, srv, ToolLoadModel, map[string]any{"model": "qwen3:0.6b", "dryRun": true})
	assert.Contains(t, text, `"wiring"`, "the load plan names the ModelConfig the auto-wire would ensure")

	for _, tool := range []string{ToolPullModel, ToolDeleteModel} {
		text, isErr := callTool(t, srv, tool, map[string]any{"model": "qwen3:0.6b", "mode": "commit"})
		require.True(t, isErr, tool)
		assert.Contains(t, text, "unsupported: ", tool)
	}
	svc.WithCommitter(gitops.NewCommitter("", func(string) (gitops.Remote, error) { return gitops.FakeRemote{Fake: commit.NewFake()}, nil }, func(context.Context) dynamic.Interface { return nil }))
	text, isErr := callTool(t, srv, ToolLoadModel, map[string]any{"model": "qwen3:0.6b", "mode": "commit"})
	require.True(t, isErr)
	assert.Contains(t, text, "only kserve")
}

func TestForbiddenAnswers403(t *testing.T) {
	denied := apierrors.NewForbidden(schema.GroupResource{Group: "kagent.dev", Resource: "modelconfigs"}, "", fmt.Errorf("RBAC: access denied"))
	status, code := statusFor(fmt.Errorf("list ModelConfigs in kagent: %w", denied))
	assert.Equal(t, http.StatusForbidden, status)
	assert.Equal(t, "forbidden", code)

	status, code = statusFor(fmt.Errorf("list ModelConfigs in kagent: %w", apierrors.NewInternalError(fmt.Errorf("etcd"))))
	assert.Equal(t, http.StatusBadGateway, status, "any other apiserver failure stays a backend error")
	assert.Equal(t, "backend_error", code)
}
