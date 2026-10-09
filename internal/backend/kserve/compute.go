package kserve

import (
	"context"
	"encoding/json"
	"fmt"
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
// run natively on Blackwell (10.0) and through vLLM's weight-only FP4 Marlin
// kernel on Ada and Hopper — the path the L40S presets serve; vLLM's
// quantization matrix lists that kernel from Turing, but nothing below 8.9
// is proven and its FP8 sibling answers wrong there, so NVFP4 needs 8.9 as
// FP8 does. bf16 weights need a GPU of 8.0 (a T4 has neither). The fit
// refuses a GPU below what the weights need, so nothing composes a
// predictor whose answers are wrong.

// skipNoConfig is why a checkpoint without a config.json is not read from
// one: a mistral-format checkpoint's params.json stands in for it.
const skipNoConfig = "the checkpoint has no config.json"

// GPU feature-discovery labels naming the GPU's compute capability.
const (
	labelGPUComputeMajor = "nvidia.com/gpu.compute.major"
	labelGPUComputeMinor = "nvidia.com/gpu.compute.minor"
)

// weightPrecision is what of the weights the GPU generation is judged by,
// ordered by what the check says of it: a checkpoint mixing precisions
// needs its highest.
type weightPrecision int

const (
	precisionBF16 weightPrecision = iota + 1
	precisionFP8
	precisionFP4
)

// capability is the compute capability the precision needs.
func (p weightPrecision) capability() string {
	switch p {
	case precisionBF16:
		return "8.0"
	case precisionFP8, precisionFP4:
		return "8.9"
	}
	return ""
}

// remedy is what a refusal for the precision says after the capabilities.
func (p weightPrecision) remedy() string {
	switch p {
	case precisionFP8:
		return "without native FP8 vLLM falls back to the weight-only FP8 Marlin kernel, which answers wrong for such a checkpoint; serve it on a GPU of 8.9 or above (Ada, Hopper, Blackwell) or serve a BF16 checkpoint here"
	case precisionFP4:
		return "NVFP4 runs natively on Blackwell and through vLLM's weight-only FP4 Marlin kernel on Ada and Hopper, which is proven from 8.9 and not below it; serve it on a GPU of 8.9 or above or serve a BF16 checkpoint here"
	case precisionBF16:
		return "bf16 needs a GPU of 8.0 or above; set --dtype=float16 for this GPU or serve it on a newer one"
	}
	return ""
}

// need is the computeNeed of weights of the precision, why naming them.
func (p weightPrecision) need(why string) computeNeed {
	return computeNeed{Capability: p.capability(), Precision: p, Why: why}
}

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
// GPU, read from its config.json (a mistral-format checkpoint's params.json)
// and the preset's --dtype: Capability (8.9), Precision (what the refusal
// says) and Why (what needs it: "FP8 weights (compressed-tensors)").
// Declared is the preset's requirements.minComputeCapability, named where it
// differs. Skip says why nothing is required or the precision could not be
// read; the GPU generation is then not judged.
type computeNeed struct {
	Capability string
	Precision  weightPrecision
	Why        string
	Declared   string
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
// (computeNeedOf) from the checkpoint's config.json (checkpointConfig), or
// from the params.json of a mistral-format checkpoint, which has none
// (computeNeedOfParams); the preset's --dtype decides the dtype of
// unquantized weights.
func (b *Backend) computeNeedFor(ctx context.Context, plan *fitPlan) *computeNeed {
	if plan.Preset.cpu() {
		return &computeNeed{Skip: "the preset requests no GPU"}
	}
	var need computeNeed
	switch raw, skip := b.checkpointConfig(ctx, plan); {
	case raw != nil:
		need = computeNeedOf(raw, presetDType(plan.Preset))
	default:
		need = computeNeed{Skip: skip}
		if params := b.checkpointParams(ctx, plan); params != nil {
			need = computeNeedOfParams(params, presetDType(plan.Preset))
		}
	}
	if plan.Preset != nil {
		need.Declared = strings.TrimSpace(plan.Preset.Spec.Requirements.MinComputeCapability)
	}
	return &need
}

// checkpointParams is a mistral-format checkpoint's params.json, read where
// the checkpoint has no config.json: the model image's, else the hub's within
// the lookup budget. nil when there is none or it could not be read — the
// check then says why there is no config.json.
func (b *Backend) checkpointParams(ctx context.Context, plan *fitPlan) []byte {
	switch p := plan.Preset; {
	case plan.ConfigSkip != skipNoConfig:
		return nil
	case p != nil && !p.storesInCache():
		if plan.Image == nil {
			return nil
		}
		return plan.Image.Params
	case plan.Hub == nil:
		return nil
	}
	hctx, _, cancel := b.hubContext(ctx)
	defer cancel()
	raw, err := b.hub.ModelParams(hctx, plan.Repo, plan.Revision, plan.Files)
	if err != nil {
		return nil
	}
	return raw
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
		plan.ConfigSkip = skipNoConfig
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
		return computeNeed{Skip: skipNoConfig}
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
	return dtypeNeed(dtypeFlag, firstNonEmpty(cfg.DType, cfg.TorchDType), "config.json")
}

// computeNeedOfParams reads what a mistral-format checkpoint's weights need
// from its params.json: its quantization_config (compressed-tensors shaped,
// read as config.json's is) or its quantization's qformat_weight; dtypeFlag
// decides the dtype of unquantized weights, which params.json does not name.
func computeNeedOfParams(raw []byte, dtypeFlag string) computeNeed {
	var params struct {
		QuantizationConfig json.RawMessage `json:"quantization_config"`
		Quantization       *struct {
			QFormatWeight string `json:"qformat_weight"`
		} `json:"quantization"`
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		return computeNeed{Skip: fmt.Sprintf("params.json does not parse: %v", err)}
	}
	if need, ok := quantizedNeed(params.QuantizationConfig); ok {
		return need
	}
	if params.Quantization != nil {
		switch f := strings.ToLower(params.Quantization.QFormatWeight); {
		case strings.Contains(f, "fp4"):
			return precisionFP4.need("FP4 weights (params.json qformat_weight " + f + ")")
		case strings.HasPrefix(f, "fp8"):
			return precisionFP8.need("FP8 weights (params.json qformat_weight " + f + ")")
		}
	}
	return dtypeNeed(dtypeFlag, "", "params.json")
}

// dtypeNeed is the need of unquantized weights: the preset's --dtype, else
// (auto or empty) the checkpoint's own, which source names.
func dtypeNeed(dtypeFlag, checkpointDType, source string) computeNeed {
	dtype := strings.ToLower(strings.TrimSpace(dtypeFlag))
	if dtype == "" || dtype == "auto" {
		dtype = strings.ToLower(checkpointDType)
	}
	switch dtype {
	case "":
		return computeNeed{Skip: source + " names no dtype"}
	case "bfloat16", "bf16":
		return precisionBF16.need("bf16 weights")
	}
	return computeNeed{Skip: dtype + " weights need no particular GPU generation"}
}

// quantizedNeed is the need of quantized weights: FP8 (vLLM's fp8 method,
// compressed-tensors' 8-bit float, ModelOpt's FP8), NVFP4 (compressed-tensors'
// 4-bit float, ModelOpt's NVFP4); a checkpoint mixing them needs the highest.
// ModelOpt's config_groups (mixed precision since ModelOpt 0.43) are read as
// compressed-tensors' are, its quant_algo and quantized_layers where it has
// none. false for unquantized weights, or a scheme whose precision the dtype
// decides (int4, int8, MXFP4: vLLM dequantizes them on any GPU it supports).
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
		return precisionFP8.need("FP8 weights (quant_method fp8)"), true
	case method == "compressed-tensors":
		switch q.floatGroups() {
		case precisionFP8:
			return precisionFP8.need("FP8 weights (compressed-tensors)"), true
		case precisionFP4:
			return precisionFP4.need("FP4 weights (compressed-tensors)"), true
		}
	case strings.HasPrefix(method, quantMethodModelOptPrefix):
		p := q.floatGroups()
		if p == 0 {
			p = q.modelOptAlgos()
		}
		switch p {
		case precisionFP8:
			return precisionFP8.need("FP8 weights (ModelOpt)"), true
		case precisionFP4:
			return precisionFP4.need("NVFP4 weights (ModelOpt)"), true
		}
		if strings.EqualFold(q.QuantAlgo, "MIXED_PRECISION") {
			return computeNeed{Skip: "the ModelOpt checkpoint quantizes its layers in mixed precision and config.json does not say to what"}, true
		}
	}
	return computeNeed{}, false
}

