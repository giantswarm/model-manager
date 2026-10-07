package kserve

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/model-manager/internal/backend"
)

// The shipped checkpoints' config.json files (testdata/kvcache) and what
// their weights need of the GPU: FP8 weights (RedHatAI's compressed-tensors
// repacks, DeepSeek's fp8 method) compute capability 8.9; int4 and MXFP4
// weights run in the model's dtype (bf16: 8.0; gpt-oss names no dtype);
// NVFP4 weights 8.9 through the FP4 Marlin kernel, the ModelOpt mixed
// precision of Nemotron 3 Super read from its config_groups (FP8 and NVFP4),
// Inferact's pure NVFP4 from its quant_algo; the Flash Next checkpoint says
// MIXED_PRECISION and nothing more.
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
		{"nemotron-3-super-nvfp4.json", "", "8.9", "NVFP4 weights (ModelOpt)", ""},
		{"qwen3-8-27b-nvfp4-inferact.json", "", "8.9", "NVFP4 weights (ModelOpt)", ""},
		{"qwen3-8-flash-next-nvfp4.json", "", "", "", "the ModelOpt checkpoint quantizes its layers in mixed precision and config.json does not say to what"},
	} {
		raw, err := kvConfig(tc.file)
		require.NoError(t, err)
		assertNeed(t, computeNeedOf(raw, tc.dtype), tc.capability, tc.why, tc.skip, "%s --dtype=%q", tc.file, tc.dtype)
	}
}

// assertNeed compares a need by what it says: the capability, why, skip.
func assertNeed(t *testing.T, need computeNeed, capability, why, skip string, msg ...any) {
	t.Helper()
	assert.Equal(t, [3]string{capability, why, skip}, [3]string{need.Capability, need.Why, need.Skip}, msg...)
}

func TestComputeNeedOfTheQuantizationSchemes(t *testing.T) {
	for name, tc := range map[string]struct {
		config, dtype, capability, why, skip string
	}{
		"ModelOpt NVFP4":                  {`{"torch_dtype":"bfloat16","quantization_config":{"quant_method":"modelopt","quant_algo":"NVFP4"}}`, "", "8.9", "NVFP4 weights (ModelOpt)", ""},
		"ModelOpt FP8":                    {`{"torch_dtype":"bfloat16","quantization_config":{"quant_method":"modelopt","quant_algo":"FP8"}}`, "", "8.9", "FP8 weights (ModelOpt)", ""},
		"ModelOpt mixed, NVFP4 layers":    {`{"torch_dtype":"bfloat16","quantization_config":{"quant_method":"modelopt","quant_algo":"MIXED_PRECISION","quantized_layers":{"model.layers.0.mlp":{"quant_algo":"NVFP4"},"model.layers.1.mlp":{"quant_algo":"FP8"}}}}`, "", "8.9", "NVFP4 weights (ModelOpt)", ""},
		"ModelOpt mixed, FP8 layers only": {`{"torch_dtype":"bfloat16","quantization_config":{"quant_method":"modelopt","quant_algo":"MIXED_PRECISION","quantized_layers":{"model.layers.0.mlp":{"quant_algo":"FP8"}}}}`, "", "8.9", "FP8 weights (ModelOpt)", ""},
		// nvidia/Qwen3.8-27B-NVFP4: FP8 attention, NVFP4 MLPs and head, as
		// config_groups beside the per-layer algorithms.
		"ModelOpt mixed, config_groups":   {`{"dtype":"bfloat16","quantization_config":{"quant_method":"modelopt","quant_algo":"MIXED_PRECISION","config_groups":{"group_0":{"weights":{"type":"float","num_bits":8},"targets":["model.language_model.layers.0.linear_attn.in_proj_qkv"]},"group_1":{"weights":{"type":"float","num_bits":4,"group_size":16},"targets":["lm_head"]}},"quantized_layers":{"model.language_model.layers.0.linear_attn.in_proj_qkv":{"quant_algo":"FP8"}}}}`, "", "8.9", "NVFP4 weights (ModelOpt)", ""},
		"ModelOpt mixed, FP8 group only":  {`{"dtype":"bfloat16","quantization_config":{"quant_method":"modelopt","quant_algo":"MIXED_PRECISION","config_groups":{"group_0":{"weights":{"type":"float","num_bits":8}}}}}`, "", "8.9", "FP8 weights (ModelOpt)", ""},
		"compressed-tensors FP4":          {`{"torch_dtype":"bfloat16","quantization_config":{"quant_method":"compressed-tensors","config_groups":{"group_0":{"weights":{"type":"float","num_bits":4}}}}}`, "", "8.9", "FP4 weights (compressed-tensors)", ""},
		"compressed-tensors FP8 and FP4":  {`{"torch_dtype":"bfloat16","quantization_config":{"quant_method":"compressed-tensors","config_groups":{"a":{"weights":{"type":"float","num_bits":8}},"b":{"weights":{"type":"float","num_bits":4}},"c":{"weights":{"type":"float","num_bits":8}}}}}`, "", "8.9", "FP4 weights (compressed-tensors)", ""},
		"compressed-tensors int8 in bf16": {`{"torch_dtype":"bfloat16","quantization_config":{"quant_method":"compressed-tensors","config_groups":{"group_0":{"weights":{"type":"int","num_bits":8}}}}}`, "", "8.0", "bf16 weights", ""},
		"AWQ in fp16":                     {`{"torch_dtype":"float16","quantization_config":{"quant_method":"awq","bits":4}}`, "", "", "", "float16 weights need no particular GPU generation"},
		"bf16, --dtype=bfloat16":          {`{"torch_dtype":"float16"}`, "bfloat16", "8.0", "bf16 weights", ""},
		"bf16, --dtype=auto":              {`{"dtype":"bfloat16"}`, "auto", "8.0", "bf16 weights", ""},
		"no config.json":                  {``, "", "", "", "the checkpoint has no config.json"},
		"not JSON":                        {`{`, "", "", "", "config.json does not parse: unexpected end of JSON input"},
	} {
		for range 5 { // the groups are a map: the highest wins every time
			assertNeed(t, computeNeedOf([]byte(tc.config), tc.dtype), tc.capability, tc.why, tc.skip, name)
		}
	}
}

