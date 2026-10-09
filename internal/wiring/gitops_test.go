package wiring

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

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

// applying is the Kustomization flux-giantswarm/flux, which the kustomize
// labels name, with an inventory recording ids: what Flux applied last.
func applying(ids ...string) *unstructured.Unstructured {
	entries := make([]any, 0, len(ids))
	for _, id := range ids {
		entries = append(entries, map[string]any{"id": id, "v": DefaultAPIVersion})
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kustomize.toolkit.fluxcd.io/v1", "kind": "Kustomization",
		"metadata": map[string]any{"name": "flux", "namespace": "flux-giantswarm"},
		"spec":     map[string]any{"path": "./platform", "prune": false},
		"status":   map[string]any{"inventory": map[string]any{"entries": entries}},
	}}
}

// inventoryID is how the Kustomization's inventory names a ModelConfig of
// the kagent namespace in group.
func inventoryID(group, name string) string { return "kagent_" + name + "_" + group + "_ModelConfig" }

func TestGitOpsOwnedModelConfigIsNeverWrittenLive(t *testing.T) {
	for name, flux := range map[string]map[string]any{
		"kustomization": kustomizeLabels,
		"helm release":  {gitops.LabelHelmName: "agent-platform", gitops.LabelHelmNamespace: "giantswarm"},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			k, client := newFakeKagent(t, committed("qwen3-0-6b-gguf", "Qwen3-0.6B-GGUF", flux), applying(inventoryID(KagentGroup, "qwen3-0-6b-gguf")))

			_, err := k.Ensure(ctx, "Qwen3-0.6B-GGUF", lemonadeEndpoint("Qwen3-0.6B-GGUF"))
			require.ErrorIs(t, err, backend.ErrGitOpsOwned)
			assert.Contains(t, err.Error(), "ModelConfig kagent/qwen3-0-6b-gguf")

			require.ErrorIs(t, k.Remove(ctx, backend.NameLemonade, "Qwen3-0.6B-GGUF", CreatedHere), backend.ErrGitOpsOwned)

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
	k, _ := newFakeKagent(t, committed("qwen3-0-6b-gguf", "Qwen3-0.6B-GGUF", kustomizeLabels), applying(inventoryID(KagentGroup, "qwen3-0-6b-gguf")))
	_, err := k.Ensure(context.Background(), "Qwen3-0.6B-GGUF", lemonadeEndpoint("Qwen3-0.6B-GGUF"))
	require.ErrorIs(t, err, backend.ErrGitOpsOwned)
	assert.Contains(t, err.Error(), "Kustomization flux-giantswarm/flux")
	assert.Contains(t, err.Error(), "mode commit")
}

// placeholderLeft is the placeholder Secret of a committed ModelConfig as
// Flux applied it: model-manager's label and the Kustomization's.
func placeholderLeft(mcName string) *unstructured.Unstructured {
	labels := map[string]any{ManagedByLabel: ManagedByValue}
	for k, v := range kustomizeLabels {
		labels[k] = v
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Secret",
		"metadata": map[string]any{"name": placeholderSecretName(mcName), "namespace": "kagent", "labels": labels},
		"type":     "Opaque",
	}}
}

