package kserve

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/model-manager/internal/backend"
)

// The shipped checkpoints' config.json files (testdata/kvcache) and what
// their weights need of the GPU: FP8 weights (RedHatAI's compressed-tensors
// repacks, DeepSeek's fp8 method) compute capability 8.9; int4 and MXFP4
// weights run in the model's dtype (bf16: 8.0; gpt-oss names no dtype); the
// ModelOpt checkpoints say MIXED_PRECISION and nothing more.
func TestComputeNeedOfTheCheckpoints(t *testing.T) {
	for _, tc := range []struct {
		file, dtype, capability, why, skip string
	}{
		{"qwen3-5-9b-fp8-dynamic.json", "", "8.9", "FP8 weights (compressed-tensors)", ""},
		{"gemma-4-31b-it-fp8-dynamic.json", "", "8.9", "FP8 weights (compressed-tensors)", ""},
		{"deepseek-v3.json", "", "8.9", "FP8 weights (quant_method fp8)", ""},
		{"gemma-4-12b-it-qat-w4a16.json", "", "8.0", "bf16 weights", ""},
		{"gemma-4-12b-it-qat-w4a16.json", "float16", "", "", "float16 weights need no particular GPU generation"},
		{"gpt-oss-20b.json", "", "", "", "config.json names no dtype"},
		{"nemotron-3-super-nvfp4.json", "", "", "", "the ModelOpt checkpoint quantizes its layers in mixed precision and config.json does not say to what"},
		{"qwen3-8-flash-next-nvfp4.json", "", "", "", "the ModelOpt checkpoint quantizes its layers in mixed precision and config.json does not say to what"},
	} {
		raw, err := kvConfig(tc.file)
		require.NoError(t, err)
		need := computeNeedOf(raw, tc.dtype)
		assert.Equal(t, computeNeed{Capability: tc.capability, Why: tc.why, Skip: tc.skip}, need, "%s --dtype=%q", tc.file, tc.dtype)
	}
}

func TestComputeNeedOfTheQuantizationSchemes(t *testing.T) {
	for name, tc := range map[string]struct {
		config, dtype, capability, why, skip string
	}{
		"ModelOpt NVFP4":                  {`{"torch_dtype":"bfloat16","quantization_config":{"quant_method":"modelopt","quant_algo":"NVFP4"}}`, "", "10.0", "NVFP4 weights (ModelOpt)", ""},
		"ModelOpt FP8":                    {`{"torch_dtype":"bfloat16","quantization_config":{"quant_method":"modelopt","quant_algo":"FP8"}}`, "", "8.9", "FP8 weights (ModelOpt)", ""},
		"ModelOpt mixed, NVFP4 layers":    {`{"torch_dtype":"bfloat16","quantization_config":{"quant_method":"modelopt","quant_algo":"MIXED_PRECISION","quantized_layers":{"model.layers.0.mlp":{"quant_algo":"NVFP4"},"model.layers.1.mlp":{"quant_algo":"FP8"}}}}`, "", "10.0", "NVFP4 weights (ModelOpt)", ""},
		"compressed-tensors FP4":          {`{"torch_dtype":"bfloat16","quantization_config":{"quant_method":"compressed-tensors","config_groups":{"group_0":{"weights":{"type":"float","num_bits":4}}}}}`, "", "10.0", "FP4 weights (compressed-tensors)", ""},
		"compressed-tensors int8 in bf16": {`{"torch_dtype":"bfloat16","quantization_config":{"quant_method":"compressed-tensors","config_groups":{"group_0":{"weights":{"type":"int","num_bits":8}}}}}`, "", "8.0", "bf16 weights", ""},
		"AWQ in fp16":                     {`{"torch_dtype":"float16","quantization_config":{"quant_method":"awq","bits":4}}`, "", "", "", "float16 weights need no particular GPU generation"},
		"bf16, --dtype=bfloat16":          {`{"torch_dtype":"float16"}`, "bfloat16", "8.0", "bf16 weights", ""},
		"bf16, --dtype=auto":              {`{"dtype":"bfloat16"}`, "auto", "8.0", "bf16 weights", ""},
		"no config.json":                  {``, "", "", "", "the checkpoint has no config.json"},
		"not JSON":                        {`{`, "", "", "", "config.json does not parse: unexpected end of JSON input"},
	} {
		need := computeNeedOf([]byte(tc.config), tc.dtype)
		assert.Equal(t, computeNeed{Capability: tc.capability, Why: tc.why, Skip: tc.skip}, need, name)
	}
}

