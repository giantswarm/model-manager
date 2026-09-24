package kserve

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/model-manager/internal/backend"
)

// The checkpoints' own config.json files (testdata/kvcache): Gemma 4 31B
// (RedHatAI's FP8-dynamic repack, the text config nested, sliding and full
// layers with their own heads), gpt-oss-20b (sliding and full, no dtype),
// DeepSeek-V3 (MLA), Qwen3.5 9B (linear and full attention) and Nemotron 3
// Super (Mamba, MoE and attention in a pattern, a ModelOpt FP8 KV cache;
// trimmed to the fields read).
func readKVLayout(t *testing.T, name string) kvLayout {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "kvcache", name))
	require.NoError(t, err)
	layout, err := parseKVLayout(raw)
	require.NoError(t, err)
	return layout
}

// l40sGPUMemory is what nvidia.com/gpu.memory says of an L40S: 46068 MiB.
const l40sGPUMemory = 46068 * mib

// gemmaShardBytes is the Gemma 4 31B FP8 checkpoint's two safetensors
// shards — the weights the fit check sizes it at (30.98 GiB).
const gemmaShardBytes = 26885346708 + 6382775828

func gemmaCheck(t *testing.T, maxModelLen string) *kvCheck {
	t.Helper()
	args, err := parseVLLMArgs([]string{"--gpu-memory-utilization=0.92", "--max-model-len=" + maxModelLen, "--max-num-seqs=4", "--max-num-batched-tokens=8192", "--enable-chunked-prefill"})
	require.NoError(t, err)
	return &kvCheck{Layout: readKVLayout(t, "gemma-4-31b-it-fp8-dynamic.json"), Args: args, Weights: gemmaShardBytes}
}

func TestKVLayoutOfTheCheckpoints(t *testing.T) {
	gemma := readKVLayout(t, "gemma-4-31b-it-fp8-dynamic.json")
	assert.Equal(t, "bfloat16", gemma.DType)
	assert.Equal(t, []kvLayers{{Count: 10, Heads: 4, HeadDim: 512}, {Count: 50, Heads: 16, HeadDim: 256, Window: 1024}}, gemma.Layers,
		"the full layers use the global heads, the sliding ones the window")

	gptOSS := readKVLayout(t, "gpt-oss-20b.json")
	assert.Empty(t, gptOSS.DType, "gpt-oss names no dtype")
	assert.Equal(t, []kvLayers{{Count: 12, Heads: 8, HeadDim: 64}, {Count: 12, Heads: 8, HeadDim: 64, Window: 128}}, gptOSS.Layers)

	deepseek := readKVLayout(t, "deepseek-v3.json")
	assert.Equal(t, []kvLayers{{Count: 61, Latent: 512 + 64}}, deepseek.Layers, "MLA: kv_lora_rank + qk_rope_head_dim per layer")

	qwen := readKVLayout(t, "qwen3-5-9b-fp8-dynamic.json")
	assert.Equal(t, []kvLayers{{Count: 8, Heads: 4, HeadDim: 256}}, qwen.Layers, "the 24 linear-attention layers hold no KV cache")

	nemotron := readKVLayout(t, "nemotron-3-super-nvfp4.json")
	assert.Equal(t, []kvLayers{{Count: 8, Heads: 2, HeadDim: 128}}, nemotron.Layers, "the attention layers of the hybrid pattern")
	assert.Equal(t, kvCacheDTypeFP8, nemotron.CheckpointCache, "ModelOpt's 8-bit float KV scheme is vLLM's fp8 for auto")
}

