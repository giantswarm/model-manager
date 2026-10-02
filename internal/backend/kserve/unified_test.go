package kserve

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/giantswarm/model-manager/internal/backend"
)

// gb10Node is a unified-memory node as a GB10 reports itself: the GPU
// operator labels one shared GPU with no memory of its own, advertises it
// time-sliced, and the node's 121.7 GiB of memory is the GPU's too.
func gb10Node(name string) *corev1.Node {
	n := withGPUs(node(name, "127496348Ki", map[string]string{labelGPUPresent: "true", labelGPUCount: "1", labelGPUProduct: "NVIDIA-GB10-SHARED"}), 3)
	n.Status.Capacity[corev1.ResourceMemory] = resource.MustParse("127598748Ki")
	return n
}

// l4Node is a node with one dedicated 24 GB L4.
func l4Node(name string) *corev1.Node {
	n := withGPUs(node(name, "60Gi", map[string]string{labelGPUPresent: "true", labelGPUCount: "1", labelGPUMemory: "23034", labelGPUProduct: "NVIDIA-L4"}), 1)
	n.Status.Capacity[corev1.ResourceMemory] = resource.MustParse("64Gi")
	return n
}

// utilizationPreset is a preset that sets --gpu-memory-utilization and its
// overhead, sized from its requirements (the hub does not know the repository).
func utilizationPreset(name string, weightsGiB, overheadGiB float64, utilization string) *corev1.ConfigMap {
	return presetConfigMap(name, fmt.Sprintf(`apiVersion: agent-platform.giantswarm.io/v1alpha1
kind: ServingPreset
metadata:
  name: %[1]s
spec:
  model:
    id: example/%[1]s
    storageUri: hf://example/%[1]s
  args:
    - --max-model-len=32768
    - --gpu-memory-utilization=%[4]s
  resources:
    gpus: 1
  requirements:
    weightsGiB: %[2]v
    overheadGiB: %[3]v
`, name, weightsGiB, overheadGiB, utilization))
}

// On a unified-memory node vLLM claims --gpu-memory-utilization of the whole
// memory at start: a preset sized for a dedicated 24 GB GPU at 0.90 is
// refused there, naming the share, while it still fits the L4 it is sized
// for and a preset tuned for unified memory still fits the GB10.
func TestFitCheckJudgesVLLMsShareOnAUnifiedMemoryNode(t *testing.T) {
	f := newFixture(t,
		gb10Node("gb10"),
		l4Node("l4"),
		utilizationPreset("gpt-oss-20b", 12.8, 9, "0.90"),
		utilizationPreset("tuned", 30, 20, "0.80"),
	)
	ctx := context.Background()

	res, err := f.b.FitCheck(ctx, backend.FitRequest{Preset: "gpt-oss-20b", Node: "gb10"})
	require.NoError(t, err)
	assert.False(t, res.Fits, res.Reason)
	assert.Equal(t, "109.5 GiB", humanBytes(res.UnifiedReservationBytes))
	assert.Equal(t, gibToBytes(DefaultUnifiedHostHeadroomGiB), res.HostHeadroomBytes)
	assert.Contains(t, res.Reason, "vLLM claims 109.5 GiB at start (--gpu-memory-utilization=0.9 of the node's 121.7 GiB unified memory)")
	assert.Contains(t, res.Reason, "more than the 105.7 GiB the node leaves models beside its 16.0 GiB host headroom")
	assert.Contains(t, res.Reason, "--gpu-memory-utilization=0.86 fits")
	assert.InDelta(t, 0.86, res.FitGPUMemoryUtilization, 1e-9)

	res, err = f.b.FitCheck(ctx, backend.FitRequest{Preset: "gpt-oss-20b", Node: "l4"})
	require.NoError(t, err)
	assert.True(t, res.Fits, res.Reason)
	assert.Zero(t, res.UnifiedReservationBytes, "a dedicated GPU is judged by weights and overhead alone")
	assert.NotContains(t, res.Reason, "unified")

	res, err = f.b.FitCheck(ctx, backend.FitRequest{Preset: "tuned", Node: "gb10"})
	require.NoError(t, err)
	assert.True(t, res.Fits, res.Reason)
	assert.Equal(t, "97.4 GiB", humanBytes(res.UnifiedReservationBytes))
	assert.Contains(t, res.Reason, "vLLM claims 97.4 GiB at start (--gpu-memory-utilization=0.8 of the node's 121.7 GiB unified memory)")
}

// load_model refuses what check_fit refuses and creates nothing.
func TestLoadRefusesVLLMsShareOnAUnifiedMemoryNode(t *testing.T) {
	f := newFixture(t, gb10Node("gb10"), utilizationPreset("gpt-oss-20b", 12.8, 9, "0.90"))
	ctx := context.Background()

	err := f.b.Load(ctx, backend.LoadRequest{Name: "gpt-oss-20b", Preset: "gpt-oss-20b", Node: "gb10"})
	require.ErrorIs(t, err, backend.ErrUnfit)
	assert.Contains(t, err.Error(), "--gpu-memory-utilization=0.9")
	list, err := f.b.listServed(ctx)
	require.NoError(t, err)
	assert.Empty(t, list, "no LLMInferenceService")
}

// A running predictor on a unified-memory node holds its share of the memory
// vLLM sees there, the node's capacity, not of its allocatable budget.
func TestPresetReserveIsOfTheUnifiedMemoryCapacity(t *testing.T) {
	capacity := resource.MustParse("127598748Ki")
	assert.Equal(t, int64(0.6*float64(capacity.Value())),
		presetReserve(reservePreset("--gpu-memory-utilization=0.60"), budgetOf(gb10Node("gb10"), DefaultGPUResourceName, DefaultBudgetSource), 30))
}