func TestComputeJudgesTheShapeAndTheNode(t *testing.T) {
	fp8 := &computeNeed{Capability: "8.9", Why: "FP8 weights (compressed-tensors)"}
	bf16 := &computeNeed{Capability: "8.0", Why: "bf16 weights"}
	shape := func(instanceType string) backend.InstanceShape {
		return backend.InstanceShape{InstanceType: instanceType, VCPU: 4, MemoryGiB: 16, GPUs: 1, GPUMemoryGiB: 24, UsableVCPU: 3, UsableMemoryGiB: 11.9}
	}

	v := fp8.judgeShape(shape("g5.xlarge"))
	assert.False(t, v.Fits)
	assert.Equal(t, "8.6", v.Capability)
	assert.Equal(t, "its FP8 weights (compressed-tensors) need compute capability 8.9 and the A10G of a g5.xlarge (Ampere) has 8.6: without native FP8 vLLM falls back to the weight-only FP8 Marlin kernel, which answers wrong for such a checkpoint; serve it on a GPU of 8.9 or above (Ada, Hopper, Blackwell) or serve a BF16 checkpoint here", v.clause())
	v = fp8.judgeShape(shape("g6.xlarge"))
	assert.True(t, v.Fits)
	assert.Equal(t, "its FP8 weights (compressed-tensors) need compute capability 8.9 and the L4 of a g6.xlarge (Ada) has 8.9", v.clause())
	assert.True(t, fp8.judgeShape(shape("g6e.2xlarge")).Fits)
	assert.True(t, fp8.judgeShape(shape("p6-b200.48xlarge")).Fits, "10.0 orders above 8.9")
	assert.False(t, fp8.judgeShape(shape("g4dn.xlarge")).Fits)
	assert.False(t, bf16.judgeShape(shape("g4dn.xlarge")).Fits, "a T4 has no bf16")
	assert.Contains(t, bf16.judgeShape(shape("g4dn.xlarge")).clause(), "the T4 of a g4dn.xlarge (Turing) has 7.5: bf16 needs a GPU of 8.0 or above")
	assert.True(t, bf16.judgeShape(shape("g5.xlarge")).Fits)
	v = fp8.judgeShape(shape("g7.xlarge"))
	assert.Equal(t, "the GPU generation of a g7.xlarge is unknown to the check", v.Skip)
	assert.Equal(t, "its FP8 weights (compressed-tensors) need compute capability 8.9; the GPU generation of a g7.xlarge is unknown to the check", v.clause())

	a10g := nodeBudget{Name: "a10g", GPUProduct: "NVIDIA-A10G", Labels: map[string]string{labelGPUComputeMajor: "8", labelGPUComputeMinor: "6"}}
	v = fp8.judgeNode(a10g)
	assert.False(t, v.Fits)
	assert.Contains(t, v.clause(), "need compute capability 8.9 and the NVIDIA-A10G of a10g has 8.6: without native FP8")
	assert.True(t, bf16.judgeNode(a10g).Fits)
	l4 := nodeBudget{Name: "l4", Labels: map[string]string{labelGPUComputeMajor: "8", labelGPUComputeMinor: "9"}}
	v = fp8.judgeNode(l4)
	assert.True(t, v.Fits)
	assert.Equal(t, "its FP8 weights (compressed-tensors) need compute capability 8.9 and the GPU of l4 has 8.9", v.clause())
	v = fp8.judgeNode(nodeBudget{Name: "bare"})
	assert.Equal(t, "the GPU generation of node bare is unknown (no nvidia.com/gpu.compute.major/nvidia.com/gpu.compute.minor label)", v.Skip)
	assert.Equal(t, "8.9", v.Required, "the need stands in the answer")

	var none *computeNeed
	v = none.judgeNode(a10g)
	assert.Equal(t, "no checkpoint config to read the weights' precision from", v.Skip)
	assert.Empty(t, v.clause(), "a need the check cannot read says nothing")
	skipped := &computeNeed{Skip: "config.json names no dtype"}
	assert.Equal(t, "config.json names no dtype", skipped.judgeShape(shape("g5.xlarge")).Skip)
	assert.Empty(t, skipped.judgeShape(shape("g5.xlarge")).clause())
}