func TestKVLayoutSaysWhatItCannotRead(t *testing.T) {
	for name, cfg := range map[string]string{
		"unknown architecture":               `{"model_type":"llama4_text","num_hidden_layers":48,"num_attention_heads":40,"num_key_value_heads":8,"head_dim":128}`,
		"sliding window, no layer types":     `{"model_type":"mistral","num_hidden_layers":32,"num_attention_heads":32,"num_key_value_heads":8,"hidden_size":4096,"sliding_window":4096}`,
		"unknown layer type":                 `{"model_type":"gemma3_text","num_hidden_layers":2,"num_attention_heads":8,"num_key_value_heads":4,"head_dim":128,"sliding_window":512,"layer_types":["full_attention","chunked_attention"]}`,
		"NVFP4 KV cache":                     `{"model_type":"llama","num_hidden_layers":2,"num_attention_heads":8,"head_dim":64,"quantization_config":{"quant_method":"modelopt","kv_cache_scheme":{"num_bits":4,"type":"float"}}}`,
		"layer types of another layer count": `{"model_type":"gpt_oss","num_hidden_layers":3,"num_attention_heads":8,"head_dim":64,"sliding_window":128,"layer_types":["full_attention"]}`,
	} {
		_, err := parseKVLayout([]byte(cfg))
		assert.Error(t, err, name)
	}
	// A sliding_window the model does not use is no sliding layer (Qwen2).
	layout, err := parseKVLayout([]byte(`{"model_type":"qwen2","num_hidden_layers":28,"num_attention_heads":28,"num_key_value_heads":4,"hidden_size":3584,"sliding_window":131072,"use_sliding_window":false,"torch_dtype":"bfloat16"}`))
	require.NoError(t, err)
	assert.Equal(t, []kvLayers{{Count: 28, Heads: 4, HeadDim: 128}}, layout.Layers)
}

// The acceptance of giantswarm/model-manager#149: vLLM v0.23 on one L40S at
// --gpu-memory-utilization=0.92 measured 12.04 GiB of KV cache for Gemma 4
// 31B at 64k and 9.54 GiB at 32k against 7.25 GiB available, and estimated
// the maximum model length at 8624; 8192 serves.
func TestKVCheckReproducesVLLMOnGemma4(t *testing.T) {
	for _, tc := range []struct {
		maxModelLen string
		needGiB     float64
		fits        bool
	}{
		{"65536", 12.04, false},
		{"32768", 9.54, false},
		{"8192", 6.89, true},
	} {
		v := gemmaCheck(t, tc.maxModelLen).judge(l40sGPUMemory, "NVIDIA-L40S")
		require.Empty(t, v.Skip)
		assert.InDelta(t, tc.needGiB, float64(v.Need)/float64(gib), 0.005, tc.maxModelLen)
		assert.InDelta(t, 7.25, float64(v.Available)/float64(gib), 0.01, "available beside 31.0 GiB of weights")
		assert.Equal(t, tc.fits, v.Fits, tc.maxModelLen)
		if tc.fits {
			assert.Zero(t, v.Estimated)
			assert.Contains(t, v.clause(), "the KV cache of one 8192-token sequence (6.9 GiB) fits the 7.3 GiB left on the 45.0 GiB GPU at --gpu-memory-utilization=0.92")
			continue
		}
		assert.EqualValues(t, 8624, v.Estimated, "vLLM's estimate")
		assert.Contains(t, v.clause(), "needs "+humanBytes(v.Need)+", more than the 7.3 GiB left")
		assert.Contains(t, v.clause(), "the estimated maximum model length is 8624 tokens")
	}
	// An fp8 KV cache halves the need: 32k fits.
	k := gemmaCheck(t, "32768")
	k.Args.KVCacheDType = kvCacheDTypeFP8
	assert.True(t, k.judge(l40sGPUMemory, "NVIDIA-L40S").Fits)
}

