package kserve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/giantswarm/model-manager/internal/backend"
)

// The KV cache check (giantswarm/model-manager#149). vLLM starts a model only
// when, beside the weights and what it profiles for activations, its KV cache
// holds one sequence of --max-model-len tokens; otherwise the engine refuses
// ("… GiB KV cache is needed, which is larger than the available KV cache
// memory …") and the predictor crash-loops. A preset's flat overheadGiB cannot
// say that: the KV cache grows with the context, per layer and per
// architecture. The check reproduces vLLM v0.23's admission
// (check_enough_kv_cache_memory in vllm/v1/core/kv_cache_utils.py, the
// per-layer max_memory_usage_bytes of vllm/v1/kv_cache_interface.py) from the
// checkpoint's config.json and the preset's vLLM arguments.

const (
	// vllmReserveGiB is what vLLM holds on each GPU beside the checkpoint's
	// bytes and the KV cache: the activations, CUDA graphs and runtime memory
	// it profiles at start (2.4 GiB) and what loading adds to the checkpoint
	// (31.7 GiB loaded from 31.0 GiB of safetensors) — measured with Gemma 4
	// 31B on one L40S at --gpu-memory-utilization=0.92, where it leaves the
	// 7.25 GiB of KV cache vLLM reported. One figure for every model: a
	// model that profiles more is judged optimistically by the difference.
	vllmReserveGiB = 3.15
	// vLLM's defaults for the arguments the check reads.
	vllmDefaultUtilization = 0.9
	vllmDefaultBlockSize   = 16
	// --max-num-batched-tokens defaults to 8192 on a GPU of 70 GiB or more
	// that is no A100, else 2048 (the API server's defaults,
	// EngineArgs.get_batch_defaults).
	vllmLargeGPUBytes             = 70 * gib
	vllmBatchedTokensLargeGPU     = 8192
	vllmBatchedTokensDefault      = 2048
	flagMaxModelLen               = "--max-model-len"
	flagMaxNumBatched             = "--max-num-batched-tokens"
	flagGPUMemoryUtilization      = "--gpu-memory-utilization"
	flagKVCacheDType              = "--kv-cache-dtype"
	flagDType                     = "--dtype"
	flagBlockSize                 = "--block-size"
	flagTensorParallelSize        = "--tensor-parallel-size"
	flagPipelineParallelSize      = "--pipeline-parallel-size"
	flagNoEnableChunkedPrefill    = "--no-enable-chunked-prefill"
	layerFullAttention            = "full_attention"
	layerSlidingAttention         = "sliding_attention"
	nemotronAttentionLayer        = "*"
	quantMethodModelOptPrefix     = "modelopt"
	kvCacheDTypeAuto              = "auto"
	kvCacheDTypeFP8               = "fp8"
	kvCacheDTypeCheckpointNVFP4   = "nvfp4"
	kvCacheDTypeCheckpointDefault = ""
)

// kvLayers is one kind of layer of a checkpoint and what its KV cache holds
// per token: K and V of Heads × HeadDim each, or for multi-head latent
// attention (MLA) one latent of Latent elements that every GPU holds whole.
// Window is the sliding window of a sliding-attention layer, 0 for a layer
// that holds the whole context.
type kvLayers struct {
	Count   int64
	Heads   int64
	HeadDim int64
	Latent  int64
	Window  int64
}

// kvLayout is what the KV cache of a checkpoint holds: its layers with
// attention state that grows with the context (linear-attention and Mamba
// layers hold a constant state and none of it), the dtype the model runs in
// and, from its quantization_config, the KV cache dtype vLLM picks for
// --kv-cache-dtype=auto when the checkpoint quantizes its KV cache.
type kvLayout struct {
	Layers          []kvLayers
	DType           string
	CheckpointCache string
}

