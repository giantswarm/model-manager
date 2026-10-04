package kserve

import (
	"cmp"
	"fmt"
	"math"
	"strconv"

	"github.com/giantswarm/model-manager/internal/backend"
)

// Serving shape (giantswarm/model-manager#223): a preset's resources.gpus and
// --tensor-parallel-size describe its reference shape, the node it was
// written for (four 48 GB L40S cards). Placed on another accelerator it runs
// in the shape derived from the node it is placed on, so one preset serves
// every accelerator:
//
//   - on a unified-memory node (one GPU sharing the host's memory, a GB10,
//     whose time-sliced replicas are no extra memory) one device per pod;
//   - on a node whose GPUs have memory of their own the smallest power of
//     two whose cards hold the weights (a split's share) and the overhead;
//   - a split requests that per-node count on each pod and runs tensor
//     parallel over devices × nodes;
//   - on a unified-memory node --gpu-memory-utilization is sized down to the
//     highest fraction that leaves the host its headroom when the preset's
//     own would not (unifiedVerdict.Fit).
//
// A node that tells nothing about its GPUs (a pool at zero, several GPUs
// without memory labels) keeps the reference shape.

// servingShape is how a preset runs where it is placed: the GPU devices each
// serving pod requests, vLLM's tensor parallel degree, and the
// --gpu-memory-utilization it is composed with (0: the preset's own).
type servingShape struct {
	Devices        int64
	TensorParallel int64
	Utilization    float64
	// CPU (millicores) and Memory (bytes) are each pod's requests: the
	// preset's, capped at what the node has left (requests.go); 0 where
	// the preset requests none.
	CPU, Memory int64
}

// referenceShape is the preset's own shape.
func referenceShape(p *servingPreset) servingShape {
	return servingShape{Devices: p.gpus(), TensorParallel: presetTensorParallel(p)}
}

// presetTensorParallel is the preset's --tensor-parallel-size, vLLM's 1 when
// it sets none or an unreadable one.
func presetTensorParallel(p *servingPreset) int64 {
	if v, ok := vllmFlagValues(p.Spec.Args)[flagTensorParallelSize]; ok {
		if n, err := humanReadableInt(v); err == nil && n > 0 {
			return n
		}
	}
	return 1
}

// unifiedGPU reports a node with one GPU that has no memory of its own: the
// node's memory is the GPU's (a GB10).
func (n nodeBudget) unifiedGPU() bool { return n.GPUCount == 1 && n.GPUMemory == 0 }

// devicesOn is the GPU devices one serving pod of p requests on node n to
// hold share bytes (its weights, or a split's share of them, and the
// overhead).
func devicesOn(p *servingPreset, n nodeBudget, share int64) int64 {
	switch {
	case p.cpu():
		return 0
	case n.unifiedGPU():
		return 1
	case n.GPUMemory <= 0:
		return p.gpus() // nothing to derive from: the reference
	}
	d := int64(1)
	for d*n.GPUMemory < share {
		d *= 2
	}
	return d
}

// devicesClause refuses a device count node n cannot schedule: more devices
// per pod than it has allocatable; "" when it can, or when the node reports
// none (its device plugin not up).
func devicesClause(devices int64, n nodeBudget) string {
	if n.GPUAllocatable <= 0 || devices <= n.GPUAllocatable {
		return ""
	}
	return fmt.Sprintf("each serving pod needs %d GPU devices there, and %s has %d allocatable", devices, n.Name, n.GPUAllocatable)
}

// shapeArgs rewrites the preset's vLLM arguments to the shape: the tensor
// parallel degree and the sized utilization, each only where it differs
// from what the preset says, so a preset on its reference node is composed
// as written.
func shapeArgs(p *servingPreset, sh servingShape) []string {
	args := p.Spec.Args
	if sh.TensorParallel > 0 && sh.TensorParallel != presetTensorParallel(p) {
		args = append(withoutFlag(args, flagTensorParallelSize), flagTensorParallelSize, strconv.FormatInt(sh.TensorParallel, 10))
	}
	if sh.Utilization > 0 && sh.Utilization != p.utilization() {
		args = append(withoutFlag(args, flagGPUMemoryUtilization), flagGPUMemoryUtilization, trimFloat2(sh.Utilization))
	}
	return args
}

// shaped is the KV cache check in the shape: its tensor parallel degree and
// utilization.
func (k *kvCheck) shaped(sh servingShape) *kvCheck {
	if k == nil {
		return nil
	}
	c := *k
	if sh.TensorParallel > 0 {
		c.Args.TensorParallel = sh.TensorParallel
	}
	if sh.Utilization > 0 {
		c.Args.Utilization = sh.Utilization
	}
	return &c
}