func TestModelConfigItsKustomizationLeftBehindIsWrittenLive(t *testing.T) {
	// The removal merged while the Kustomization does not prune: the
	// ModelConfig and its placeholder Secret stay, labels and all, and the
	// inventory no longer lists them.
	for name, ks := range map[string][]runtime.Object{
		"dropped from the inventory": {applying(inventoryID(KagentGroup, "other"))},
		"kustomization gone":         nil,
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			mc := committed("qwen3-0-6b-gguf", "Qwen3-0.6B-GGUF", kustomizeLabels)
			labels := mc.GetLabels()
			labels[InstanceLabel] = testInstance
			mc.SetLabels(labels)
			k, client := newFakeKagent(t, append([]runtime.Object{mc, placeholderLeft("qwen3-0-6b-gguf")}, ks...)...)

			r, err := k.Removal(ctx, backend.NameLemonade, "Qwen3-0.6B-GGUF", CreatedHere)
			require.NoError(t, err)
			assert.Nil(t, r.GitOps, "the dry run reports no Flux owner")
			require.Len(t, r.Objects, 2, "the ModelConfig and its placeholder Secret go")

			require.NoError(t, k.Remove(ctx, backend.NameLemonade, "Qwen3-0.6B-GGUF", CreatedHere))
			_, err = client.Resource(testGVR).Namespace("kagent").Get(ctx, "qwen3-0-6b-gguf", metav1.GetOptions{})
			assert.True(t, apierrors.IsNotFound(err), "the ModelConfig is deleted: %v", err)
			_, err = client.Resource(secretGVR).Namespace("kagent").Get(ctx, "qwen3-0-6b-gguf-api-key", metav1.GetOptions{})
			assert.True(t, apierrors.IsNotFound(err), "and its placeholder Secret: %v", err)
		})
	}

	// A wire over a leftover updates it live, as over any ModelConfig of
	// model-manager's own.
	ctx := context.Background()
	k, client := newFakeKagent(t, committed("qwen3-0-6b-gguf", "Qwen3-0.6B-GGUF", kustomizeLabels), applying())
	r, err := k.Render(ctx, "Qwen3-0.6B-GGUF", lemonadeEndpoint("Qwen3-0.6B-GGUF"))
	require.NoError(t, err)
	assert.Nil(t, r.GitOps)
	_, err = k.Ensure(ctx, "Qwen3-0.6B-GGUF", lemonadeEndpoint("Qwen3-0.6B-GGUF"))
	require.NoError(t, err)
	obj, err := client.Resource(testGVR).Namespace("kagent").Get(ctx, "qwen3-0-6b-gguf", metav1.GetOptions{})
	require.NoError(t, err)
	base, _, _ := unstructured.NestedString(obj.Object, "spec", "openAI", "baseUrl")
	assert.Equal(t, "http://172.21.0.1:13305/api/v1", base)
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
	k, _ := newFakeKagent(t, committed("qwen3-0-6b-gguf", "Qwen3-0.6B-GGUF", kustomizeLabels), applying(inventoryID(KagentGroup, "qwen3-0-6b-gguf")))
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

	r, err := k.Removal(ctx, backend.NameLemonade, "Qwen3-0.6B-GGUF", CreatedHere)
	require.NoError(t, err)
	require.Len(t, r.Objects, 2)
	assert.Equal(t, "ModelConfig", r.Objects[0].GetKind())
	assert.Equal(t, "Secret", r.Objects[1].GetKind())
	_, err = client.Resource(testGVR).Namespace("kagent").Get(ctx, "qwen3-0-6b-gguf", metav1.GetOptions{})
	require.NoError(t, err, "a removal plan deletes nothing")

	none, err := k.Removal(ctx, backend.NameLemonade, "other", CreatedHere)
	require.NoError(t, err)
	assert.Empty(t, none.Objects)
}

// TestListReportsTheOwnerTheWriteToolsActOn: list, list-all and lookup hold
// the GitOps owner to the Kustomization's inventory, as unwire and wire do.
func TestListReportsTheOwnerTheWriteToolsActOn(t *testing.T) {
	for name, tc := range map[string]struct {
		labels map[string]any
		ks     []runtime.Object
		owned  bool
	}{
		"listed in the inventory":    {kustomizeLabels, []runtime.Object{applying(inventoryID(KagentGroup, "qwen3-0-6b-gguf"))}, true},
		"dropped from the inventory": {kustomizeLabels, []runtime.Object{applying(inventoryID(KagentGroup, "other"))}, false},
		"kustomization gone":         {kustomizeLabels, nil, false},
		"no inventory recorded yet":  {kustomizeLabels, []runtime.Object{noInventory()}, true},
		"without flux labels":        {nil, []runtime.Object{applying(inventoryID(KagentGroup, "qwen3-0-6b-gguf"))}, false},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			k, _ := newFakeKagent(t, append([]runtime.Object{committed("qwen3-0-6b-gguf", "Qwen3-0.6B-GGUF", tc.labels)}, tc.ks...)...)

			ref, err := k.Lookup(ctx, backend.NameLemonade, "Qwen3-0.6B-GGUF")
			require.NoError(t, err)
			refs := []ModelConfigRef{*ref}
			for _, list := range []func(context.Context) ([]ModelConfigRef, error){k.List, k.ListAll} {
				got, err := list(ctx)
				require.NoError(t, err)
				require.Len(t, got, 1)
				refs = append(refs, got...)
			}
			for _, ref := range refs {
				assert.True(t, ref.Managed)
				if !tc.owned {
					assert.Nil(t, ref.GitOps, "reported as written live")
					continue
				}
				require.NotNil(t, ref.GitOps)
				assert.Equal(t, gitops.Owner{Kind: gitops.KindKustomization, Namespace: "flux-giantswarm", Name: "flux"}, *ref.GitOps)
			}

			// The write tools agree: a gitops-owned ModelConfig is refused live.
			err = k.Remove(ctx, backend.NameLemonade, "Qwen3-0.6B-GGUF", CreatedHere)
			if tc.owned {
				require.ErrorIs(t, err, backend.ErrGitOpsOwned)
			} else {
				require.NotErrorIs(t, err, backend.ErrGitOpsOwned)
			}
		})
	}
}

// noInventory is the Kustomization flux-giantswarm/flux before it recorded
// what it applies.
func noInventory() *unstructured.Unstructured {
	ks := applying()
	unstructured.RemoveNestedField(ks.Object, "status")
	return ks
}