// hfTextConfig is the part of a config.json (or its text_config) the layout
// is read from.
type hfTextConfig struct {
	ModelType              string   `json:"model_type"`
	NumHiddenLayers        int64    `json:"num_hidden_layers"`
	NumAttentionHeads      int64    `json:"num_attention_heads"`
	NumKeyValueHeads       *int64   `json:"num_key_value_heads"`
	HeadDim                *int64   `json:"head_dim"`
	HiddenSize             int64    `json:"hidden_size"`
	LayerTypes             []string `json:"layer_types"`
	SlidingWindow          *int64   `json:"sliding_window"`
	UseSlidingWindow       *bool    `json:"use_sliding_window"`
	NumGlobalKeyValueHeads *int64   `json:"num_global_key_value_heads"`
	GlobalHeadDim          *int64   `json:"global_head_dim"`
	NumKVSharedLayers      int64    `json:"num_kv_shared_layers"`
	KVLoraRank             *int64   `json:"kv_lora_rank"`
	QKRopeHeadDim          int64    `json:"qk_rope_head_dim"`
	HybridOverridePattern  string   `json:"hybrid_override_pattern"`
	DType                  string   `json:"dtype"`
	TorchDType             string   `json:"torch_dtype"`
}

type hfConfig struct {
	hfTextConfig
	TextConfig         *hfTextConfig   `json:"text_config"`
	QuantizationConfig json.RawMessage `json:"quantization_config"`
}

// kvModelTypes are the architectures (the text config's model_type) whose KV
// cache the layout reads from config.json, by how their layers are declared:
// every layer full attention, the layer_types list (full, sliding, and
// layers without growing state), MLA, or Nemotron-H's
// hybrid_override_pattern. Any other architecture is not guessed at.
var kvModelTypes = map[string]bool{
	"llama": true, "mistral": true, "mixtral": true, "ministral": true,
	"qwen2": true, "qwen2_moe": true, "qwen3": true, "qwen3_moe": true, "qwen3_vl_text": true, "qwen3_vl_moe_text": true,
	"qwen3_next": true, "qwen3_5_text": true, "qwen3_5_moe_text": true,
	"phi3": true, "granite": true, "granitemoe": true, "granitemoehybrid": true,
	"glm4": true, "glm4_moe": true, "olmo2": true, "olmo3": true,
	"gemma2": true, "gemma3_text": true, "gemma4_text": true, "cohere2": true,
	"gpt_oss": true, "deepseek_v2": true, "deepseek_v3": true, "kimi_k2": true,
	"nemotron_h": true, "lfm2": true,
}

// layerTypesWithoutKV are the layer_types entries that hold a constant state
// rather than a KV cache that grows with the context.
var layerTypesWithoutKV = map[string]bool{"linear_attention": true, "mamba": true, "conv": true}

// parseKVLayout reads a checkpoint's KV cache layout from its config.json;
// the error says what the check cannot read.
func parseKVLayout(raw []byte) (kvLayout, error) {
	var cfg hfConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return kvLayout{}, fmt.Errorf("config.json does not parse: %v", err)
	}
	tc := cfg.hfTextConfig
	if cfg.TextConfig != nil {
		tc = *cfg.TextConfig
	}
	if !kvModelTypes[tc.ModelType] {
		return kvLayout{}, fmt.Errorf("the architecture %q is not one whose KV cache the check reads", tc.ModelType)
	}
	layout := kvLayout{DType: firstNonEmpty(tc.DType, tc.TorchDType, cfg.DType, cfg.TorchDType)}
	var err error
	if layout.CheckpointCache, err = checkpointKVCacheDType(cfg.QuantizationConfig); err != nil {
		return kvLayout{}, err
	}
	if layout.Layers, err = kvLayersOf(tc); err != nil {
		return kvLayout{}, err
	}
	return layout, nil
}

