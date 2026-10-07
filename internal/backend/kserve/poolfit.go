package kserve

import (
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/giantswarm/model-manager/internal/backend"
)

// The fit of a model against a GPU pool that has no node yet. Karpenter
// launches the smallest size of the pool the pending predictor fits — or
// none, in its own log, when no size does: a predictor requesting
// 4 vCPU / 16 GiB sat Pending for ten minutes on a pool of g6.xlarge nodes
// (giantswarm/agent-platform#502). With the pool's shapes in the backend
// document (backend.GPUPool.Instances, written by cluster-manager) the fit
// check says so first, and load_model refuses before a predictor exists.

// predictorNeeds is what the predictor composed for a model asks of the
// node: the preset's CPU and memory requests (zero without a preset or a
// request), its GPUs, the GPU memory its weights and overhead need, and the
// KV cache of one sequence (KV, judged per GPU).
type predictorNeeds struct {
	VCPU         float64
	MemoryGiB    float64
	GPUs         int
	GPUMemoryGiB float64
	KV           *kvCheck
}

// needsOf reads the predictor's needs from the plan: the preset's requests
// when a preset serves the model, the sized weights and overhead in any
// case. An unparsable request is the preset's error, not the pool's.
func needsOf(plan *fitPlan) (predictorNeeds, error) {
	n := predictorNeeds{GPUs: 1, GPUMemoryGiB: float64(plan.Result.RequiredBytes) / float64(gib), KV: plan.KV}
	p := plan.Preset
	if p == nil {
		return n, nil
	}
	n.GPUs = int(p.gpus())
	var err error
	if n.VCPU, err = requestOf(p, "cpu", func(q resource.Quantity) float64 { return float64(q.MilliValue()) / 1000 }); err != nil {
		return n, err
	}
	if n.MemoryGiB, err = requestOf(p, "memory", func(q resource.Quantity) float64 { return float64(q.Value()) / float64(gib) }); err != nil {
		return n, err
	}
	return n, nil
}

// requestOf reads one of the preset's resources.requests as a number; zero
// when the preset does not name it.
func requestOf(p *servingPreset, name string, as func(resource.Quantity) float64) (float64, error) {
	v, ok := p.Spec.Resources.Requests[name]
	if !ok || v == nil {
		return 0, nil
	}
	q, err := resource.ParseQuantity(fmt.Sprint(v))
	if err != nil {
		return 0, fmt.Errorf("%w: preset %s: resources.requests.%s %q: %v", backend.ErrInvalid, p.name(), name, fmt.Sprint(v), err)
	}
	return as(q), nil
}

// hosts reports whether a node of shape s can run a predictor with needs n:
// its requests, its GPU memory, and one sequence of its KV cache on a GPU of
// the size (unless the KV cache is not checked).
func hosts(s backend.InstanceShape, n predictorNeeds) bool {
	kv := n.KV.judge(shapeGPUMemory(s), "")
	return n.VCPU <= s.UsableVCPU && n.MemoryGiB <= s.UsableMemoryGiB &&
		n.GPUs <= s.GPUs && n.GPUMemoryGiB <= gpuBudgetGiB(s, n) && (kv.Skip != "" || kv.Fits)
}

// shapeGPUMemory is the memory of one GPU of a size in bytes: gpuMemoryGiB
// is the nominal size a card is sold as (48 for an L40S), in decimal GB.
func shapeGPUMemory(s backend.InstanceShape) int64 {
	return int64(s.GPUMemoryGiB) * 1e9
}

// gpuBudgetGiB is the GPU memory a predictor with needs n has on a node of
// shape s: the memory of the GPUs it requests, not the node's — a one-GPU
// predictor on a four-GPU size gets one card, whatever the other three add
// up to (giantswarm/model-manager#113). The same arithmetic sized the pool
// on the form (cluster-manager's compose.hosts).
func gpuBudgetGiB(s backend.InstanceShape, n predictorNeeds) float64 {
	return float64(s.GPUMemoryGiB * requestedGPUs(n))
}

// requestedGPUs is the GPUs the predictor is scheduled with: at least one.
func requestedGPUs(n predictorNeeds) int { return max(n.GPUs, 1) }

// sortedShapes is the pool's shapes smallest first: by vCPU, then memory.
func sortedShapes(shapes []backend.InstanceShape) []backend.InstanceShape {
	out := make([]backend.InstanceShape, len(shapes))
	copy(out, shapes)
	sort.SliceStable(out, func(i, j int) bool { return smallerShape(out[i], out[j]) })
	return out
}

