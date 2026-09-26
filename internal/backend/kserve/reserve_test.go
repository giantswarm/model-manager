package kserve

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/utils/ptr"
)

// shareOf is what a utilization of the budget comes to, as presetReserve
// computes it.
func shareOf(u float64, budget int64) int64 { return int64(u * float64(budget)) }

func reservePreset(args ...string) *servingPreset {
	p := &servingPreset{}
	p.Metadata.Name = "qwen"
	p.Spec.Args = args
	p.Spec.Requirements.WeightsGiB = 28
	p.Spec.Requirements.OverheadGiB = ptr.To(20.0)
	return p
}

// A running predictor on a unified-memory node holds what vLLM claims at
// start — --gpu-memory-utilization of the node's budget — not what its
// requests say; a node whose GPUs report their own memory is judged by the
// preset's requirements (giantswarm/model-manager#18).
func TestPresetReserveCountsVLLMsShareOnUnifiedMemory(t *testing.T) {
	unified := nodeBudget{Name: "spark", GPUCount: 1, Budget: 120 * gib, BudgetSource: "allocatable"}
	discrete := nodeBudget{Name: "l40s", GPUCount: 1, GPUMemory: 48 * gib, Budget: 48 * gib}

	assert.Equal(t, shareOf(0.6, 120*gib), presetReserve(reservePreset("--gpu-memory-utilization=0.60"), unified, 30))
	assert.Equal(t, shareOf(vllmDefaultUtilization, 120*gib), presetReserve(reservePreset(), unified, 30), "vLLM's default share when the preset sets none")
	assert.Equal(t, 48*gib, presetReserve(reservePreset("--gpu-memory-utilization", "0.2"), unified, 30), "the requirements when they are the larger")
	assert.Equal(t, 48*gib, presetReserve(reservePreset("--gpu-memory-utilization=0.60"), discrete, 30))
	assert.Equal(t, 48*gib, presetReserve(reservePreset("--gpu-memory-utilization=0.60"), nodeBudget{}, 30), "a node the inventory does not know")

	cpu := reservePreset("--gpu-memory-utilization=0.60")
	cpu.Spec.Resources.GPUs = ptr.To(int64(0))
	assert.Equal(t, 48*gib, presetReserve(cpu, unified, 30), "a CPU preset claims no GPU memory")
}

func TestPresetUtilizationIsPublished(t *testing.T) {
	assert.InDelta(t, 0.6, reservePreset("--gpu_memory_utilization=0.60").view(30).GPUMemoryUtilization, 1e-9)
	assert.InDelta(t, vllmDefaultUtilization, reservePreset().view(30).GPUMemoryUtilization, 1e-9)
	assert.InDelta(t, vllmDefaultUtilization, reservePreset("--gpu-memory-utilization=1.5").view(30).GPUMemoryUtilization, 1e-9, "an unreadable value is vLLM's default")
	cpu := reservePreset()
	cpu.Spec.Resources.GPUs = ptr.To(int64(0))
	assert.Zero(t, cpu.view(30).GPUMemoryUtilization)
}