// kvLayersOf groups the layers of a text config by what they hold.
func kvLayersOf(tc hfTextConfig) ([]kvLayers, error) {
	if tc.NumHiddenLayers <= 0 {
		return nil, errors.New("config.json names no num_hidden_layers")
	}
	if tc.KVLoraRank != nil && *tc.KVLoraRank > 0 {
		return []kvLayers{{Count: tc.NumHiddenLayers, Latent: *tc.KVLoraRank + tc.QKRopeHeadDim}}, nil
	}
	heads, headDim := derefOr(tc.NumKeyValueHeads, tc.NumAttentionHeads), derefOr(tc.HeadDim, 0)
	if headDim == 0 && tc.NumAttentionHeads > 0 {
		headDim = tc.HiddenSize / tc.NumAttentionHeads
	}
	if heads <= 0 || headDim <= 0 {
		return nil, errors.New("config.json names no attention heads or head dimension")
	}
	full := kvLayers{Heads: derefOr(tc.NumGlobalKeyValueHeads, heads), HeadDim: derefOr(tc.GlobalHeadDim, headDim)}
	sliding := kvLayers{Heads: heads, HeadDim: headDim}
	// Gemma's KV-shared layers read the cache of an earlier layer and hold
	// none of their own: the last num_kv_shared_layers.
	owning := tc.NumHiddenLayers - tc.NumKVSharedLayers
	switch {
	case tc.HybridOverridePattern != "":
		full.Count = int64(strings.Count(tc.HybridOverridePattern, nemotronAttentionLayer))
	case len(tc.LayerTypes) > 0:
		if int64(len(tc.LayerTypes)) != tc.NumHiddenLayers {
			return nil, fmt.Errorf("config.json lists %d layer_types for %d layers", len(tc.LayerTypes), tc.NumHiddenLayers)
		}
		for _, t := range tc.LayerTypes[:owning] {
			switch {
			case t == layerFullAttention || t == "attention":
				full.Count++
			case t == layerSlidingAttention:
				sliding.Count++
			case layerTypesWithoutKV[t]:
			default:
				return nil, fmt.Errorf("config.json names the layer type %q", t)
			}
		}
		if sliding.Count > 0 {
			if tc.SlidingWindow == nil || *tc.SlidingWindow <= 0 {
				return nil, errors.New("config.json has sliding-attention layers without a sliding_window")
			}
			sliding.Window = *tc.SlidingWindow
		}
	case tc.SlidingWindow != nil && *tc.SlidingWindow > 0 && (tc.UseSlidingWindow == nil || *tc.UseSlidingWindow):
		return nil, errors.New("config.json sets a sliding_window without layer_types")
	default:
		full.Count = owning
	}
	var out []kvLayers
	for _, l := range []kvLayers{full, sliding} {
		if l.Count > 0 {
			out = append(out, l)
		}
	}
	return out, nil
}

// checkpointKVCacheDType is the KV cache dtype vLLM resolves
// --kv-cache-dtype=auto to from a ModelOpt checkpoint's quantization_config
// (resolve_kv_cache_dtype_string): fp8 for an 8-bit float scheme, the
// model's dtype without one. A 4-bit scheme (NVFP4) is not a layout the
// check computes.
func checkpointKVCacheDType(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return kvCacheDTypeCheckpointDefault, nil
	}
	var q struct {
		QuantMethod      string          `json:"quant_method"`
		KVCacheScheme    json.RawMessage `json:"kv_cache_scheme"`
		KVCacheQuantAlgo json.RawMessage `json:"kv_cache_quant_algo"`
	}
	if err := json.Unmarshal(raw, &q); err != nil || !strings.HasPrefix(q.QuantMethod, quantMethodModelOptPrefix) {
		return kvCacheDTypeCheckpointDefault, nil
	}
	algo := q.KVCacheScheme
	if len(algo) == 0 || string(algo) == "null" {
		algo = q.KVCacheQuantAlgo
	}
	var scheme struct {
		Dynamic *bool  `json:"dynamic"`
		NumBits int    `json:"num_bits"`
		Type    string `json:"type"`
	}
	var name string
	switch {
	case json.Unmarshal(algo, &name) == nil:
		name = strings.ToLower(name)
	case json.Unmarshal(algo, &scheme) == nil && scheme.Type == "float":
		switch {
		case scheme.NumBits == 8 && scheme.Dynamic != nil && !*scheme.Dynamic:
			name = kvCacheDTypeFP8
		case scheme.NumBits == 4:
			name = kvCacheDTypeCheckpointNVFP4
		}
	}
	switch name {
	case kvCacheDTypeFP8:
		return kvCacheDTypeFP8, nil
	case kvCacheDTypeCheckpointNVFP4:
		return "", errors.New("the checkpoint quantizes its KV cache to NVFP4, a layout the check does not compute")
	}
	return kvCacheDTypeCheckpointDefault, nil
}

// vllmArgs are the vLLM arguments of a preset the KV cache check reads, with
// vLLM's defaults where the preset sets none (BatchedTokens 0: the GPU's
// default, known only on the GPU).
type vllmArgs struct {
	MaxModelLen    int64
	BatchedTokens  int64
	Utilization    float64
	KVCacheDType   string
	DType          string
	BlockSize      int64
	TensorParallel int64
	ChunkedPrefill bool
}