// A mistral-format checkpoint ships params.json in place of config.json:
// Mistral Small 4's NVFP4A16 (compressed-tensors) needs 8.9 like the
// config.json shape; the older qformat_weight is read too, and unquantized
// weights take the preset's --dtype, params.json naming none.
func TestComputeNeedOfParams(t *testing.T) {
	raw, err := kvConfig("mistral-small-4-nvfp4.params.json")
	require.NoError(t, err)
	need := computeNeedOfParams(raw, "")
	assertNeed(t, need, "8.9", "FP4 weights (compressed-tensors)", "")
	assert.Equal(t, precisionFP4, need.Precision)

	for name, tc := range map[string]struct {
		params, dtype, capability, why, skip string
	}{
		"qformat fp8":       {`{"dim":4096,"quantization":{"qformat_weight":"fp8_e4m3","qscheme_act":"TENSOR"}}`, "", "8.9", "FP8 weights (params.json qformat_weight fp8_e4m3)", ""},
		"qformat nvfp4":     {`{"dim":4096,"quantization":{"qformat_weight":"nvfp4"}}`, "", "8.9", "FP4 weights (params.json qformat_weight nvfp4)", ""},
		"unquantized, bf16": {`{"dim":4096}`, "bfloat16", "8.0", "bf16 weights", ""},
		"unquantized, auto": {`{"dim":4096}`, "auto", "", "", "params.json names no dtype"},
		"unquantized, fp16": {`{"dim":4096}`, "float16", "", "", "float16 weights need no particular GPU generation"},
		"not JSON":          {`{`, "", "", "", "params.json does not parse: unexpected end of JSON input"},
	} {
		assertNeed(t, computeNeedOfParams([]byte(tc.params), tc.dtype), tc.capability, tc.why, tc.skip, name)
	}
}

