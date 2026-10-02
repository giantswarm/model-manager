package kserve

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/giantswarm/model-manager/internal/backend"
)

const testTemplateImage = "gsoci.azurecr.io/giantswarm/llm-d-cuda:v0.4.0"

// addPinnedPreset publishes a preset that names its own runtime image, in a
// ConfigMap the connectivity chart of version chartVersion rendered.
func (f *fixture) addPinnedPreset(ctx context.Context, chartVersion string) {
	f.t.Helper()
	doc := presetDoc("pinned", "org/pinned", 1, `  template:
    containers:
      - name: main
        image: gsoci.azurecr.io/giantswarm/llm-d-cpu:v0.8.0
`)
	cm := presetConfigMap("pinned", doc)
	cm.Annotations = map[string]string{PresetChartVersionAnnotation: chartVersion}
	_, err := f.cs.CoreV1().ConfigMaps(testPlatformNS).Create(ctx, cm, metav1.CreateOptions{})
	require.NoError(f.t, err)
}

// A benchmark records how its model was served from list_presets: the image
// the load composes into the predictor — the preset's own, else the
// well-known template's — and the chart version that shipped the preset.
func TestListPresetsReportsRuntimeImageAndChartVersion(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.addPinnedPreset(ctx, "4.107.0+86298ff6b72b")

	presets, err := f.b.ListPresets(ctx)
	require.NoError(t, err)
	byName := map[string]backend.Preset{}
	for _, p := range presets {
		byName[p.Name] = p
	}
	assert.Equal(t, "gsoci.azurecr.io/giantswarm/llm-d-cpu:v0.8.0", byName["pinned"].RuntimeImage, "the preset's containers[main].image")
	assert.Equal(t, "4.107.0+86298ff6b72b", byName["pinned"].ChartVersion)
	assert.Equal(t, testTemplateImage, byName["tiny"].RuntimeImage, "no image in the preset: the well-known template's")
	assert.Empty(t, byName["tiny"].ChartVersion, "a ConfigMap without the annotation names no chart version")
}

func TestListPresetsWithoutControlPlaneNamesNoTemplateImage(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.dropControlPlane(ctx)

	presets, err := f.b.ListPresets(ctx)
	require.NoError(t, err)
	for _, p := range presets {
		assert.Empty(t, p.RuntimeImage, "%s: no well-known template, no image to name", p.Name)
	}
}

// The served model reports the same two values for the serving object
// model-manager created: the chart version the load recorded on it, the
// image its main container runs.
func TestListLoadedReportsRuntimeImageAndChartVersion(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.addPinnedPreset(ctx, "4.107.0")
	obj := f.pendingLLMISVC(ctx, "pinned")
	assert.Equal(t, "4.107.0", obj.GetAnnotations()[ChartVersionAnnotation], "the load records the preset's chart version")
	f.pendingLLMISVC(ctx, "tiny")

	loaded, err := f.b.ListLoaded(ctx)
	require.NoError(t, err)
	byPreset := map[string]backend.LoadedModel{}
	for _, lm := range loaded {
		byPreset[lm.Preset] = lm
	}
	require.Len(t, byPreset, 2)
	assert.Equal(t, "gsoci.azurecr.io/giantswarm/llm-d-cpu:v0.8.0", byPreset["pinned"].RuntimeImage)
	assert.Equal(t, "4.107.0", byPreset["pinned"].ChartVersion)
	assert.Equal(t, testTemplateImage, byPreset["tiny"].RuntimeImage, "no image on the object: the well-known template's")
	assert.Empty(t, byPreset["tiny"].ChartVersion, "its preset's ConfigMap names none")
}
