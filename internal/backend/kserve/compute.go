package kserve

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/giantswarm/model-manager/internal/backend"
)

// The GPU generation a checkpoint's weights need, judged beside the memory
// and the KV cache: vLLM runs FP8 weights natively on a GPU of compute
// capability 8.9 and above (Ada, Hopper, Blackwell) and falls back to the
// weight-only FP8 Marlin kernel below it — a path that goes Ready and
// answers wrong: an FP8 W8A8 checkpoint of the Qwen3.5 lineage served on an
// A10G (Ampere, 8.6) answered gibberish at temperature 0 while the same
// preset answered on an L4 (giantswarm/model-manager#264). NVFP4 weights
// need a Blackwell GPU (10.0); bf16 weights a GPU of 8.0 (a T4 has
// neither). The fit refuses a GPU below what the weights need, so nothing
// composes a predictor whose answers are wrong.

// GPU feature-discovery labels naming the GPU's compute capability.
const (
	labelGPUComputeMajor = "nvidia.com/gpu.compute.major"
	labelGPUComputeMinor = "nvidia.com/gpu.compute.minor"

	computeCapabilityBF16 = "8.0"
	computeCapabilityFP8  = "8.9"
	computeCapabilityFP4  = "10.0"
)

// gpuGeneration is the GPU of an EC2 instance family and its compute
// capability.
type gpuGeneration struct {
	GPU, Architecture, Capability string
}

// awsGPUFamilies is the GPU of the EC2 families GPU pools come as — the
// curated accelerators (T4, A10G, L4, L40S) and the A100, H100, H200 and B200
// families — for a pool that has no node yet to read the labels from. The
// same geometry sizes the pool on the form (cluster-manager's accelerators).
var awsGPUFamilies = map[string]gpuGeneration{
	"g4dn":    {"T4", "Turing", "7.5"},
	"g5":      {"A10G", "Ampere", "8.6"},
	"g6":      {"L4", "Ada", "8.9"},
	"g6e":     {"L40S", "Ada", "8.9"},
	"p4d":     {"A100", "Ampere", "8.0"},
	"p4de":    {"A100", "Ampere", "8.0"},
	"p5":      {"H100", "Hopper", "9.0"},
	"p5e":     {"H200", "Hopper", "9.0"},
	"p5en":    {"H200", "Hopper", "9.0"},
	"p6-b200": {"B200", "Blackwell", "10.0"},
}

// computeNeed is the compute capability the checkpoint's weights need of each
// GPU, read from its config.json and the preset's --dtype: Capability (8.9)
// and Why (what needs it: "FP8 weights (compressed-tensors)"). Skip says why
// nothing is required or the precision could not be read; the GPU generation
// is then not judged.
type computeNeed struct {
	Capability string
	Why        string
	Skip       string
}

// hfQuantization is the part of a config.json's quantization_config the
// need is read from: the method, and for compressed-tensors the groups'
// weight types, for ModelOpt the algorithm (per layer in mixed precision).
type hfQuantization struct {
	QuantMethod  string `json:"quant_method"`
	QuantAlgo    string `json:"quant_algo"`
	ConfigGroups map[string]struct {
		Weights *struct {
			Type    string `json:"type"`
			NumBits int    `json:"num_bits"`
		} `json:"weights"`
	} `json:"config_groups"`
	QuantizedLayers map[string]struct {
		QuantAlgo string `json:"quant_algo"`
	} `json:"quantized_layers"`
}

// computeNeedFor reads what the planned model's weights need of the GPU
// (computeNeedOf) from the checkpoint's config.json (checkpointConfig); the
// preset's --dtype decides the dtype of unquantized weights.
func (b *Backend) computeNeedFor(ctx context.Context, plan *fitPlan) *computeNeed {
	if plan.Preset.cpu() {
		return &computeNeed{Skip: "the preset requests no GPU"}
	}
	raw, skip := b.checkpointConfig(ctx, plan)
	if skip != "" {
		return &computeNeed{Skip: skip}
	}
	need := computeNeedOf(raw, presetDType(plan.Preset))
	return &need
}

// checkpointConfig is the checkpoint's config.json for the checks that read
// it — the KV cache layout (kvCheckFor) and the GPU generation the weights
// need (computeNeedFor) — read once per plan: the model image's for a
// preset served from one (sizeModelImage), else the hub's within the lookup
// budget. skip says why there is none.
func (b *Backend) checkpointConfig(ctx context.Context, plan *fitPlan) ([]byte, string) {
	if plan.Config != nil || plan.ConfigSkip != "" {
		return plan.Config, plan.ConfigSkip
	}
	switch p := plan.Preset; {
	case p != nil && !p.storesInCache():
		if plan.Image == nil {
			plan.ConfigSkip = "the model image's registry did not answer for the checkpoint's config.json"
			return nil, plan.ConfigSkip
		}
		plan.Config = plan.Image.Config
	case plan.Hub == nil:
		plan.ConfigSkip = "the Hugging Face Hub did not answer for the checkpoint's config.json"
		return nil, plan.ConfigSkip
	default:
		hctx, hubBudget, cancel := b.hubContext(ctx)
		defer cancel()
		raw, err := b.hub.ModelConfig(hctx, plan.Repo, plan.Revision, plan.Files)
		if err != nil {
			plan.ConfigSkip = "reading config.json failed: " + describeHubFailure(err, hubBudget)
			return nil, plan.ConfigSkip
		}
		plan.Config = raw
	}
	if plan.Config == nil {
		plan.ConfigSkip = "the checkpoint has no config.json"
	}
	return plan.Config, plan.ConfigSkip
}

