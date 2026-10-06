package kserve

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

// appliedFromGit serves the tiny preset and labels its serving object as
// applied by the Kustomization flux-giantswarm/models, which records the
// given inventory ids (nil: no Kustomization at all).
func appliedFromGit(t *testing.T, f *fixture, inventory []string) string {
	t.Helper()
	ctx := context.Background()
	f.shareVolume(ctx)
	f.setDiscoveryOpts(ctx, discoveryOpts{gpuPool: poolInput()})
	require.NoError(t, f.b.Load(ctx, backend.LoadRequest{Preset: "tiny"}))
	ns := f.b.cfg.settings(ctx).Namespace
	res := f.dyn.Resource(llmisvcGVR).Namespace(ns)
	obj, err := res.Get(ctx, "tiny", metav1.GetOptions{})
	require.NoError(t, err)
	labels := obj.GetLabels()
	labels[gitops.LabelKustomizeName], labels[gitops.LabelKustomizeNamespace] = "models", "flux-giantswarm"
	obj.SetLabels(labels)
	_, err = res.Update(ctx, obj, metav1.UpdateOptions{})
	require.NoError(t, err)
	if inventory == nil {
		return ns
	}
	entries := make([]any, 0, len(inventory))
	for _, id := range inventory {
		entries = append(entries, map[string]any{"id": id, "v": "v1alpha1"})
	}
	ks := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kustomize.toolkit.fluxcd.io/v1", "kind": "Kustomization",
		"metadata": map[string]any{"name": "models", "namespace": "flux-giantswarm"},
		"spec":     map[string]any{"path": "./models", "prune": false},
		"status":   map[string]any{"inventory": map[string]any{"entries": entries}},
	}}
	_, err = f.dyn.Resource(gitops.KustomizationGVR).Namespace("flux-giantswarm").Create(ctx, ks, metav1.CreateOptions{})
	require.NoError(t, err)
	return ns
}

func TestStopRefusesAServingObjectFluxApplies(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	ns := appliedFromGit(t, f, []string{ns(t, f) + "_tiny_serving.kserve.io_LLMInferenceService"})

	_, err := f.b.Stop(ctx, "tiny")
	require.ErrorIs(t, err, backend.ErrGitOpsOwned)
	assert.Contains(t, err.Error(), "Kustomization flux-giantswarm/models")
	left, err := f.b.getServing(ctx, ns, "tiny")
	require.NoError(t, err)
	assert.NotNil(t, left, "the serving object stays")

	plan, err := f.b.StopPlan(ctx, "tiny")
	require.NoError(t, err)
	require.Len(t, plan.Objects, 1, "the plan lists the object, for mode commit to remove it in git")
	require.ErrorIs(t, plan.Refusal, backend.ErrGitOpsOwned, "and carries the refusal the live call answers")
	assert.Contains(t, plan.Refusal.Error(), "Kustomization flux-giantswarm/models")
}

// ns is the serving namespace of the fixture.
func ns(t *testing.T, f *fixture) string {
	t.Helper()
	return f.b.cfg.settings(context.Background()).Namespace
}

func TestStopDeletesAServingObjectItsKustomizationNoLongerApplies(t *testing.T) {
	for name, inventory := range map[string][]string{
		"dropped from the inventory": {"model-serving_other_serving.kserve.io_LLMInferenceService"},
		"kustomization gone":         nil,
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			ctx := context.Background()
			ns := appliedFromGit(t, f, inventory)

			plan, err := f.b.StopPlan(ctx, "tiny")
			require.NoError(t, err)
			require.Len(t, plan.Objects, 1)
			assert.NoError(t, plan.Refusal, "the dry run plans the deletion the live call does")

			_, err = f.b.Stop(ctx, "tiny")
			require.NoError(t, err, "left behind by a Kustomization that does not prune: model-manager deletes it")
			left, err := f.b.getServing(ctx, ns, "tiny")
			require.NoError(t, err)
			assert.Nil(t, left, "the serving object is gone")
		})
	}
}

func TestServeDryRunCreatesNothing(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.shareVolume(ctx)
	f.setDiscoveryOpts(ctx, discoveryOpts{gpuPool: poolInput()})
	res, err := f.b.Serve(ctx, backend.LoadRequest{Preset: "tiny", DryRun: true})
	require.NoError(t, err)
	require.Len(t, res.Manifests, 1)
	assert.Equal(t, kindLLMInferenceService, res.Manifests[0].GetKind())
	assert.Equal(t, "tiny", res.Manifests[0].GetName())
	require.NotNil(t, res.Fit)
	left, err := f.b.getServing(ctx, f.b.cfg.settings(ctx).Namespace, "tiny")
	require.NoError(t, err)
	assert.Nil(t, left, "a dry run creates no serving object")

	require.NoError(t, f.b.Load(ctx, backend.LoadRequest{Preset: "tiny"}))
	created, err := f.b.getServing(ctx, f.b.cfg.settings(ctx).Namespace, "tiny")
	require.NoError(t, err)
	assert.Equal(t, res.Manifests[0].Object["spec"], created.Object["spec"], "the dry run shows what the load creates")
}

func TestStopPlanListsWithoutDeleting(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.shareVolume(ctx)
	f.setDiscoveryOpts(ctx, discoveryOpts{gpuPool: poolInput()})
	require.NoError(t, f.b.Load(ctx, backend.LoadRequest{Preset: "tiny"}))
	plan, err := f.b.StopPlan(ctx, "tiny")
	require.NoError(t, err)
	assert.Equal(t, tinyRepo, plan.Model)
	require.Len(t, plan.Objects, 1)
	assert.Equal(t, "tiny", plan.Objects[0].GetName())
	assert.NoError(t, plan.Refusal)
	left, err := f.b.getServing(ctx, f.b.cfg.settings(ctx).Namespace, "tiny")
	require.NoError(t, err)
	assert.NotNil(t, left)

	_, err = f.b.StopPlan(ctx, "nothing-served")
	require.ErrorIs(t, err, backend.ErrNotFound)
}