// parseVLLMArgs reads the preset's vLLM arguments (--flag=value or
// --flag value, dashes or underscores); the error says what the check cannot
// judge.
func parseVLLMArgs(args []string) (vllmArgs, error) {
	a := vllmArgs{Utilization: vllmDefaultUtilization, KVCacheDType: kvCacheDTypeAuto, DType: kvCacheDTypeAuto, BlockSize: vllmDefaultBlockSize, TensorParallel: 1, ChunkedPrefill: true}
	values := vllmFlagValues(args)
	if _, off := values[flagNoEnableChunkedPrefill]; off {
		a.ChunkedPrefill = false
	}
	raw, ok := values[flagMaxModelLen]
	if !ok {
		return a, errors.New("the preset sets no " + flagMaxModelLen + ": the context vLLM derives from the checkpoint is not judged")
	}
	var err error
	if a.MaxModelLen, err = humanReadableInt(raw); err != nil || a.MaxModelLen <= 0 {
		return a, fmt.Errorf("%s=%s is not a token count the check judges (vLLM fits auto to the memory itself)", flagMaxModelLen, raw)
	}
	for flag, dst := range map[string]*int64{flagMaxNumBatched: &a.BatchedTokens, flagBlockSize: &a.BlockSize, flagTensorParallelSize: &a.TensorParallel} {
		if v, ok := values[flag]; ok {
			if *dst, err = humanReadableInt(v); err != nil || *dst <= 0 {
				return a, fmt.Errorf("%s=%s does not parse", flag, v)
			}
		}
	}
	if v, ok := values[flagPipelineParallelSize]; ok && v != "1" {
		return a, fmt.Errorf("%s=%s splits the layers across GPUs, which the check does not judge", flagPipelineParallelSize, v)
	}
	if v, ok := values[flagGPUMemoryUtilization]; ok {
		if a.Utilization, err = parseUtilization(v); err != nil {
			return a, err
		}
	}
	if v, ok := values[flagKVCacheDType]; ok {
		a.KVCacheDType = strings.ToLower(v)
	}
	if v, ok := values[flagDType]; ok {
		a.DType = strings.ToLower(v)
	}
	return a, nil
}

// vllmFlagValues reads vLLM's command-line flags as the API server does:
// --flag=value or --flag value, dashes and underscores alike, quotes
// trimmed; a bare switch maps to "".
func vllmFlagValues(args []string) map[string]string {
	values := map[string]string{}
	for i := 0; i < len(args); i++ {
		flag, value, hasValue := strings.Cut(strings.TrimSpace(args[i]), "=")
		if !strings.HasPrefix(flag, "--") {
			continue
		}
		flag = "--" + strings.ReplaceAll(flag[2:], "_", "-")
		if !hasValue && flag != flagNoEnableChunkedPrefill && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
			i++
			value = args[i]
		}
		values[flag] = strings.Trim(strings.TrimSpace(value), `'"`)
	}
	return values
}

// parseUtilization reads a --gpu-memory-utilization value: a fraction in (0, 1].
func parseUtilization(v string) (float64, error) {
	u, err := strconv.ParseFloat(v, 64)
	if err != nil || u <= 0 || u > 1 {
		return 0, fmt.Errorf("%s=%s is not a fraction", flagGPUMemoryUtilization, v)
	}
	return u, nil
}

// humanReadableInt parses vLLM's token counts: 8192, 8k (×1000), 8K (×1024),
// 1m, 1M.
func humanReadableInt(s string) (int64, error) {
	mult := map[byte]float64{'k': 1e3, 'K': 1 << 10, 'm': 1e6, 'M': 1 << 20, 'g': 1e9, 'G': 1 << 30}
	num := s
	factor := 1.0
	if n := len(s); n > 0 {
		if f, ok := mult[s[n-1]]; ok {
			num, factor = s[:n-1], f
		}
	}
	v, err := strconv.ParseFloat(num, 64)
	if err != nil {
		return 0, err
	}
	v *= factor
	if v != math.Trunc(v) {
		return 0, fmt.Errorf("%s is not a whole number", s)
	}
	return int64(v), nil
}

// kvCheck is the KV cache check of one fit: the layout, the preset's
// arguments and the weights; Skip says why the KV cache is not checked
// (the flat overhead is then all the fit judges).
type kvCheck struct {
	Layout  kvLayout
	Args    vllmArgs
	Weights int64
	Skip    string
}