func TestPoolSizesJudgeTheGPUGeneration(t *testing.T) {
	a10g := backend.InstanceShape{InstanceType: "g5.xlarge", VCPU: 4, MemoryGiB: 16, GPUs: 1, GPUMemoryGiB: 24, UsableVCPU: 3, UsableMemoryGiB: 11.9}
	l4 := backend.InstanceShape{InstanceType: "g6.xlarge", VCPU: 4, MemoryGiB: 16, GPUs: 1, GPUMemoryGiB: 24, UsableVCPU: 3, UsableMemoryGiB: 11.9}
	needs := predictorNeeds{VCPU: 2, MemoryGiB: 10, GPUs: 1, GPUMemoryGiB: 22, Compute: &computeNeed{Capability: "8.9", Why: "FP8 weights (compressed-tensors)"}}
	assert.False(t, hosts(a10g, needs), "FP8 weights need native FP8")
	assert.True(t, hosts(l4, needs))
	needs.Compute = &computeNeed{Skip: "the checkpoint has no config.json"}
	assert.True(t, hosts(a10g, needs), "a precision the check cannot read leaves the memory to judge")
	needs.Compute = nil
	assert.True(t, hosts(a10g, needs))
}

// fp8PresetDoc is the shipped qwen3-5-9b-fp8 preset as far as the fit reads
// it: served from a model image, 32k context at 0.90, 14 + 9 GiB declared.
func fp8PresetDoc(name, image string) string {
	return fmt.Sprintf(`apiVersion: agent-platform.giantswarm.io/v1alpha1
kind: ServingPreset
metadata:
  name: %s
spec:
  displayName: Qwen3.5 9B (FP8, one 24 GB GPU)
  model:
    id: RedHatAI/Qwen3.5-9B-FP8-dynamic
    storageUri: %s
    format: vLLM
  args:
    - --gpu-memory-utilization=0.90
    - --max-model-len=32768
    - --max-num-seqs=4
  resources:
    gpus: 1
    requests: {cpu: "2", memory: 10Gi}
    limits: {cpu: "4", memory: 12Gi}
  requirements:
    weightsGiB: 14
    overheadGiB: 9
`, name, image)
}

// A pool with no node yet: the FP8 preset fits a g5.xlarge's memory and KV
// cache, but the A10G has no native FP8 — refused, naming the way out; a
// g6.xlarge (L4) hosts it; among both pools the L4 pool is the one chosen.
func TestFitCheckRefusesFP8WeightsOnAnAmperePool(t *testing.T) {
	config, err := kvConfig("qwen3-5-9b-fp8-dynamic.json")
	require.NoError(t, err)
	image := serveModelImage(t, "models/qwen3-5-9b-fp8:790f0576d2d7", 13*gib, config)
	f := newFixture(t, presetConfigMap("qwen3-5-9b-fp8", fp8PresetDoc("qwen3-5-9b-fp8", image)))
	ctx := context.Background()
	a10g := backend.InstanceShape{InstanceType: "g5.xlarge", Size: "xlarge", VCPU: 4, MemoryGiB: 16, GPUs: 1, GPUMemoryGiB: 24, UsableVCPU: 3, UsableMemoryGiB: 11.9}

	f.setPool(ctx, a10g)
	res, err := f.b.FitCheck(ctx, backend.FitRequest{Preset: "qwen3-5-9b-fp8"})
	require.NoError(t, err)
	assert.False(t, res.Fits, res.Reason)
	assert.Equal(t, "8.9", res.ComputeCapabilityRequired)
	assert.Equal(t, "8.6", res.ComputeCapability)
	assert.Contains(t, res.Reason, "no size of the pool (xlarge) hosts preset qwen3-5-9b-fp8")
	assert.Contains(t, res.Reason, "; its FP8 weights (compressed-tensors) need compute capability 8.9 and the A10G of a g5.xlarge (Ampere) has 8.6: without native FP8 vLLM falls back to the weight-only FP8 Marlin kernel, which answers wrong for such a checkpoint; serve it on a GPU of 8.9 or above (Ada, Hopper, Blackwell) or serve a BF16 checkpoint here")
	assert.Zero(t, servingObjects(t, f, ctx))

	f.setPool(ctx, shapeXLarge)
	res, err = f.b.FitCheck(ctx, backend.FitRequest{Preset: "qwen3-5-9b-fp8"})
	require.NoError(t, err)
	assert.True(t, res.Fits, res.Reason)
	assert.Equal(t, "g6.xlarge", res.InstanceType)
	assert.Equal(t, "8.9", res.ComputeCapability)
	assert.Contains(t, res.Reason, "; its FP8 weights (compressed-tensors) need compute capability 8.9 and the L4 of a g6.xlarge (Ada) has 8.9")
}

