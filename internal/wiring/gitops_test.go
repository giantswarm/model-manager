package wiring

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/model-manager/internal/backend"
	"github.com/giantswarm/model-manager/internal/gitops"
)

// committed is a ModelConfig as model-manager commits it — its own labels
// and annotation — applied by Flux, which adds its provenance labels.
func committed(name, model string, flux map[string]any) *unstructured.Unstructured {
	labels := map[string]any{ManagedByLabel: ManagedByValue, BackendLabel: string(backend.NameLemonade)}
	for k, v := range flux {
		labels[k] = v
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": testAPIVersion,
		"kind":       "ModelConfig",
		"metadata":   map[string]any{"name": name, "namespace": "kagent", "labels": labels, "annotations": map[string]any{ModelAnnotation: model}},
		"spec":       map[string]any{"provider": "OpenAI", "model": model, "openAI": map[string]any{"baseUrl": "http://old/v1"}},
	}}
}

var kustomizeLabels = map[string]any{gitops.LabelKustomizeName: "flux", gitops.LabelKustomizeNamespace: "flux-giantswarm"}

func TestGitOpsOwnedModelConfigIsNeverWrittenLive(t *testing.T) {
	for name, flux := range map[string]map[string]any{
		"kustomization": kustomizeLabels,
		"helm release":  {gitops.LabelHelmName: "agent-platform", gitops.LabelHelmNamespace: "giantswarm"},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			k, client := newFakeKagent(t, committed("qwen3-0-6b-gguf", "Qwen3-0.6B-GGUF", flux))

			_, err := k.Ensure(ctx, "Qwen3-0.6B-GGUF", lemonadeEndpoint("Qwen3-0.6B-GGUF"))
			require.ErrorIs(t, err, backend.ErrGitOpsOwned)
			assert.Contains(t, err.Error(), "ModelConfig kagent/qwen3-0-6b-gguf")

			require.ErrorIs(t, k.Remove(ctx, backend.NameLemonade, "Qwen3-0.6B-GGUF"), backend.ErrGitOpsOwned)

			obj, err := client.Resource(testGVR).Namespace("kagent").Get(ctx, "qwen3-0-6b-gguf", metav1.GetOptions{})
			require.NoError(t, err, "neither the update nor the delete reached the ModelConfig")
			base, _, _ := unstructured.NestedString(obj.Object, "spec", "openAI", "baseUrl")
			assert.Equal(t, "http://old/v1", base)
			_, err = client.Resource(secretGVR).Namespace("kagent").Get(ctx, "qwen3-0-6b-gguf-api-key", metav1.GetOptions{})
			require.Error(t, err, "no placeholder Secret was created either")
		})
	}
}

func TestKustomizationOwnerNamesModeCommit(t *testing.T) {
	k, _ := newFakeKagent(t, committed("qwen3-0-6b-gguf", "Qwen3-0.6B-GGUF", kustomizeLabels))
	_, err := k.Ensure(context.Background(), "Qwen3-0.6B-GGUF", lemonadeEndpoint("Qwen3-0.6B-GGUF"))
	require.ErrorIs(t, err, backend.ErrGitOpsOwned)
	assert.Contains(t, err.Error(), "Kustomization flux-giantswarm/flux")
	assert.Contains(t, err.Error(), "mode commit")
}

func TestRenderWritesNothing(t *testing.T) {
	ctx := context.Background()
	k, client := newFakeKagent(t)
	r, err := k.Render(ctx, "Qwen3-0.6B-GGUF", lemonadeEndpoint("Qwen3-0.6B-GGUF"))
	require.NoError(t, err)
	assert.Equal(t, "qwen3-0-6b-gguf", r.Name)
	require.Len(t, r.Objects, 2)
	assert.Equal(t, "ModelConfig", r.Objects[0].GetKind())
	assert.Equal(t, "Secret", r.Objects[1].GetKind())
	assert.Equal(t, "qwen3-0-6b-gguf-api-key", r.Objects[1].GetName())
	assert.Nil(t, r.GitOps)

	list, err := client.Resource(testGVR).Namespace("kagent").List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, list.Items)
	secrets, err := client.Resource(secretGVR).Namespace("kagent").List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, secrets.Items)
}

func TestRenderEqualsWhatEnsureWrites(t *testing.T) {
	ctx := context.Background()
	k, client := newFakeKagent(t)
	ep := lemonadeEndpoint("Qwen3-0.6B-GGUF")
	r, err := k.Render(ctx, "Qwen3-0.6B-GGUF", ep)
	require.NoError(t, err)
	_, err = k.Ensure(ctx, "Qwen3-0.6B-GGUF", ep)
	require.NoError(t, err)
	obj, err := client.Resource(testGVR).Namespace("kagent").Get(ctx, r.Name, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, r.ModelConfig().Object["spec"], obj.Object["spec"])
	assert.Equal(t, r.ModelConfig().GetLabels(), obj.GetLabels())
}

func TestRenderNamesTheOwnerOfACommittedModelConfig(t *testing.T) {
	k, _ := newFakeKagent(t, committed("qwen3-0-6b-gguf", "Qwen3-0.6B-GGUF", kustomizeLabels))
	r, err := k.Render(context.Background(), "Qwen3-0.6B-GGUF", lemonadeEndpoint("Qwen3-0.6B-GGUF"))
	require.NoError(t, err, "a dry run plans a GitOps-owned ModelConfig like any other")
	require.NotNil(t, r.GitOps)
	assert.Equal(t, gitops.Owner{Kind: gitops.KindKustomization, Namespace: "flux-giantswarm", Name: "flux"}, *r.GitOps)
	base, _, _ := unstructured.NestedString(r.ModelConfig().Object, "spec", "openAI", "baseUrl")
	assert.Equal(t, "http://172.21.0.1:13305/api/v1", base)
}

func TestRemovalListsTheModelConfigAndItsPlaceholder(t *testing.T) {
	ctx := context.Background()
	k, client := newFakeKagent(t)
	_, err := k.Ensure(ctx, "Qwen3-0.6B-GGUF", lemonadeEndpoint("Qwen3-0.6B-GGUF"))
	require.NoError(t, err)

	r, err := k.Removal(ctx, backend.NameLemonade, "Qwen3-0.6B-GGUF")
	require.NoError(t, err)
	require.Len(t, r.Objects, 2)
	assert.Equal(t, "ModelConfig", r.Objects[0].GetKind())
	assert.Equal(t, "Secret", r.Objects[1].GetKind())
	_, err = client.Resource(testGVR).Namespace("kagent").Get(ctx, "qwen3-0-6b-gguf", metav1.GetOptions{})
	require.NoError(t, err, "a removal plan deletes nothing")

	none, err := k.Removal(ctx, backend.NameLemonade, "other")
	require.NoError(t, err)
	assert.Empty(t, none.Objects)
}

func TestToRefReportsTheGitOpsOwner(t *testing.T) {
	ref := toRef(committed("qwen3-0-6b-gguf", "Qwen3-0.6B-GGUF", kustomizeLabels))
	require.NotNil(t, ref.GitOps)
	assert.Equal(t, "flux", ref.GitOps.Name)
	assert.True(t, ref.Managed)
	assert.Nil(t, toRef(committed("x", "x", nil)).GitOps)
}
