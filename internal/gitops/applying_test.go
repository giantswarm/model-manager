package gitops

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/giantswarm/model-manager/internal/backend"
)

// servingObject is an LLMInferenceService the Kustomization
// flux-giantswarm/models applied from git.
func servingObject() *unstructured.Unstructured {
	return obj("serving.kserve.io/v1alpha1", "LLMInferenceService", "model-serving", "tiny",
		map[string]any{LabelKustomizeName: "models", LabelKustomizeNamespace: "flux-giantswarm"}, nil)
}

// servingID is how the inventory names servingObject.
const servingID = "model-serving_tiny_serving.kserve.io_LLMInferenceService"

// kustomization is flux-giantswarm/models with the given inventory; no ids
// is an inventory recorded empty, inventory nil is none recorded.
func kustomization(inventory []string) *unstructured.Unstructured {
	ks := obj("kustomize.toolkit.fluxcd.io/v1", "Kustomization", "flux-giantswarm", "models", nil, map[string]any{"path": "./models", "prune": false})
	if inventory == nil {
		return ks
	}
	entries := make([]any, 0, len(inventory))
	for _, id := range inventory {
		entries = append(entries, map[string]any{"id": id, "v": "v1alpha1"})
	}
	ks.Object["status"] = map[string]any{"inventory": map[string]any{"entries": entries}}
	return ks
}

func fluxCluster(objs ...runtime.Object) dynamic.Interface {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{KustomizationGVR: "KustomizationList"}, objs...)
}

func TestApplyingHoldsAKustomizationToItsInventory(t *testing.T) {
	models := &Owner{Kind: KindKustomization, Namespace: "flux-giantswarm", Name: "models"}
	for name, tc := range map[string]struct {
		cluster dynamic.Interface
		object  *unstructured.Unstructured
		owner   *Owner
	}{
		"listed in the inventory":    {fluxCluster(kustomization([]string{"model-serving_other__Secret", servingID})), servingObject(), models},
		"dropped from the inventory": {fluxCluster(kustomization([]string{"model-serving_other__Secret"})), servingObject(), nil},
		"inventory recorded empty":   {fluxCluster(kustomization([]string{})), servingObject(), nil},
		"no inventory recorded yet":  {fluxCluster(kustomization(nil)), servingObject(), models},
		"kustomization gone":         {fluxCluster(), servingObject(), nil},
		"rendered by a helm release": {fluxCluster(), obj("v1", "Secret", "kagent", "key", map[string]any{LabelHelmName: "agent-platform", LabelHelmNamespace: "giantswarm"}, nil), &Owner{Kind: KindHelmRelease, Namespace: "giantswarm", Name: "agent-platform"}},
		"written live":               {fluxCluster(), obj("v1", "Secret", "kagent", "key", nil, nil), nil},
	} {
		t.Run(name, func(t *testing.T) {
			owner, err := Applying(context.Background(), tc.cluster, tc.object)
			require.NoError(t, err)
			assert.Equal(t, tc.owner, owner)
		})
	}
}

func TestKeptIsANonPruningKustomizationThatDroppedTheObject(t *testing.T) {
	models := &Owner{Kind: KindKustomization, Namespace: "flux-giantswarm", Name: "models"}
	pruning := kustomization([]string{})
	pruning.Object["spec"].(map[string]any)["prune"] = true
	for name, tc := range map[string]struct {
		cluster dynamic.Interface
		object  *unstructured.Unstructured
		kept    *Owner
	}{
		"dropped from the inventory": {fluxCluster(kustomization([]string{"model-serving_other__Secret"})), servingObject(), models},
		"inventory recorded empty":   {fluxCluster(kustomization([]string{})), servingObject(), models},
		"listed in the inventory":    {fluxCluster(kustomization([]string{servingID})), servingObject(), nil},
		"no inventory recorded yet":  {fluxCluster(kustomization(nil)), servingObject(), nil},
		"the kustomization prunes":   {fluxCluster(pruning), servingObject(), nil},
		"kustomization gone":         {fluxCluster(), servingObject(), nil},
		"written live":               {fluxCluster(), obj("v1", "Secret", "kagent", "key", nil, nil), nil},
	} {
		t.Run(name, func(t *testing.T) {
			kept, err := Kept(context.Background(), tc.cluster, tc.object)
			require.NoError(t, err)
			assert.Equal(t, tc.kept, kept)
		})
	}
}

func TestApplyingReportsAKustomizationItCannotRead(t *testing.T) {
	cluster := fluxCluster(kustomization([]string{servingID}))
	cluster.(*dynamicfake.FakeDynamicClient).PrependReactor("get", "kustomizations", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(KustomizationGVR.GroupResource(), "models", fmt.Errorf("no"))
	})
	_, err := Applying(context.Background(), cluster, servingObject())
	require.Error(t, err)
	assert.True(t, apierrors.IsForbidden(err), "the read's own error is kept: %v", err)
	assert.Contains(t, err.Error(), "Kustomization flux-giantswarm/models")
	assert.Contains(t, err.Error(), "LLMInferenceService model-serving/tiny")
}

func TestRefuseNamesTheKustomizationStillApplying(t *testing.T) {
	ctx := context.Background()
	err := Refuse(ctx, fluxCluster(kustomization([]string{servingID})), servingObject())
	require.ErrorIs(t, err, backend.ErrGitOpsOwned)
	assert.Contains(t, err.Error(), "LLMInferenceService model-serving/tiny is applied from git by Kustomization flux-giantswarm/models")
	assert.Contains(t, err.Error(), "mode commit")

	assert.NoError(t, Refuse(ctx, fluxCluster(kustomization([]string{})), servingObject()), "left behind by a Kustomization that does not prune: removed live")
	assert.NoError(t, Refuse(ctx, fluxCluster(), obj("v1", "Secret", "kagent", "key", nil, nil)))
}