// sized is the verdict with the utilization sized from the weights and
// overhead against the node's memory — the smallest fraction, in hundredths,
// that holds them — when the preset's own claim does not fit and that one
// does; the verdict as it is otherwise.
func (v unifiedVerdict) sized() unifiedVerdict {
	if !v.Checked || v.Fits || v.Memory <= 0 {
		return v
	}
	u := math.Ceil(float64(v.Need)/float64(v.Memory)*100) / 100
	claim := int64(u * float64(v.Memory))
	if u <= 0 || u > 1 || claim > v.Available {
		return v
	}
	v.Preset, v.Utilization, v.Claim, v.Fits = v.Utilization, u, claim, true
	return v
}

// shapeOn derives the shape of preset p on node n for a pod holding share
// bytes, one of nodes pods of a split (1: a single copy), beside reserved
// bytes of running models, and returns the unified-memory verdict at the
// sized utilization. A bare model reference, a CPU preset and a preset that
// runs pipeline parallel keep the reference shape.
func (b *Backend) shapeOn(p *servingPreset, n nodeBudget, reserved, share, nodes int64) (servingShape, unifiedVerdict) {
	uv := b.unifiedClaimOn(p, n, reserved, share).sized()
	if p == nil || p.cpu() || pipelineParallel(p) {
		if p == nil {
			return servingShape{}, uv
		}
		return referenceShape(p), uv
	}
	sh := referenceShape(p)
	sh.Utilization = p.utilization()
	if n.unifiedGPU() || n.GPUMemory > 0 {
		sh.Devices = devicesOn(p, n, share)
		sh.TensorParallel = sh.Devices * nodes
	}
	if uv.Checked {
		sh.Utilization = uv.Utilization
	}
	return sh, uv
}

// pipelineParallel reports a preset that splits its layers across GPUs: its
// devices are tensor × pipeline parallel, a shape the derivation leaves as
// written.
func pipelineParallel(p *servingPreset) bool {
	v, ok := vllmFlagValues(p.Spec.Args)[flagPipelineParallelSize]
	return ok && v != "1"
}

// applyShape writes the shape into a fit answer: the figures, a refusal when
// node n cannot schedule its devices, and a note when it is not the preset's
// reference shape.
func applyShape(res *backend.FitResult, p *servingPreset, sh servingShape, n nodeBudget, room podRoom) {
	if p == nil {
		return
	}
	refusal := sh.fitRequests(p, n, room)
	res.CPURequestMillis, res.MemoryRequestBytes = sh.CPU, sh.Memory
	if !p.cpu() {
		res.DevicesPerPod, res.TensorParallel, res.GPUMemoryUtilization = sh.Devices, sh.TensorParallel, sh.Utilization
		refusal = cmp.Or(refusal, devicesClause(sh.Devices, n))
	}
	if refusal != "" && res.Fits {
		res.Fits = false
		res.Reason += ", but " + refusal
		return
	}
	for _, note := range []string{shapeNote(p, sh), requestsNote(p, sh, n.Name)} {
		if note != "" {
			res.Reason += "; " + note
		}
	}
	if !room.Known && room.Why != "" {
		res.Reason += "; the CPU and memory " + n.Name + " has left are not checked: " + room.Why
	}
}

// shapeNote words a shape that differs from the preset's reference shape;
// "" when it is the reference.
func shapeNote(p *servingPreset, sh servingShape) string {
	ref := referenceShape(p)
	if sh.Devices == ref.Devices && sh.TensorParallel == ref.TensorParallel {
		return ""
	}
	return fmt.Sprintf("runs with %s per pod and tensor parallel %d there (the preset's reference shape: %s, tensor parallel %d)",
		plural(sh.Devices, "GPU device"), sh.TensorParallel, plural(ref.Devices, "GPU device"), ref.TensorParallel)
}

func plural(n int64, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.FormatInt(n, 10) + " " + noun + "s"
}

// shapeOf is the shape a fit answer judged for the preset: its figures, the
// preset's reference shape when it judged no node.
func shapeOf(p *servingPreset, res backend.FitResult) servingShape {
	sh := referenceShape(p)
	if res.DevicesPerPod > 0 {
		sh = servingShape{Devices: res.DevicesPerPod, TensorParallel: res.TensorParallel, Utilization: res.GPUMemoryUtilization}
	}
	sh.CPU, sh.Memory = res.CPURequestMillis, res.MemoryRequestBytes
	return sh
}