// presetDType is the preset's --dtype; empty when it names none.
func presetDType(p *servingPreset) string {
	if p == nil {
		return ""
	}
	for i, a := range p.Spec.Args {
		if v, ok := strings.CutPrefix(a, flagDType+"="); ok {
			return v
		}
		if a == flagDType && i+1 < len(p.Spec.Args) {
			return p.Spec.Args[i+1]
		}
	}
	return ""
}

// computeNeedOf reads what the checkpoint's weights need from its
// config.json; dtypeFlag is the preset's --dtype, which decides the running
// dtype of unquantized weights (auto or empty: the checkpoint's own).
func computeNeedOf(raw []byte, dtypeFlag string) computeNeed {
	if len(raw) == 0 {
		return computeNeed{Skip: "the checkpoint has no config.json"}
	}
	var cfg struct {
		DType              string          `json:"dtype"`
		TorchDType         string          `json:"torch_dtype"`
		QuantizationConfig json.RawMessage `json:"quantization_config"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return computeNeed{Skip: fmt.Sprintf("config.json does not parse: %v", err)}
	}
	if need, ok := quantizedNeed(cfg.QuantizationConfig); ok {
		return need
	}
	dtype := strings.ToLower(strings.TrimSpace(dtypeFlag))
	if dtype == "" || dtype == "auto" {
		dtype = strings.ToLower(firstNonEmpty(cfg.DType, cfg.TorchDType))
	}
	switch dtype {
	case "":
		return computeNeed{Skip: "config.json names no dtype"}
	case "bfloat16", "bf16":
		return computeNeed{Capability: computeCapabilityBF16, Why: "bf16 weights"}
	}
	return computeNeed{Skip: dtype + " weights need no particular GPU generation"}
}

// quantizedNeed is the need of quantized weights: FP8 (vLLM's fp8 method,
// compressed-tensors' 8-bit float, ModelOpt's FP8), NVFP4 (compressed-tensors'
// 4-bit float, ModelOpt's NVFP4). false for unquantized weights, or a
// scheme whose precision the dtype decides (int4, int8, MXFP4: vLLM
// dequantizes them on any GPU it supports).
func quantizedNeed(raw json.RawMessage) (computeNeed, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return computeNeed{}, false
	}
	var q hfQuantization
	if err := json.Unmarshal(raw, &q); err != nil {
		return computeNeed{Skip: fmt.Sprintf("quantization_config does not parse: %v", err)}, true
	}
	method := strings.ToLower(q.QuantMethod)
	switch {
	case method == "fp8":
		return computeNeed{Capability: computeCapabilityFP8, Why: "FP8 weights (quant_method fp8)"}, true
	case method == "compressed-tensors":
		for _, g := range q.ConfigGroups {
			if g.Weights == nil || strings.ToLower(g.Weights.Type) != "float" {
				continue
			}
			switch g.Weights.NumBits {
			case 8:
				return computeNeed{Capability: computeCapabilityFP8, Why: "FP8 weights (compressed-tensors)"}, true
			case 4:
				return computeNeed{Capability: computeCapabilityFP4, Why: "FP4 weights (compressed-tensors)"}, true
			}
		}
		return computeNeed{}, false
	case strings.HasPrefix(method, quantMethodModelOptPrefix):
		algos := []string{q.QuantAlgo}
		for _, l := range q.QuantizedLayers {
			algos = append(algos, l.QuantAlgo)
		}
		var fp8 bool
		for _, a := range algos {
			switch a = strings.ToUpper(a); {
			case strings.Contains(a, "FP4"):
				return computeNeed{Capability: computeCapabilityFP4, Why: "NVFP4 weights (ModelOpt)"}, true
			case a == "FP8":
				fp8 = true
			}
		}
		if fp8 {
			return computeNeed{Capability: computeCapabilityFP8, Why: "FP8 weights (ModelOpt)"}, true
		}
		if strings.EqualFold(q.QuantAlgo, "MIXED_PRECISION") {
			return computeNeed{Skip: "the ModelOpt checkpoint quantizes its layers in mixed precision and config.json does not say to what"}, true
		}
	}
	return computeNeed{}, false
}

// computeVerdict is the check of one GPU: what the weights need (Required,
// Why), what the GPU has (Capability) and how it is named (GPU: "the A10G of
// a g5.xlarge (Ampere)"); Skip says why it is not judged.
type computeVerdict struct {
	Skip       string
	Fits       bool
	Required   string
	Why        string
	Capability string
	GPU        string
}

// judgeNode runs the check against a node's GPU, from its compute capability
// labels.
func (n *computeNeed) judgeNode(nb nodeBudget) computeVerdict {
	if v, done := n.skipped(); done {
		return v
	}
	major, minor := strings.TrimSpace(nb.Labels[labelGPUComputeMajor]), strings.TrimSpace(nb.Labels[labelGPUComputeMinor])
	if major == "" || minor == "" {
		return n.unknown(fmt.Sprintf("the GPU generation of node %s is unknown (no %s/%s label)", nb.Name, labelGPUComputeMajor, labelGPUComputeMinor))
	}
	gpu := "the GPU of " + nb.Name
	if nb.GPUProduct != "" {
		gpu = "the " + nb.GPUProduct + " of " + nb.Name
	}
	return n.judge(major+"."+minor, gpu)
}

// judgeShape runs the check against the GPU of a pool size, from its
// instance family (awsGPUFamilies).
func (n *computeNeed) judgeShape(s backend.InstanceShape) computeVerdict {
	if v, done := n.skipped(); done {
		return v
	}
	family, _, _ := strings.Cut(s.InstanceType, ".")
	gen, ok := awsGPUFamilies[family]
	if !ok {
		return n.unknown(fmt.Sprintf("the GPU generation of a %s is unknown to the check", s.InstanceType))
	}
	return n.judge(gen.Capability, fmt.Sprintf("the %s of a %s (%s)", gen.GPU, s.InstanceType, gen.Architecture))
}

// unknown is the verdict of a GPU whose generation the check cannot tell:
// the need stands in the answer, judged against nothing.
func (n *computeNeed) unknown(why string) computeVerdict {
	return computeVerdict{Skip: why, Required: n.Capability, Why: n.Why}
}

// skipped is the verdict of a need that judges nothing: none read, or none
// required.
func (n *computeNeed) skipped() (computeVerdict, bool) {
	switch {
	case n == nil:
		return computeVerdict{Skip: "no checkpoint config to read the weights' precision from"}, true
	case n.Skip != "":
		return computeVerdict{Skip: n.Skip}, true
	}
	return computeVerdict{}, false
}

func (n *computeNeed) judge(capability, gpu string) computeVerdict {
	have, err := parseCapability(capability)
	if err != nil {
		return n.unknown(fmt.Sprintf("%s names compute capability %q, which does not parse", gpu, capability))
	}
	need, _ := parseCapability(n.Capability)
	return computeVerdict{Fits: have >= need, Required: n.Capability, Why: n.Why, Capability: capability, GPU: gpu}
}

// parseCapability turns "8.6" into a number that orders generations
// (8.6 < 8.9 < 10.0).
func parseCapability(s string) (float64, error) {
	major, minor, ok := strings.Cut(s, ".")
	if !ok {
		minor = "0"
	}
	a, err := strconv.Atoi(major)
	if err != nil {
		return 0, err
	}
	b, err := strconv.Atoi(minor)
	if err != nil {
		return 0, err
	}
	return float64(a) + float64(b)/100, nil
}

// clause words a verdict for the fit's reason; empty when the weights need
// nothing the check reads (nothing to say).
func (v computeVerdict) clause() string {
	if v.Skip != "" {
		if v.Required == "" {
			return ""
		}
		return fmt.Sprintf("its %s need compute capability %s; %s", v.Why, v.Required, v.Skip)
	}
	if v.Fits {
		return fmt.Sprintf("its %s need compute capability %s and %s has %s", v.Why, v.Required, v.GPU, v.Capability)
	}
	out := fmt.Sprintf("its %s need compute capability %s and %s has %s", v.Why, v.Required, v.GPU, v.Capability)
	switch v.Required {
	case computeCapabilityFP8:
		out += ": without native FP8 vLLM falls back to the weight-only FP8 Marlin kernel, which answers wrong for such a checkpoint; serve it on a GPU of 8.9 or above (Ada, Hopper, Blackwell) or serve a BF16 checkpoint here"
	case computeCapabilityFP4:
		out += ": NVFP4 runs on Blackwell GPUs; serve it there or serve an FP8 or BF16 checkpoint here"
	case computeCapabilityBF16:
		out += ": bf16 needs a GPU of 8.0 or above; set --dtype=float16 for this GPU or serve it on a newer one"
	}
	return out
}

// applyCompute writes a GPU generation verdict into a fit answer: the
// capabilities, and the clause of the reason — a refusal when the memory
// fit passed but the GPU is below what the weights need.
func applyCompute(res *backend.FitResult, v computeVerdict) {
	clause := v.clause()
	if clause == "" {
		return
	}
	res.ComputeCapabilityRequired, res.ComputeCapability = v.Required, v.Capability
	if res.Fits && v.Skip == "" && !v.Fits {
		res.Fits = false
		res.Reason += ", but " + clause
		return
	}
	res.Reason += "; " + clause
}