// floatGroups is the highest float precision of the config_groups' weights
// (8-bit: FP8, 4-bit: FP4); 0 when no group quantizes its weights to a float.
func (q hfQuantization) floatGroups() weightPrecision {
	var p weightPrecision
	for _, g := range q.ConfigGroups {
		if g.Weights == nil || strings.ToLower(g.Weights.Type) != "float" {
			continue
		}
		switch g.Weights.NumBits {
		case 8:
			p = max(p, precisionFP8)
		case 4:
			p = max(p, precisionFP4)
		}
	}
	return p
}

// modelOptAlgos is the highest precision of ModelOpt's quant_algo and the
// quantized_layers' algorithms; 0 when none names FP8 or FP4.
func (q hfQuantization) modelOptAlgos() weightPrecision {
	var p weightPrecision
	algos := []string{q.QuantAlgo}
	for _, l := range q.QuantizedLayers {
		algos = append(algos, l.QuantAlgo)
	}
	for _, a := range algos {
		switch a = strings.ToUpper(a); {
		case strings.Contains(a, "FP4"):
			p = max(p, precisionFP4)
		case a == "FP8":
			p = max(p, precisionFP8)
		}
	}
	return p
}

// computeVerdict is the check of one GPU: what the weights need (Required,
// Precision, Why; Declared the preset's own floor), what the GPU has
// (Capability) and how it is named (GPU: "the A10G of a g5.xlarge
// (Ampere)"); Skip says why it is not judged.
type computeVerdict struct {
	Skip       string
	Fits       bool
	Required   string
	Precision  weightPrecision
	Why        string
	Declared   string
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

// judgeShape runs the check against the GPU of a pool size: the compute
// capability the shape declares (what cluster-manager knows of the
// accelerator it sized the pool with), else its instance family's
// (awsGPUFamilies). The family names the GPU in the verdict as long as it
// agrees with the declared value.
func (n *computeNeed) judgeShape(s backend.InstanceShape) computeVerdict {
	if v, done := n.skipped(); done {
		return v
	}
	family, _, _ := strings.Cut(s.InstanceType, ".")
	gen, known := awsGPUFamilies[family]
	named := fmt.Sprintf("the %s of a %s (%s)", gen.GPU, s.InstanceType, gen.Architecture)
	switch {
	case s.ComputeCapability == "" && !known:
		return n.unknown(fmt.Sprintf("the GPU generation of a %s is unknown to the check", s.InstanceType))
	case s.ComputeCapability == "":
		return n.judge(gen.Capability, named)
	case known && gen.Capability == s.ComputeCapability:
		return n.judge(s.ComputeCapability, named)
	}
	return n.judge(s.ComputeCapability, "the GPU of a "+s.InstanceType)
}

// unknown is the verdict of a GPU whose generation the check cannot tell:
// the need stands in the answer, judged against nothing.
func (n *computeNeed) unknown(why string) computeVerdict {
	return computeVerdict{Skip: why, Required: n.Capability, Precision: n.Precision, Why: n.Why, Declared: n.Declared}
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
	return computeVerdict{Fits: have >= need, Required: n.Capability, Precision: n.Precision, Why: n.Why, Declared: n.Declared, Capability: capability, GPU: gpu}
}

// parseCapability turns "8.6" into a number that orders generations
// (8.6 < 8.9 < 10.0).
func parseCapability(s string) (float64, error) {
	major, minor, err := backend.ParseComputeCapability(s)
	if err != nil {
		return 0, err
	}
	return float64(major) + float64(minor)/100, nil
}

// clause words a verdict for the fit's reason; empty when the weights need
// nothing the check reads (nothing to say).
func (v computeVerdict) clause() string {
	var out string
	switch {
	case v.Skip != "" && v.Required == "":
		return ""
	case v.Skip != "":
		out = fmt.Sprintf("its %s need compute capability %s; %s", v.Why, v.Required, v.Skip)
	case v.Fits:
		out = fmt.Sprintf("its %s need compute capability %s and %s has %s", v.Why, v.Required, v.GPU, v.Capability)
	default:
		out = fmt.Sprintf("its %s need compute capability %s and %s has %s", v.Why, v.Required, v.GPU, v.Capability)
		if r := v.Precision.remedy(); r != "" {
			out += ": " + r
		}
	}
	return out + v.declaredDiffers()
}

// declaredDiffers names the preset's requirements.minComputeCapability where
// it differs from what the checkpoint needs, as a weights discrepancy is
// named: the checkpoint is what the check judges by.
func (v computeVerdict) declaredDiffers() string {
	if v.Declared == "" {
		return ""
	}
	declared, err := parseCapability(v.Declared)
	if err != nil {
		return fmt.Sprintf(" (the preset declares requirements.minComputeCapability %q, which does not parse)", v.Declared)
	}
	if need, _ := parseCapability(v.Required); need == declared {
		return ""
	}
	return fmt.Sprintf(" (the preset declares requirements.minComputeCapability %s; the checkpoint needs %s)", v.Declared, v.Required)
}

// applyCompute writes a GPU generation verdict into a fit answer: the
// capabilities, and the clause of the reason — a refusal when the memory
// fit passed but the GPU is below what the weights need. A generation not
// judged is named in skippedChecks, also where the reason has nothing to say.
func applyCompute(res *backend.FitResult, v computeVerdict) {
	if v.Skip != "" {
		res.Skip(backend.CheckComputeCapability, v.Skip)
	}
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