func TestComputeJudgesTheShapeAndTheNode(t *testing.T) {
	fp8 := needOf(precisionFP8, "FP8 weights (compressed-tensors)")
	bf16 := needOf(precisionBF16, "bf16 weights")
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

	// A shape declaring its compute capability is judged by it, over the
	// family table and for a family the table does not know.
	declared := func(instanceType, capability string) backend.InstanceShape {
		s := shape(instanceType)
		s.ComputeCapability = capability
		return s
	}
	v = fp8.judgeShape(declared("g5.xlarge", "8.9"))
	assert.True(t, v.Fits, "the declared value wins over the family table's 8.6")
	assert.Equal(t, "its FP8 weights (compressed-tensors) need compute capability 8.9 and the GPU of a g5.xlarge has 8.9", v.clause(), "a table that disagrees names nothing")
	v = fp8.judgeShape(declared("g5.xlarge", "8.6"))
	assert.False(t, v.Fits)
	assert.Contains(t, v.clause(), "the A10G of a g5.xlarge (Ampere) has 8.6", "a table that agrees names the GPU")
	v = fp8.judgeShape(declared("g7.xlarge", "9.0"))
	assert.True(t, v.Fits)
	assert.Equal(t, "its FP8 weights (compressed-tensors) need compute capability 8.9 and the GPU of a g7.xlarge has 9.0", v.clause())
	v = fp8.judgeShape(declared("g7.xlarge", "ampere"))
	assert.Equal(t, `the GPU of a g7.xlarge names compute capability "ampere", which does not parse`, v.Skip, "a value the document's validation would refuse is not judged")
	assert.Equal(t, "8.9", v.Required)

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

	// NVFP4 weights: the L40S (8.9) serves them through the FP4 Marlin
	// kernel, the A10G (8.6) is refused naming the real floor.
	fp4 := precisionFP4.need("NVFP4 weights (ModelOpt)")
	v = fp4.judgeShape(shape("g6e.xlarge"))
	assert.True(t, v.Fits)
	assert.Equal(t, "its NVFP4 weights (ModelOpt) need compute capability 8.9 and the L40S of a g6e.xlarge (Ada) has 8.9", v.clause())
	v = fp4.judgeShape(shape("g5.xlarge"))
	assert.False(t, v.Fits)
	assert.Equal(t, "its NVFP4 weights (ModelOpt) need compute capability 8.9 and the A10G of a g5.xlarge (Ampere) has 8.6: NVFP4 runs natively on Blackwell and through vLLM's weight-only FP4 Marlin kernel on Ada and Hopper, which is proven from 8.9 and not below it; serve it on a GPU of 8.9 or above or serve a BF16 checkpoint here", v.clause())
	assert.True(t, fp4.judgeShape(shape("p6-b200.48xlarge")).Fits)

	// The preset's declared floor is named where it differs from the
	// checkpoint's need, and only there.
	fp4.Declared = "8.9"
	assert.NotContains(t, fp4.judgeShape(shape("g6e.xlarge")).clause(), "declares")
	fp4.Declared = "10.0"
	v = fp4.judgeShape(shape("g6e.xlarge"))
	assert.True(t, v.Fits, "the checkpoint is what the check judges by")
	assert.Equal(t, "its NVFP4 weights (ModelOpt) need compute capability 8.9 and the L40S of a g6e.xlarge (Ada) has 8.9 (the preset declares requirements.minComputeCapability 10.0; the checkpoint needs 8.9)", v.clause())
	fp4.Declared = "eight"
	assert.Contains(t, fp4.judgeShape(shape("g6e.xlarge")).clause(), `(the preset declares requirements.minComputeCapability "eight", which does not parse)`)
}

