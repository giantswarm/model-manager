package kserve

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/giantswarm/model-manager/internal/backend"
)

// servingTiny serves the tiny preset on a GPU pool, where no scan pod runs.
func servingTiny(t *testing.T, f *fixture) {
	t.Helper()
	ctx := context.Background()
	f.shareVolume(ctx)
	f.setDiscoveryOpts(ctx, discoveryOpts{gpuPool: poolInput()})
	require.NoError(t, f.b.Load(ctx, backend.LoadRequest{Preset: "tiny"}))
}

// A delete the API server accepts without effect leaves the object serving:
// Stop answers an error naming it, never a stopped model, and the model stays
// on the LLM endpoint.
func TestStopRefusesWhenTheDeleteDoesNotTakeEffect(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	servingTiny(t, f)
	f.dyn.PrependReactor("delete", llmisvcGVR.Resource, func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, nil
	})

	res, err := f.b.Stop(ctx, "tiny")
	require.ErrorIs(t, err, backend.ErrConflict)
	assert.Nil(t, res)
	assert.Contains(t, err.Error(), kindLLMInferenceService+" "+testServingNS+"/tiny is still served")
	left, err := f.b.getServing(ctx, testServingNS, "tiny")
	require.NoError(t, err)
	assert.NotNil(t, left, "the serving object stays")
}

// A delete that leaves the object terminating — the foreground deletion
// waiting for its pods — is an unload.
func TestStopAnswersATerminatingObjectAsStopped(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	servingTiny(t, f)
	f.dyn.PrependReactor("delete", llmisvcGVR.Resource, func(a k8stesting.Action) (bool, runtime.Object, error) {
		name := a.(k8stesting.DeleteAction).GetName()
		got, err := f.dyn.Tracker().Get(llmisvcGVR, a.GetNamespace(), name)
		if err != nil {
			return true, nil, err
		}
		obj := got.(*unstructured.Unstructured)
		now := metav1.Now()
		obj.SetDeletionTimestamp(&now)
		obj.SetFinalizers([]string{metav1.FinalizerDeleteDependents})
		return true, nil, f.dyn.Tracker().Update(llmisvcGVR, obj, a.GetNamespace())
	})

	res, err := f.b.Stop(ctx, "tiny")
	require.NoError(t, err)
	assert.Equal(t, tinyRepo, res.Model)
	left, err := f.b.getServing(ctx, testServingNS, "tiny")
	require.NoError(t, err)
	require.NotNil(t, left)
	assert.NotNil(t, left.GetDeletionTimestamp(), "the object is terminating")
}