// On nodes: the A10G's compute capability labels refuse the FP8 preset
// (its memory and KV cache would pass), the L4's take it, and bf16 weights
// (the int4 Gemma 12B QAT, run in bf16) are refused on a T4 and taken on
// the A10G.
func TestFitCheckRefusesFP8WeightsOnAnAmpereNode(t *testing.T) {
	fp8Config, err := kvConfig("qwen3-5-9b-fp8-dynamic.json")
	require.NoError(t, err)
	bf16Config, err := kvConfig("gemma-4-12b-it-qat-w4a16.json")
	require.NoError(t, err)
	fp8Image := serveModelImage(t, "models/qwen3-5-9b-fp8:790f0576d2d7", 13*gib, fp8Config)
	bf16Image := serveModelImage(t, "models/gemma-4-12b-qat:0a5c47e8e5b7", 8*gib, bf16Config)
	gpu := func(product, major, minor string) map[string]string {
		return map[string]string{labelGPUCount: "1", labelGPUMemory: "23028", labelGPUProduct: product, labelGPUComputeMajor: major, labelGPUComputeMinor: minor}
	}
	f := newFixture(t,
		withGPUs(node("a10g", "16Gi", gpu("NVIDIA-A10G", "8", "6")), 1),
		withGPUs(node("l4", "16Gi", gpu("NVIDIA-L4", "8", "9")), 1),
		withGPUs(node("t4", "16Gi", gpu("Tesla-T4", "7", "5")), 1),
		presetConfigMap("qwen3-5-9b-fp8", fp8PresetDoc("qwen3-5-9b-fp8", fp8Image)),
		presetConfigMap("gemma-4-12b", gemmaPresetDoc("gemma-4-12b", "8192", bf16Image)),
	)
	ctx := context.Background()

	res, err := f.b.FitCheck(ctx, backend.FitRequest{Preset: "qwen3-5-9b-fp8", Node: "a10g"})
	require.NoError(t, err)
	assert.False(t, res.Fits, res.Reason)
	assert.True(t, res.RequiredBytes <= res.BudgetBytes, "the memory alone passes: %s", res.Reason)
	assert.Equal(t, "8.9", res.ComputeCapabilityRequired)
	assert.Equal(t, "8.6", res.ComputeCapability)
	assert.Contains(t, res.Reason, ", but its FP8 weights (compressed-tensors) need compute capability 8.9 and the NVIDIA-A10G of a10g has 8.6: without native FP8 vLLM falls back")

	res, err = f.b.FitCheck(ctx, backend.FitRequest{Preset: "qwen3-5-9b-fp8", Node: "l4"})
	require.NoError(t, err)
	assert.True(t, res.Fits, res.Reason)
	assert.Contains(t, res.Reason, "; its FP8 weights (compressed-tensors) need compute capability 8.9 and the NVIDIA-L4 of l4 has 8.9")

	res, err = f.b.FitCheck(ctx, backend.FitRequest{Preset: "qwen3-5-9b-fp8"})
	require.NoError(t, err)
	assert.True(t, res.Fits, res.Reason)
	assert.Equal(t, "l4", res.Node, "the smallest node that hosts it, the A10G passed over")

	res, err = f.b.FitCheck(ctx, backend.FitRequest{Preset: "gemma-4-12b", Node: "t4"})
	require.NoError(t, err)
	assert.False(t, res.Fits, res.Reason)
	assert.Contains(t, res.Reason, ", but its bf16 weights need compute capability 8.0 and the Tesla-T4 of t4 has 7.5: bf16 needs a GPU of 8.0 or above")

	res, err = f.b.FitCheck(ctx, backend.FitRequest{Preset: "gemma-4-12b", Node: "a10g"})
	require.NoError(t, err)
	assert.True(t, res.Fits, res.Reason)
	assert.Contains(t, res.Reason, "; its bf16 weights need compute capability 8.0 and the NVIDIA-A10G of a10g has 8.6")
}
