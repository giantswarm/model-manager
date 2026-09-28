package kserve

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/giantswarm/model-manager/internal/backend"
	"github.com/giantswarm/model-manager/internal/gitops"
)

func TestStopRefusesAServingObjectFluxApplies(t *testing.T) {
	f := newFixture(t)
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

	_, err = f.b.Stop(ctx, "tiny")
	require.ErrorIs(t, err, backend.ErrGitOpsOwned)
	assert.Contains(t, err.Error(), "Kustomization flux-giantswarm/models")
	left, err := f.b.getServing(ctx, ns, "tiny")
	require.NoError(t, err)
	assert.NotNil(t, left, "the serving object stays")
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
	model, objs, err := f.b.StopPlan(ctx, "tiny")
	require.NoError(t, err)
	assert.Equal(t, tinyRepo, model)
	require.Len(t, objs, 1)
	assert.Equal(t, "tiny", objs[0].GetName())
	left, err := f.b.getServing(ctx, f.b.cfg.settings(ctx).Namespace, "tiny")
	require.NoError(t, err)
	assert.NotNil(t, left)

	_, _, err = f.b.StopPlan(ctx, "nothing-served")
	require.ErrorIs(t, err, backend.ErrNotFound)
}
