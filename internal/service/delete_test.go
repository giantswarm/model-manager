package service

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/model-manager/internal/backend"
	"github.com/giantswarm/model-manager/internal/gitops"
	"github.com/giantswarm/model-manager/internal/wiring"
)

func TestDeleteRefusal(t *testing.T) {
	require.NoError(t, deleteRefusal("qwen3:0.6b", nil), "nothing wired")
	require.NoError(t, deleteRefusal("qwen3:0.6b", &WirePlan{ModelConfig: "qwen3-0-6b", Namespace: "kagent"}), "one this instance removes")

	err := deleteRefusal("qwen3:0.6b", &WirePlan{
		ModelConfig: "qwen3-0-6b",
		Namespace:   "kagent",
		GitOps:      &gitops.Owner{Kind: gitops.KindKustomization, Namespace: "flux-giantswarm", Name: "models"},
	})
	require.ErrorIs(t, err, backend.ErrGitOpsOwned)
	assert.ErrorContains(t, err, "ModelConfig kagent/qwen3-0-6b is applied from git")
	assert.ErrorContains(t, err, "nothing was deleted")

	left := &wiring.NotOwnedError{Namespace: "kagent", Name: "qwen3-0-6b", Message: "ModelConfig kagent/qwen3-0-6b was not created by this model-manager"}
	err = deleteRefusal("qwen3:0.6b", &WirePlan{ModelConfig: "qwen3-0-6b", Namespace: "kagent", Left: left})
	require.ErrorIs(t, err, backend.ErrConflict)
	var notOwned *wiring.NotOwnedError
	require.ErrorAs(t, err, &notOwned)
	assert.ErrorContains(t, err, "nothing was deleted")
}