// placeOnPool writes the pool's verdict into the plan: the node the model
// goes to is one the pool launches, since it has none yet, its nodes all
// still start, or none of them takes the predictor (nodes, the pool's; see
// takes) — the reason names which. Nothing of a node the model does not go
// to stands in the verdict: the predictor is composed with the preset's own
// shape. Without instance shapes the answer is yes and says the fit is
// unverified; with them, the smallest size hosting the predictor is the node
// it will come as, and when none does the answer is no, naming what the
// predictor asks and what the pool's largest size leaves it. The GPU memory
// budget on either verdict is that of the GPUs the predictor requests on the
// size; a preset whose declared weights the Hub contradicts hears so on both.
func (b *Backend) placeOnPool(plan *fitPlan, pool backend.GPUPool, nodes []nodeBudget) error {
	res := &plan.Result
	res.Node, res.InstanceType, res.ReservedBytes, res.FreeBytes = "", "", 0, 0
	res.UnifiedReservationBytes, res.HostHeadroomBytes, res.FitGPUMemoryUtilization = 0, 0, 0
	res.CPURequestMillis, res.MemoryRequestBytes, res.DevicesPerPod, res.TensorParallel, res.GPUMemoryUtilization = 0, 0, 0, 0, 0
	res.BudgetSource = budgetSourcePoolScaleFromZero
	where := poolWhere(pool, nodes, plannedGPUs(plan))
	need := weightsNeed(res)
	if len(pool.Instances) == 0 {
		res.Fits = true
		res.Reason = fmt.Sprintf("%s — the predictor waits for its node; the fit of %s against the pool's accelerator is unverified", where, need)
		return nil
	}
	needs, err := needsOf(plan)
	if err != nil {
		return err
	}
	shapes := sortedShapes(pool.Instances)
	for _, s := range shapes {
		if !hosts(s, needs) {
			continue
		}
		res.Fits = true
		res.InstanceType = s.InstanceType
		res.BudgetBytes = gibToBytes(gpuBudgetGiB(s, needs))
		res.FreeBytes = res.BudgetBytes
		res.Reason = fmt.Sprintf("%s — the node comes as %s (%s: %s vCPU / %s GiB for the predictor, %d × %d GiB GPU); %s fit within %s on the %d GPU the predictor requests, and %s hosts %s%s",
			where, s.SizeName(), s.InstanceType, trimFloat(s.UsableVCPU), trimFloat(s.UsableMemoryGiB), s.GPUs, s.GPUMemoryGiB,
			need, humanBytes(res.BudgetBytes), requestedGPUs(needs), s.SizeName(), describeNeeds(needs, plan.Preset), declarationNote(plan))
		applyKV(res, plan.KV.judge(shapeGPUMemory(s), ""))
		return nil
	}
	largest := shapes[len(shapes)-1]
	res.Fits = false
	res.BudgetBytes = gibToBytes(gpuBudgetGiB(largest, needs))
	res.Reason = fmt.Sprintf("%s — no size of the pool (%s) hosts %s: %s; %s, the largest, leaves a predictor %s vCPU / %s GiB after the node's kubelet reservations and daemonsets and carries %d × %d GiB GPU, %s on the %d GPU the predictor requests (%s needed); widen the pool's sizes or pick a smaller preset%s",
		where, sizeNames(shapes), presetOrModel(plan), describeNeeds(needs, plan.Preset),
		largest.SizeName(), trimFloat(largest.UsableVCPU), trimFloat(largest.UsableMemoryGiB), largest.GPUs, largest.GPUMemoryGiB,
		humanBytes(res.BudgetBytes), requestedGPUs(needs), need, declarationNote(plan))
	applyKV(res, plan.KV.judge(shapeGPUMemory(largest), ""))
	return nil
}

// poolWhere words why the pool launches the predictor's node: it has none
// yet, its nodes all still start, or none of them takes a predictor
// requesting gpus GPUs.
func poolWhere(pool backend.GPUPool, nodes []nodeBudget, gpus int) string {
	selector := formatSelector(pool.NodeSelector)
	switch {
	case len(nodes) == 0:
		return fmt.Sprintf("no node in the GPU pool yet (%s): the pool scales from zero", selector)
	case !allStarting(nodes):
		return fmt.Sprintf("no node of the GPU pool (%s) takes the predictor (%s): the pool launches one", selector, describeHeld(nodes, gpus))
	case len(pool.NodeSelector) > 0:
		return fmt.Sprintf("no ready node in the GPU pool yet (%s): %s", selector, describeStarting(nodes))
	}
	return "no ready node yet: " + describeStarting(nodes)
}

func allStarting(nodes []nodeBudget) bool {
	for _, n := range nodes {
		if !n.Starting {
			return false
		}
	}
	return true
}

// describeHeld names what keeps each of a pool's nodes from taking a
// predictor requesting gpus GPUs: "node a has 1 GPU, the predictor requests
// 4; node b: not ready".
func describeHeld(nodes []nodeBudget, gpus int) string {
	parts := make([]string, 0, len(nodes))
	for _, n := range nodes {
		switch {
		case n.Starting:
			parts = append(parts, describeStarting([]nodeBudget{n}))
		case !n.Eligible:
			parts = append(parts, fmt.Sprintf("node %s: %s", n.Name, n.EligibilityReason))
		default:
			parts = append(parts, fmt.Sprintf("node %s has %s, the predictor requests %d", n.Name, describeDevices(n), gpus))
		}
	}
	return strings.Join(parts, "; ")
}