// kvVerdict is the KV cache check on one GPU: the KV cache one sequence of
// MaxModelLen tokens needs on each of its GPUs, what vLLM leaves it there
// (Available), and on a refusal the longest sequence that fits
// (Estimated, vLLM's own estimate; 0 when not even one token fits).
type kvVerdict struct {
	Skip        string
	Fits        bool
	MaxModelLen int64
	Need        int64
	Available   int64
	Estimated   int64
	GPUMemory   int64
	Utilization float64
	GPUs        int64
}

// judge runs the check against a GPU of gpuMemory bytes (product: the GPU's
// name where the node labels it, for vLLM's batch default).
func (k *kvCheck) judge(gpuMemory int64, product string) kvVerdict {
	if k == nil {
		return kvVerdict{Skip: "no preset to read vLLM's arguments from"}
	}
	if k.Skip != "" {
		return kvVerdict{Skip: k.Skip}
	}
	if gpuMemory <= 0 {
		return kvVerdict{Skip: "the node reports no GPU memory (" + labelGPUMemory + ")"}
	}
	dtypeBytes, err := k.dtypeBytes()
	if err != nil {
		return kvVerdict{Skip: err.Error()}
	}
	a := k.Args
	batched := a.BatchedTokens
	if batched == 0 {
		batched = vllmBatchedTokensDefault
		if gpuMemory >= vllmLargeGPUBytes && !strings.Contains(strings.ToLower(product), "a100") {
			batched = vllmBatchedTokensLargeGPU
		}
	}
	v := kvVerdict{MaxModelLen: a.MaxModelLen, GPUMemory: gpuMemory, Utilization: a.Utilization, GPUs: a.TensorParallel}
	v.Available = int64(a.Utilization*float64(gpuMemory)) - k.Weights/a.TensorParallel - gibToBytes(vllmReserveGiB)
	need := func(maxLen int64) int64 { return k.needBytes(maxLen, batched, dtypeBytes) }
	v.Need = need(a.MaxModelLen)
	v.Fits = v.Available > 0 && v.Need <= v.Available
	if !v.Fits && v.Available > 0 {
		// vLLM's estimate_max_model_len: the longest length that fits, by
		// binary search up to --max-model-len.
		lo, hi := int64(1), a.MaxModelLen
		for lo <= hi {
			mid := (lo + hi) / 2
			if need(mid) <= v.Available {
				v.Estimated, lo = mid, mid+1
			} else {
				hi = mid - 1
			}
		}
	}
	return v
}

// needBytes is vLLM's max_memory_usage_bytes on one GPU for one sequence of
// maxLen tokens: a full-attention layer holds the whole sequence, a
// sliding-window layer the window and one prefill chunk, in blocks
// (FullAttentionSpec, SlidingWindowSpec); tensor parallelism splits the KV
// heads, never below one per GPU, and leaves an MLA latent whole.
func (k *kvCheck) needBytes(maxLen, batched, dtypeBytes int64) int64 {
	a := k.Args
	if !a.ChunkedPrefill {
		batched = max(batched, maxLen)
	}
	var total int64
	for _, l := range k.Layout.Layers {
		perToken := l.Latent * dtypeBytes
		if l.Latent == 0 {
			perToken = 2 * max(l.Heads/a.TensorParallel, 1) * l.HeadDim * dtypeBytes
		}
		blocks := ceilDiv(maxLen, a.BlockSize)
		if l.Window > 0 {
			blocks = ceilDiv(min(l.Window-1+batched, maxLen), a.BlockSize) + 1
		}
		total += l.Count * blocks * a.BlockSize * perToken
	}
	return total
}

// dtypeBytes is the size of one KV cache element: the --kv-cache-dtype, or
// for auto the checkpoint's KV quantization, else the model's dtype
// (--dtype, else config.json's). vLLM's --dtype=auto runs a float32
// checkpoint, and one whose config.json names no dtype (float32 to
// transformers), in 16 bits.
func (k *kvCheck) dtypeBytes() (int64, error) {
	cache := k.Args.KVCacheDType
	if cache == kvCacheDTypeAuto && k.Layout.CheckpointCache != "" {
		cache = k.Layout.CheckpointCache
	}
	switch cache {
	case "fp8", "fp8_e4m3", "fp8_e5m2", "fp8_inc":
		return 1, nil
	case "bfloat16", "float16":
		return 2, nil
	case kvCacheDTypeAuto:
	default:
		return 0, fmt.Errorf("%s=%s is a layout the check does not compute", flagKVCacheDType, cache)
	}
	dtype := k.Args.DType
	if dtype == kvCacheDTypeAuto {
		dtype = strings.ToLower(k.Layout.DType)
	}
	switch dtype {
	case "bfloat16", "float16", "half", "float32", "float", "":
		return 2, nil
	}
	return 0, fmt.Errorf("the model dtype %s is not one the check computes", dtype)
}