func TestKVCheckSplitsHeadsAcrossTensorParallelGPUs(t *testing.T) {
	args, err := parseVLLMArgs([]string{"--tensor-parallel-size", "4", "--max-model-len", "128K"})
	require.NoError(t, err)
	assert.EqualValues(t, 131072, args.MaxModelLen, "K is 1024")
	k := &kvCheck{Layout: readKVLayout(t, "gpt-oss-20b.json"), Args: args}
	// Two KV heads per GPU of 64 × 2 bytes, K and V: 512 bytes per token and
	// layer; 12 full layers hold 131072 tokens, 12 sliding ones the window
	// and one 2048-token chunk (the default below 70 GiB), in 16-token blocks.
	full := int64(12 * 131072 * 512)
	sliding := int64(12 * (ceilDiv(128-1+2048, 16) + 1) * 16 * 512)
	assert.Equal(t, full+sliding, k.judge(l40sGPUMemory, "").Need)
	// On a GPU of 70 GiB or more the chunk is 8192 tokens, except on an A100.
	large := int64(80 * gib)
	assert.Equal(t, full+12*(ceilDiv(128-1+8192, 16)+1)*16*512, k.judge(large, "NVIDIA-H100-80GB-HBM3").Need)
	assert.Equal(t, full+sliding, k.judge(large, "NVIDIA-A100-SXM4-80GB").Need)

	// An MLA latent is whole on every GPU.
	mla := &kvCheck{Layout: readKVLayout(t, "deepseek-v3.json"), Args: args}
	assert.Equal(t, int64(61*131072*576*2), mla.judge(large, "").Need)
}

func TestKVCheckReadsTheCheckpointsKVCacheDType(t *testing.T) {
	args, err := parseVLLMArgs([]string{"--max-model-len=8192"})
	require.NoError(t, err)
	k := &kvCheck{Layout: readKVLayout(t, "nemotron-3-super-nvfp4.json"), Args: args}
	assert.Equal(t, int64(8*8192*2*2*128), k.judge(l40sGPUMemory, "").Need, "fp8 from the checkpoint: one byte per element")
	k.Args.KVCacheDType = "bfloat16"
	assert.Equal(t, int64(8*8192*2*2*128*2), k.judge(l40sGPUMemory, "").Need)
}

func TestVLLMArgsTheCheckCannotJudge(t *testing.T) {
	for name, args := range map[string][]string{
		"no max-model-len":   {"--gpu-memory-utilization=0.9"},
		"auto":               {"--max-model-len=auto"},
		"pipeline parallel":  {"--max-model-len=8192", "--pipeline-parallel-size=2"},
		"utilization over 1": {"--max-model-len=8192", "--gpu-memory-utilization=1.5"},
	} {
		_, err := parseVLLMArgs(args)
		assert.Error(t, err, name)
	}
	k := &kvCheck{Layout: kvLayout{Layers: []kvLayers{{Count: 1, Heads: 1, HeadDim: 1}}}, Args: vllmArgs{MaxModelLen: 1, KVCacheDType: "nvfp4", BlockSize: 16, TensorParallel: 1, ChunkedPrefill: true}}
	assert.Contains(t, k.judge(l40sGPUMemory, "").Skip, "--kv-cache-dtype=nvfp4")
	assert.Contains(t, (&kvCheck{}).judge(0, "").Skip, labelGPUMemory, "a node without the GPU memory label")
	args, err := parseVLLMArgs([]string{"--max_model_len", "25.6k", "--no-enable-chunked-prefill", "--kv-cache-dtype", "fp8"})
	require.NoError(t, err)
	assert.Equal(t, vllmArgs{MaxModelLen: 25600, Utilization: 0.9, KVCacheDType: "fp8", DType: "auto", BlockSize: 16, TensorParallel: 1}, args)
}

func gemmaPresetDoc(name, maxModelLen string) string {
	return fmt.Sprintf(`apiVersion: agent-platform.giantswarm.io/v1alpha1
kind: ServingPreset
metadata:
  name: %s
spec:
  displayName: Gemma 4 31B
  model:
    id: %s
    storageUri: oci://registry.example/models/gemma-4-31b-fp8:d4ab4f579dd3
    format: vLLM
  args:
    - --gpu-memory-utilization=0.92
    - --max-model-len=%s
    - --max-num-batched-tokens=8192
  resources:
    gpus: 1
  requirements:
    weightsGiB: 31
    overheadGiB: 13
`, name, gemmaRepo, maxModelLen)
}

