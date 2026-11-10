package kserve

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"

	"github.com/giantswarm/model-manager/internal/backend"
)

// dropPresets deletes the published presets, as on a cluster whose serving
// slice has not published them yet.
func (f *fixture) dropPresets(ctx context.Context) {
	f.t.Helper()
	for _, name := range []string{"tiny", "big"} {
		require.NoError(f.t, f.cs.CoreV1().ConfigMaps(testPlatformNS).Delete(ctx, presetConfigMap(name, "").Name, metav1.DeleteOptions{}))
	}
}

// publishPresets puts the fixture's presets back.
func (f *fixture) publishPresets(ctx context.Context) {
	f.t.Helper()
	for _, cm := range newFixturePresets() {
		_, err := f.cs.CoreV1().ConfigMaps(testPlatformNS).Create(ctx, cm, metav1.CreateOptions{})
		require.NoError(f.t, err)
	}
}

func newFixturePresets() []*corev1.ConfigMap {
	return []*corev1.ConfigMap{
		presetConfigMap("tiny", presetDoc("tiny", tinyRepo, 0.001, "")),
		presetConfigMap("big", presetDoc("big", bigRepo, 100, "")),
	}
}

// A newly registered backend whose llm-d controller is still installing has
// published no preset yet: list_presets and load_model of a preset model say
// the backend is not ready — naming it and what it waits on — instead of an
// empty list and not_found; once the controller and its presets land, the
// same calls behave as on a ready backend.
func TestPresetReadsWhileTheBackendIsNotReady(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		name   string
		drop   func(*fixture)
		waitOn string
	}{
		{"the controller is installing", func(f *fixture) { f.dropControlPlane(ctx) }, "the llm-d controller is not installed"},
		{"the API is not served yet", func(f *fixture) { f.dropLLMAPI() }, "is not served on this cluster"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.dropPresets(ctx)
			tc.drop(f)

			presets, err := f.b.ListPresets(ctx)
			require.ErrorIs(t, err, backend.ErrUnavailable, "not an empty list")
			assert.Nil(t, presets)
			assert.ErrorContains(t, err, "backend "+string(f.b.Name())+" is not ready")
			assert.ErrorContains(t, err, tc.waitOn)
			assert.ErrorContains(t, err, "retry in a moment")

			_, err = f.b.GetModel(ctx, tinyRepo)
			require.ErrorIs(t, err, backend.ErrUnavailable)
			assert.NotErrorIs(t, err, backend.ErrNotFound, "the model is not judged before its presets are published")
			assert.ErrorContains(t, err, "backend "+string(f.b.Name())+" is not ready")
			assert.ErrorContains(t, err, "whether "+tinyRepo+" is a preset is known once its serving presets are published")
			assert.ErrorContains(t, err, tc.waitOn)

			_, err = f.b.GetModel(ctx, "tiny")
			require.ErrorIs(t, err, backend.ErrUnavailable, "by preset name too")
		})
	}

	t.Run("the missing API rejects inventory reads", func(t *testing.T) {
		f := newFixture(t)
		f.dropPresets(ctx)
		f.dropLLMAPI()
		f.dyn.PrependReactor("list", llmisvcGVR.Resource, func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewNotFound(schema.GroupResource{Group: llmisvcGVR.Group, Resource: llmisvcGVR.Resource}, "")
		})
		for _, name := range []string{tinyRepo, "tiny"} {
			_, err := f.b.GetModel(ctx, name)
			require.ErrorIs(t, err, backend.ErrUnavailable)
			assert.ErrorContains(t, err, "backend "+string(f.b.Name())+" is not ready")
			assert.ErrorContains(t, err, "is not served on this cluster")
			assert.ErrorContains(t, err, "retry in a moment")
		}
	})

	t.Run("an inventory permission failure is preserved", func(t *testing.T) {
		f := newFixture(t)
		f.dropPresets(ctx)
		f.dropControlPlane(ctx)
		f.dyn.PrependReactor("list", llmisvcGVR.Resource, func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: llmisvcGVR.Group, Resource: llmisvcGVR.Resource}, "", errors.New("list denied"))
		})
		_, err := f.b.GetModel(ctx, tinyRepo)
		require.True(t, apierrors.IsForbidden(err))
		assert.NotErrorIs(t, err, backend.ErrUnavailable)
	})

	t.Run("the backend becomes ready", func(t *testing.T) {
		f := newFixture(t)
		f.dropPresets(ctx)
		f.dropControlPlane(ctx)
		_, err := f.b.ListPresets(ctx)
		require.ErrorIs(t, err, backend.ErrUnavailable)

		f.installControlPlane(ctx)
		f.publishPresets(ctx)

		presets, err := f.b.ListPresets(ctx)
		require.NoError(t, err)
		assert.Len(t, presets, 2)
		m, err := f.b.GetModel(ctx, tinyRepo)
		require.NoError(t, err)
		assert.Equal(t, "tiny", m.Preset)
		_, err = f.b.GetModel(ctx, "example/unknown")
		require.ErrorIs(t, err, backend.ErrNotFound, "a wrong name on a ready backend is not_found")
		require.NoError(t, f.b.Load(ctx, backend.LoadRequest{Name: tinyRepo, Preset: "tiny"}))
	})

	t.Run("presets published, controller still installing", func(t *testing.T) {
		f := newFixture(t)
		f.dropControlPlane(ctx)
		presets, err := f.b.ListPresets(ctx)
		require.NoError(t, err, "the published presets are listed as today")
		assert.Len(t, presets, 2)
		_, err = f.b.GetModel(ctx, "example/unknown")
		require.ErrorIs(t, err, backend.ErrNotFound, "the presets are known: a name none serves is not_found")
		err = f.b.Load(ctx, backend.LoadRequest{Preset: "tiny"})
		require.ErrorIs(t, err, backend.ErrUnavailable)
		assert.ErrorContains(t, err, "backend "+string(f.b.Name())+" is not ready: the "+kindLLMInferenceService+" API is served but the llm-d controller is not installed")
	})

	t.Run("a ready backend publishing no preset", func(t *testing.T) {
		f := newFixture(t)
		f.dropPresets(ctx)
		presets, err := f.b.ListPresets(ctx)
		require.NoError(t, err, "as today")
		assert.Empty(t, presets)
		_, err = f.b.GetModel(ctx, tinyRepo)
		require.ErrorIs(t, err, backend.ErrNotFound)
		assert.ErrorContains(t, err, "is neither cached, served nor a preset")
	})
}