// clause words a verdict for the fit's reason: the KV need against what is
// left on the GPU, and on a refusal the estimated maximum model length.
func (v kvVerdict) clause() string {
	if v.Skip != "" {
		return "the KV cache is not checked: " + v.Skip
	}
	gpus := "the"
	if v.GPUs > 1 {
		gpus = fmt.Sprintf("each of the %d", v.GPUs)
	}
	left := fmt.Sprintf("%s left on %s %s GPU at %s=%s beside the weights and vLLM's %s GiB reserve",
		humanBytes(max(v.Available, 0)), gpus, humanBytes(v.GPUMemory), flagGPUMemoryUtilization, trimFloat2(v.Utilization), trimFloat2(vllmReserveGiB))
	if v.Fits {
		return fmt.Sprintf("the KV cache of one %d-token sequence (%s) fits the %s", v.MaxModelLen, humanBytes(v.Need), left)
	}
	out := fmt.Sprintf("the KV cache of one %d-token sequence needs %s, more than the %s", v.MaxModelLen, humanBytes(v.Need), left)
	if v.Estimated > 0 {
		return out + fmt.Sprintf(": the estimated maximum model length is %d tokens (lower %s or set %s=fp8)", v.Estimated, flagMaxModelLen, flagKVCacheDType)
	}
	return out + ": not one token fits"
}

func ceilDiv(a, b int64) int64 { return (a + b - 1) / b }

func derefOr(p *int64, def int64) int64 {
	if p == nil {
		return def
	}
	return *p
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// trimFloat2 prints a number to two decimals without trailing zeros (0.92, 0.9, 3.15).
func trimFloat2(f float64) string {
	return strconv.FormatFloat(math.Round(f*100)/100, 'f', -1, 64)
}

// kvCheckFor prepares the KV cache check of a sized plan: the preset's vLLM
// arguments and the layout of the checkpoint's config.json, read from the
// hub within the lookup budget. A plan the hub did not size (the preset's
// requirements stood in) has no config.json to read, and says so.
func (b *Backend) kvCheckFor(ctx context.Context, plan *fitPlan) *kvCheck {
	k := &kvCheck{Weights: plan.Result.WeightsBytes}
	p := plan.Preset
	var err error
	switch {
	case p == nil:
		k.Skip = "no serving preset names vLLM's " + flagMaxModelLen
		return k
	case p.cpu():
		k.Skip = "the preset requests no GPU"
		return k
	case plan.Hub == nil:
		k.Skip = "the Hugging Face Hub did not answer for the checkpoint's config.json"
		return k
	}
	if k.Args, err = parseVLLMArgs(p.Spec.Args); err != nil {
		k.Skip = err.Error()
		return k
	}
	hctx, hubBudget, cancel := b.hubContext(ctx)
	defer cancel()
	raw, err := b.hub.ModelConfig(hctx, plan.Repo, plan.Revision, plan.Files)
	switch {
	case err != nil:
		k.Skip = "reading config.json failed: " + describeHubFailure(err, hubBudget)
	case raw == nil:
		k.Skip = "the checkpoint has no config.json"
	default:
		if k.Layout, err = parseKVLayout(raw); err != nil {
			k.Skip = err.Error()
		}
	}
	return k
}

// applyKV writes a KV cache verdict into a fit answer: the numbers, and the
// clause of the reason — a refusal when the flat check passed but one
// sequence does not fit.
func applyKV(res *backend.FitResult, v kvVerdict) {
	res.MaxModelLen, res.KVCacheBytes, res.KVCacheAvailableBytes, res.EstimatedMaxModelLen = v.MaxModelLen, v.Need, max(v.Available, 0), v.Estimated
	if res.Fits && v.Skip == "" && !v.Fits {
		res.Fits = false
		res.Reason += ", but " + v.clause()
		return
	}
	res.Reason += "; " + v.clause()
}
