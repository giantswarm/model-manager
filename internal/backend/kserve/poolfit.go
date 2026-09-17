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
// none, in its own log, when no size does: on gazelle a predictor requesting
// 4 vCPU / 16 GiB sat Pending for ten minutes on a pool of g6.xlarge nodes
// (giantswarm/agent-platform#502). With the pool's shapes in the backend
// document (backend.GPUPool.Instances, written by cluster-manager) the fit
// check says so first, and load_model refuses before a predictor exists.

// predictorNeeds is what the predictor composed for a model asks of the
// node: the preset's CPU and memory requests (zero without a preset or a
// request), its GPUs, and the GPU memory its weights and overhead need.
type predictorNeeds struct {
	VCPU         float64
	MemoryGiB    float64
	GPUs         int
	GPUMemoryGiB float64
}

// needsOf reads the predictor's needs from the plan: the preset's requests
// when a preset serves the model, the sized weights and overhead in any
// case. An unparsable request is the preset's error, not the pool's.
func needsOf(plan *fitPlan) (predictorNeeds, error) {
	n := predictorNeeds{GPUs: 1, GPUMemoryGiB: float64(plan.Result.RequiredBytes) / float64(gib)}
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

// hosts reports whether a node of shape s can run a predictor with needs n.
func hosts(s backend.InstanceShape, n predictorNeeds) bool {
	return n.VCPU <= s.UsableVCPU && n.MemoryGiB <= s.UsableMemoryGiB &&
		n.GPUs <= s.GPUs && n.GPUMemoryGiB <= gpuBudgetGiB(s, n)
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
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].VCPU != out[j].VCPU {
			return out[i].VCPU < out[j].VCPU
		}
		return out[i].MemoryGiB < out[j].MemoryGiB
	})
	return out
}

// placeOnPool writes the verdict for a pool with no node into the plan.
// Without instance shapes the answer is yes and says the fit is unverified;
// with them, the smallest size hosting the predictor is the node it will
// come as, and when none does the answer is no, naming what the predictor
// asks and what the pool's largest size leaves it. The GPU memory budget on
// either verdict is that of the GPUs the predictor requests on the size; a
// preset whose declared weights the Hub contradicts hears so on both.
func (b *Backend) placeOnPool(plan *fitPlan, pool backend.GPUPool) error {
	res := &plan.Result
	res.BudgetSource = budgetSourcePoolScaleFromZero
	where := fmt.Sprintf("no node in the GPU pool yet (%s): the pool scales from zero", formatSelector(pool.NodeSelector))
	need := fmt.Sprintf("%s weights + %s overhead = %s", humanBytes(res.WeightsBytes), humanBytes(res.OverheadBytes), humanBytes(res.RequiredBytes))
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
		return nil
	}
	largest := shapes[len(shapes)-1]
	res.Fits = false
	res.BudgetBytes = gibToBytes(gpuBudgetGiB(largest, needs))
	res.Reason = fmt.Sprintf("%s — no size of the pool (%s) hosts %s: %s; %s, the largest, leaves a predictor %s vCPU / %s GiB after the node's kubelet reservations and daemonsets and carries %d × %d GiB GPU, %s on the %d GPU the predictor requests (%s needed); widen the pool's sizes or pick a smaller preset%s",
		where, sizeNames(shapes), presetOrModel(plan), describeNeeds(needs, plan.Preset),
		largest.SizeName(), trimFloat(largest.UsableVCPU), trimFloat(largest.UsableMemoryGiB), largest.GPUs, largest.GPUMemoryGiB,
		humanBytes(res.BudgetBytes), requestedGPUs(needs), need, declarationNote(plan))
	return nil
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