func TestPoolSizesJudgeTheGPUGeneration(t *testing.T) {
	a10g := backend.InstanceShape{InstanceType: "g5.xlarge", VCPU: 4, MemoryGiB: 16, GPUs: 1, GPUMemoryGiB: 24, UsableVCPU: 3, UsableMemoryGiB: 11.9}
	l4 := backend.InstanceShape{InstanceType: "g6.xlarge", VCPU: 4, MemoryGiB: 16, GPUs: 1, GPUMemoryGiB: 24, UsableVCPU: 3, UsableMemoryGiB: 11.9}
	needs := predictorNeeds{VCPU: 2, MemoryGiB: 10, GPUs: 1, GPUMemoryGiB: 22, Compute: needOf(precisionFP8, "FP8 weights (compressed-tensors)")}
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

	// The pool's document declares the size's compute capability: the fit
	// judges by it, not by the family table (giantswarm/model-manager#266).
	a10g.ComputeCapability = "8.9"
	f.setPool(ctx, a10g)
	res, err = f.b.FitCheck(ctx, backend.FitRequest{Preset: "qwen3-5-9b-fp8"})
	require.NoError(t, err)
	assert.True(t, res.Fits, res.Reason)
	assert.Equal(t, "g5.xlarge", res.InstanceType)
	assert.Equal(t, "8.9", res.ComputeCapability)
	assert.Contains(t, res.Reason, "; its FP8 weights (compressed-tensors) need compute capability 8.9 and the GPU of a g5.xlarge has 8.9")
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

// nvfp4PresetDoc is a preset of an NVFP4 checkpoint served from a model
// image on one GPU, declaring the GPU generation it needs.
func nvfp4PresetDoc(name, id, image, minComputeCapability string) string {
	return fmt.Sprintf(`apiVersion: agent-platform.giantswarm.io/v1alpha1
kind: ServingPreset
metadata:
  name: %s
spec:
  displayName: NVFP4 on one GPU
  model:
    id: %s
    storageUri: %s
    format: vLLM
  args:
    - --gpu-memory-utilization=0.90
    - --max-model-len=8192
    - --max-num-seqs=4
  resources:
    gpus: 1
    requests: {cpu: "2", memory: 10Gi}
    limits: {cpu: "4", memory: 12Gi}
  requirements:
    weightsGiB: 12
    overheadGiB: 4
    minComputeCapability: "%s"
`, name, id, image, minComputeCapability)
}

// A pure NVFP4 checkpoint (Inferact's Qwen3.8 27B shape) and a
// mistral-format one (Mistral Small 4's params.json, no config.json): the
// L40S pool serves both through the FP4 Marlin kernel, the A10G pool is
// refused naming 8.9 and the Marlin path, and a preset declaring another
// floor than the checkpoint needs is named in the reason.
func TestFitCheckJudgesNVFP4OnTheL40S(t *testing.T) {
	config, err := kvConfig("qwen3-8-27b-nvfp4-inferact.json")
	require.NoError(t, err)
	params, err := kvConfig("mistral-small-4-nvfp4.params.json")
	require.NoError(t, err)
	pure := serveModelImage(t, "models/qwen3-8-27b-nvfp4:1", 12*gib, config)
	mistral := serveModelImageFiles(t, func(h http.Handler) http.Handler { return h }, "models/mistral-small-4-nvfp4:1", 12*gib,
		map[string][]byte{"models/tekken.json": []byte("{}"), modelImageParamsPath: params})
	f := newFixture(t,
		presetConfigMap("pure-nvfp4", nvfp4PresetDoc("pure-nvfp4", "Inferact/Qwen3.8-27B-NVFP4", pure, "8.9")),
		presetConfigMap("mistral-nvfp4", nvfp4PresetDoc("mistral-nvfp4", "mistralai/Mistral-Small-4-119B-2603-NVFP4", mistral, "8.9")),
		presetConfigMap("blackwell-nvfp4", nvfp4PresetDoc("blackwell-nvfp4", "Inferact/Qwen3.8-27B-NVFP4", pure, "10.0")),
	)
	ctx := context.Background()
	a10g := backend.InstanceShape{InstanceType: "g5.xlarge", Size: "xlarge", VCPU: 4, MemoryGiB: 16, GPUs: 1, GPUMemoryGiB: 24, UsableVCPU: 3, UsableMemoryGiB: 11.9}

	for preset, why := range map[string]string{"pure-nvfp4": "NVFP4 weights (ModelOpt)", "mistral-nvfp4": "FP4 weights (compressed-tensors)"} {
		f.setPool(ctx, shapeL40S)
		res, err := f.b.FitCheck(ctx, backend.FitRequest{Preset: preset})
		require.NoError(t, err)
		assert.True(t, res.Fits, "%s: %s", preset, res.Reason)
		assert.Equal(t, "8.9", res.ComputeCapabilityRequired, preset)
		assert.Contains(t, res.Reason, "; its "+why+" need compute capability 8.9 and the L40S of a g6e.xlarge (Ada) has 8.9", preset)
		assert.NotContains(t, res.Reason, "declares requirements.minComputeCapability", preset)

		f.setPool(ctx, a10g)
		res, err = f.b.FitCheck(ctx, backend.FitRequest{Preset: preset})
		require.NoError(t, err)
		assert.False(t, res.Fits, "%s: %s", preset, res.Reason)
		assert.Equal(t, "8.6", res.ComputeCapability, preset)
		assert.Contains(t, res.Reason, "; its "+why+" need compute capability 8.9 and the A10G of a g5.xlarge (Ampere) has 8.6: NVFP4 runs natively on Blackwell and through vLLM's weight-only FP4 Marlin kernel on Ada and Hopper, which is proven from 8.9 and not below it", preset)
		assert.Zero(t, servingObjects(t, f, ctx))
	}

	f.setPool(ctx, shapeL40S)
	res, err := f.b.FitCheck(ctx, backend.FitRequest{Preset: "blackwell-nvfp4"})
	require.NoError(t, err)
	assert.True(t, res.Fits, res.Reason)
	assert.Contains(t, res.Reason, "has 8.9 (the preset declares requirements.minComputeCapability 10.0; the checkpoint needs 8.9)")
}

// needOf is the need of weights of the precision, as the fit plan holds it.
func needOf(p weightPrecision, why string) *computeNeed {
	need := p.need(why)
	return &need
}