// check_fit on one L40S node refuses the contexts vLLM refuses, naming the
// KV cache it needs and the maximum model length that fits, and passes 8k.
func TestFitCheckJudgesTheKVCacheOnTheNode(t *testing.T) {
	const l40s = "l40s"
	f := newFixture(t,
		withGPUs(node(l40s, "128Gi", map[string]string{labelGPUCount: "1", labelGPUMemory: "46068", labelGPUProduct: "NVIDIA-L40S"}), 1),
		presetConfigMap("gemma-64k", gemmaPresetDoc("gemma-64k", "65536")),
		presetConfigMap("gemma-32k", gemmaPresetDoc("gemma-32k", "32768")),
		presetConfigMap("gemma-8k", gemmaPresetDoc("gemma-8k", "8192")),
	)
	ctx := context.Background()
	for _, tc := range []struct {
		preset, kv string
		fits       bool
	}{
		{"gemma-64k", "12.0 GiB", false},
		{"gemma-32k", "9.5 GiB", false},
		{"gemma-8k", "6.9 GiB", true},
	} {
		res, err := f.b.FitCheck(ctx, backend.FitRequest{Preset: tc.preset, Node: l40s})
		require.NoError(t, err)
		assert.Equal(t, weightsSourceShards, res.WeightsSource)
		assert.True(t, res.RequiredBytes <= res.BudgetBytes, "the flat overhead alone passes: %s", res.Reason)
		assert.Equal(t, tc.fits, res.Fits, res.Reason)
		assert.Equal(t, tc.kv, humanBytes(res.KVCacheBytes), tc.preset)
		assert.Equal(t, "7.3 GiB", humanBytes(res.KVCacheAvailableBytes))
		if tc.fits {
			assert.Zero(t, res.EstimatedMaxModelLen)
			assert.Contains(t, res.Reason, "; the KV cache of one 8192-token sequence (6.9 GiB) fits the 7.3 GiB left on the 45.0 GiB GPU")
			continue
		}
		assert.EqualValues(t, 8624, res.EstimatedMaxModelLen)
		assert.Contains(t, res.Reason, "fit within 45.0 GiB on l40s (gpu-labels), but the KV cache of one")
		assert.Contains(t, res.Reason, "needs "+tc.kv+", more than the 7.3 GiB left on the 45.0 GiB GPU at --gpu-memory-utilization=0.92 beside the weights and vLLM's 3.15 GiB reserve: the estimated maximum model length is 8624 tokens")
	}
}

// A GPU pool's size hosts the predictor only when one sequence of its KV
// cache fits a GPU of the size: gpuMemoryGiB is the nominal decimal size.
func TestPoolSizesJudgeTheKVCache(t *testing.T) {
	shape := backend.InstanceShape{InstanceType: "g6e.2xlarge", VCPU: 8, MemoryGiB: 64, GPUs: 1, GPUMemoryGiB: 48, UsableVCPU: 7, UsableMemoryGiB: 58}
	assert.EqualValues(t, 48e9, shapeGPUMemory(shape))
	needs := predictorNeeds{VCPU: 4, MemoryGiB: 32, GPUs: 1, GPUMemoryGiB: 44, KV: gemmaCheck(t, "65536")}
	assert.False(t, hosts(shape, needs))
	needs.KV = gemmaCheck(t, "8192")
	assert.True(t, hosts(shape, needs))
	needs.KV = &kvCheck{Skip: "the checkpoint has no config.json"}
	assert.True(t, hosts(shape, needs), "a KV cache the check cannot read leaves the flat overhead to judge")
}
