package kserve

import (
	"context"
	"fmt"
	"strings"
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
// sized down there to the share its weights and overhead need, so the host
// keeps its headroom, while it runs as written on the L4 it is sized for and
// a preset tuned for unified memory still fits the GB10 as written. A preset
// whose weights and overhead alone leave the host less than its headroom is
// refused, naming the share.
func TestFitCheckJudgesVLLMsShareOnAUnifiedMemoryNode(t *testing.T) {
	f := newFixture(t,
		gb10Node("gb10"),
		l4Node("l4"),
		utilizationPreset("gpt-oss-20b", 12.8, 9, "0.90"),
		utilizationPreset("tuned", 30, 20, "0.80"),
		utilizationPreset("crowded", 80, 30, "0.90"),
	)
	ctx := context.Background()

	res, err := f.b.FitCheck(ctx, backend.FitRequest{Preset: "gpt-oss-20b", Node: "gb10"})
	require.NoError(t, err)
	assert.True(t, res.Fits, res.Reason)
	assert.InDelta(t, 0.18, res.GPUMemoryUtilization, 1e-9)
	assert.Equal(t, "21.9 GiB", humanBytes(res.UnifiedReservationBytes))
	assert.Equal(t, gibToBytes(DefaultUnifiedHostHeadroomGiB), res.HostHeadroomBytes)
	assert.Contains(t, res.Reason, "vLLM claims 21.9 GiB at start (--gpu-memory-utilization=0.18, sized down from the preset's 0.9, of the node's 121.7 GiB unified memory)")
	assert.Zero(t, res.FitGPUMemoryUtilization)

	res, err = f.b.FitCheck(ctx, backend.FitRequest{Preset: "gpt-oss-20b", Node: "l4"})
	require.NoError(t, err)
	assert.True(t, res.Fits, res.Reason)
	assert.Zero(t, res.UnifiedReservationBytes, "a dedicated GPU is judged by weights and overhead alone")
	assert.InDelta(t, 0.9, res.GPUMemoryUtilization, 1e-9)
	assert.NotContains(t, res.Reason, "unified")

	res, err = f.b.FitCheck(ctx, backend.FitRequest{Preset: "tuned", Node: "gb10"})
	require.NoError(t, err)
	assert.True(t, res.Fits, res.Reason)
	assert.Equal(t, "97.4 GiB", humanBytes(res.UnifiedReservationBytes))
	assert.Contains(t, res.Reason, "vLLM claims 97.4 GiB at start (--gpu-memory-utilization=0.8 of the node's 121.7 GiB unified memory)")

	res, err = f.b.FitCheck(ctx, backend.FitRequest{Preset: "crowded", Node: "gb10"})
	require.NoError(t, err)
	assert.False(t, res.Fits, res.Reason)
	assert.Equal(t, "109.5 GiB", humanBytes(res.UnifiedReservationBytes))
	assert.Contains(t, res.Reason, "vLLM claims 109.5 GiB at start (--gpu-memory-utilization=0.9 of the node's 121.7 GiB unified memory)")
	assert.Contains(t, res.Reason, "more than the 105.7 GiB the node leaves models beside its 16.0 GiB host headroom")
	assert.Zero(t, res.FitGPUMemoryUtilization, "no utilization holds 110 GiB within 105.7 GiB")
}

// load_model refuses what check_fit refuses and creates nothing; what it
// sizes down it composes at the sized utilization.
func TestLoadOnAUnifiedMemoryNode(t *testing.T) {
	f := newFixture(t, gb10Node("gb10"), utilizationPreset("crowded", 80, 30, "0.90"), utilizationPreset("gpt-oss-20b", 12.8, 9, "0.90"))
	ctx := context.Background()

	err := f.b.Load(ctx, backend.LoadRequest{Name: "crowded", Preset: "crowded", Node: "gb10"})
	require.ErrorIs(t, err, backend.ErrUnfit)
	assert.Contains(t, err.Error(), "--gpu-memory-utilization=0.9")
	list, err := f.b.listServed(ctx)
	require.NoError(t, err)
	assert.Empty(t, list, "no LLMInferenceService")

	require.NoError(t, f.b.Load(ctx, backend.LoadRequest{Name: "gpt-oss-20b", Preset: "gpt-oss-20b", Node: "gb10"}))
	list, err = f.b.listServed(ctx)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.InDelta(t, 0.18, list[0].Utilization, 1e-9)
}

// A running predictor on a unified-memory node holds its share of the memory
// vLLM sees there, the node's capacity, not of its allocatable budget.
func TestPresetReserveIsOfTheUnifiedMemoryCapacity(t *testing.T) {
	capacity := resource.MustParse("127598748Ki")
	assert.Equal(t, int64(0.6*float64(capacity.Value())),
		presetReserve(reservePreset("--gpu-memory-utilization=0.60"), budgetOf(gb10Node("gb10"), DefaultGPUResourceName, DefaultBudgetSource), 30))
}

// residentPreset is utilizationPreset with part of its weights left on disk:
// requirements.residentWeightsGiB is what the runtime holds in memory.
func residentPreset(name string, weightsGiB, residentGiB, overheadGiB float64, utilization string) *corev1.ConfigMap {
	cm := utilizationPreset(name, weightsGiB, overheadGiB, utilization)
	cm.Data[presetConfigKey] += fmt.Sprintf("    residentWeightsGiB: %v\n", residentGiB)
	return cm
}

// A claim the node leaves but that cannot hold the weights and overhead it
// serves is refused, naming the utilization that holds them — never sized
// up behind the preset's back, and load_model refuses it too. Weights held on
// disk do not count against the claim.
func TestFitCheckRefusesAUnifiedClaimShortOfItsNeed(t *testing.T) {
	f := newFixture(t,
		gb10Node("gb10"),
		utilizationPreset("kolibri", 73.4, 34, "0.60"),
		utilizationPreset("short", 60, 20, "0.60"),
		residentPreset("ngram", 100, 74, 20, "0.80"),
	)
	ctx := context.Background()

	res, err := f.b.FitCheck(ctx, backend.FitRequest{Preset: "kolibri", Node: "gb10"})
	require.NoError(t, err)
	assert.False(t, res.Fits, res.Reason)
	assert.Contains(t, res.Reason, "vLLM claims 73.0 GiB at start (--gpu-memory-utilization=0.6 of the node's 121.7 GiB unified memory), less than the 107.4 GiB of weights and overhead it must hold: --gpu-memory-utilization=0.89 would, more than the 105.7 GiB the node leaves models")
	assert.Zero(t, res.FitGPUMemoryUtilization, "0.89 claims more than the node leaves")

	res, err = f.b.FitCheck(ctx, backend.FitRequest{Preset: "short", Node: "gb10"})
	require.NoError(t, err)
	assert.False(t, res.Fits, res.Reason)
	assert.Contains(t, res.Reason, "less than the 80.0 GiB of weights and overhead it must hold: --gpu-memory-utilization=0.66 holds them")
	assert.InDelta(t, 0.66, res.FitGPUMemoryUtilization, 1e-9)
	assert.InDelta(t, 0.6, res.GPUMemoryUtilization, 1e-9, "the preset's own utilization stands")

	err = f.b.Load(ctx, backend.LoadRequest{Name: "short", Preset: "short", Node: "gb10"})
	require.ErrorIs(t, err, backend.ErrUnfit)
	assert.Contains(t, err.Error(), "--gpu-memory-utilization=0.66 holds them")
	list, err := f.b.listServed(ctx)
	require.NoError(t, err)
	assert.Empty(t, list, "no LLMInferenceService")

	res, err = f.b.FitCheck(ctx, backend.FitRequest{Preset: "ngram", Node: "gb10"})
	require.NoError(t, err)
	assert.True(t, res.Fits, res.Reason)
	assert.Contains(t, res.Reason, "vLLM claims 97.4 GiB at start (--gpu-memory-utilization=0.8 of the node's 121.7 GiB unified memory), within")
}

// A split's claim on each unified-memory node is judged against that node's
// share: half the weights beside the whole overhead.
func TestSplitClaimHoldsItsShareOnUnifiedMemory(t *testing.T) {
	ctx := context.Background()
	link := backend.FastLink{Name: "sparks", Nodes: []string{"gb10-a", "gb10-b"}}
	for _, tc := range []struct {
		overhead float64
		fits     bool
	}{
		{34, true},  // 36.7 + 34 = 70.7 GiB within the 0.60 claim of 73.0 GiB
		{40, false}, // 36.7 + 40 = 76.7 GiB, more than the claim
	} {
		kolibri := utilizationPreset("kolibri", 73.4, tc.overhead, "0.60")
		kolibri.Data[presetConfigKey] = strings.Replace(kolibri.Data[presetConfigKey], "storageUri: hf://", "storageUri: oci://registry.example/", 1)
		f := newFixture(t, gb10Node("gb10-a"), gb10Node("gb10-b"), kolibri)
		f.b.cfg.opts.FastLinks = []backend.FastLink{link}
		f.resetSettings()

		res, err := f.b.FitCheck(ctx, backend.FitRequest{Preset: "kolibri", Placement: backend.PlacementSplit})
		require.NoError(t, err)
		assert.Equal(t, tc.fits, res.Fits, res.Reason)
		if !tc.fits {
			assert.Contains(t, res.Reason, "less than the 76.7 GiB of weights and overhead it must hold: --gpu-memory-utilization=0.64 holds them")
		}
	}
}