// takes reports whether node n schedules a predictor requesting gpus GPUs:
// a serving target with that many devices free (freeGPUs). A node whose
// device count is unknown is taken at its word. A pool none of whose nodes
// takes the predictor leaves it pending, and the autoscaler launches one of
// the pool's sizes for it.
func takes(n nodeBudget, gpus int) bool {
	return n.Eligible && (gpuDevices(n) == 0 || freeGPUs(n) >= int64(gpus))
}

// discreteGPUs says whether the node's GPUs have memory of their own (the
// GPU memory label): only there does a running predictor hold its devices
// apart from the memory judgement. A unified-memory node keeps the memory
// judgement alone.
func discreteGPUs(n nodeBudget) bool {
	return n.GPUMemory > 0 && gpuDevices(n) > 0
}

// freeGPUs is the node's devices the running predictors leave (GPUsTaken,
// recorded for a serve); on a node without discrete GPUs every device.
func freeGPUs(n nodeBudget) int64 {
	if !discreteGPUs(n) {
		return gpuDevices(n)
	}
	return max(gpuDevices(n)-n.GPUsTaken, 0)
}

// describeDevices is a node's devices as a refusal names them: "1 GPU", or
// "0 of 1 GPU free" when running predictors hold some.
func describeDevices(n nodeBudget) string {
	if free := freeGPUs(n); free < gpuDevices(n) {
		return fmt.Sprintf("%d of %s free", free, plural(gpuDevices(n), "GPU"))
	}
	return plural(gpuDevices(n), "GPU")
}

// gpuDevices is the node's accelerator devices: allocatable, else the
// feature-discovery count; 0 when it reports neither.
func gpuDevices(n nodeBudget) int64 {
	if n.GPUAllocatable > 0 {
		return n.GPUAllocatable
	}
	return n.GPUCount
}

// inPool are the nodes that carry every label of the pool's selector.
func inPool(nodes []nodeBudget, selector map[string]string) []nodeBudget {
	var out []nodeBudget
	for _, n := range nodes {
		if matchesSelector(n.Labels, selector) {
			out = append(out, n)
		}
	}
	return out
}

// plannedGPUs is the GPUs the predictor is scheduled with: the preset's, at
// least one.
func plannedGPUs(plan *fitPlan) int {
	if plan.Preset == nil {
		return 1
	}
	return max(int(plan.Preset.gpus()), 1)
}

// describeStarting names the starting nodes and what each still waits on:
// "node pool1 is starting (not ready, ebs.csi.aws.com/agent-not-ready:NoExecute)".
func describeStarting(nodes []nodeBudget) string {
	parts := make([]string, 0, len(nodes))
	for _, n := range nodes {
		var waits []string
		if !n.Ready {
			waits = append(waits, "not ready")
		}
		for _, t := range n.Taints {
			if isStartupTaint(t) {
				waits = append(waits, formatTaint(t))
			}
		}
		parts = append(parts, fmt.Sprintf("node %s is starting (%s)", n.Name, strings.Join(waits, ", ")))
	}
	return strings.Join(parts, "; ")
}

// declarationNote is the clause the verdict carries when the weights the Hub
// holds exceed what the preset declares (requirements.weightsGiB): the pool
// was sized on the form from the declaration, so a person who chose the
// preset there needs to hear that the Hub's size is what was judged. Empty
// without a preset, when the preset sized the weights itself, and when the
// declaration covers the Hub.
func declarationNote(plan *fitPlan) string {
	res := &plan.Result
	if plan.Preset == nil || res.WeightsBytes <= res.DeclaredWeightsBytes {
		return ""
	}
	declared, hub := humanBytes(res.DeclaredWeightsBytes), humanBytes(res.WeightsBytes)
	if res.Fits {
		return fmt.Sprintf("; the preset declares %s of weights, the Hub holds %s", declared, hub)
	}
	return fmt.Sprintf("; the preset declares %s of weights; the Hub holds %s, which is what does not fit — correct the preset", declared, hub)
}

// describeNeeds words the predictor's needs: the requests when a preset
// serves the model, the GPU memory alone otherwise.
func describeNeeds(n predictorNeeds, p *servingPreset) string {
	if p == nil {
		return fmt.Sprintf("%s GiB of GPU memory on %d GPU (no preset: weights and default overhead only)", trimFloat(n.GPUMemoryGiB), n.GPUs)
	}
	return fmt.Sprintf("%s vCPU / %s GiB requested, %d GPU, %s GiB of GPU memory", trimFloat(n.VCPU), trimFloat(n.MemoryGiB), n.GPUs, trimFloat(n.GPUMemoryGiB))
}

func presetOrModel(plan *fitPlan) string {
	if plan.Preset != nil {
		return "preset " + plan.Preset.name()
	}
	return plan.Repo
}

func sizeNames(shapes []backend.InstanceShape) string {
	names := make([]string, 0, len(shapes))
	for _, s := range shapes {
		names = append(names, s.SizeName())
	}
	return strings.Join(names, ", ")
}

// trimFloat prints a number to one decimal, without a trailing .0.
func trimFloat(f float64) string {
	s := fmt.Sprintf("%.1f", f)
	return strings.TrimSuffix(s, ".0")
}
