package kserve

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
)

// Requests in the serving shape (giantswarm/model-manager#223): a preset's
// CPU and memory requests are its reference node's too — 16 CPUs on a
// 48-vCPU g6e.12xlarge do not schedule on a 20-core GB10 beside the pods it
// already runs. Each pod requests the preset's, capped at what the node has
// left beside the requests of its other pods (the preset's own serving pods
// excluded: a reload replaces them); a node with nothing left refuses.

// podRoom is the CPU (millicores) and memory (bytes) a node has left beside
// the requests of its other pods; Known is false when its pods could not be
// read, and Why then says why.
type podRoom struct {
	CPU, Memory int64
	Known       bool
	Why         string
}

// roomOn reads what node n has left for a serving pod of preset p.
func (b *Backend) roomOn(ctx context.Context, n nodeBudget, p *servingPreset) podRoom {
	if p == nil {
		return podRoom{}
	}
	list, err := b.k8s(ctx).CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{FieldSelector: fields.OneTermEqualSelector("spec.nodeName", n.Name).String()})
	if err != nil {
		b.log.Warn("listing the pods of a node failed", "node", n.Name, "error", err)
		return podRoom{Why: "listing its pods failed: " + err.Error()}
	}
	ns := b.cfg.settings(ctx).Namespace
	room := podRoom{CPU: n.AllocatableCPU, Memory: n.Allocatable, Known: true}
	for i := range list.Items {
		pod := &list.Items[i]
		if pod.Spec.NodeName != n.Name || pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		if pod.Namespace == ns && pod.Labels[llmisvcPodLabel] == p.name() {
			continue
		}
		cpu, mem := podRequests(pod)
		room.CPU -= cpu
		room.Memory -= mem
	}
	return room
}

// podRequests is what a pod requests as the scheduler counts it: the sum of
// its containers, at least its largest init container, plus its overhead.
func podRequests(pod *corev1.Pod) (cpu, mem int64) {
	for _, c := range pod.Spec.Containers {
		cpu += c.Resources.Requests.Cpu().MilliValue()
		mem += c.Resources.Requests.Memory().Value()
	}
	for _, c := range pod.Spec.InitContainers {
		cpu = max(cpu, c.Resources.Requests.Cpu().MilliValue())
		mem = max(mem, c.Resources.Requests.Memory().Value())
	}
	cpu += pod.Spec.Overhead.Cpu().MilliValue()
	mem += pod.Spec.Overhead.Memory().Value()
	return cpu, mem
}

// presetRequests are the preset's CPU (millicores) and memory (bytes)
// requests; zero for one it does not name or names unreadably (the fit's
// pool check names the latter).
func presetRequests(p *servingPreset) (cpu, mem int64) {
	if v, err := requestOf(p, "cpu", func(q resource.Quantity) float64 { return float64(q.MilliValue()) }); err == nil {
		cpu = int64(v)
	}
	if v, err := requestOf(p, "memory", func(q resource.Quantity) float64 { return float64(q.Value()) }); err == nil {
		mem = int64(v)
	}
	return cpu, mem
}

// fitRequests sets the shape's requests from the preset's, capped at the
// room node n has left (to whole tenths of a CPU and whole MiB), and returns
// the refusal when the node has none left of one the preset requests.
func (sh *servingShape) fitRequests(p *servingPreset, n nodeBudget, room podRoom) string {
	sh.CPU, sh.Memory = presetRequests(p)
	if !room.Known {
		return ""
	}
	const tenthCPU = 100
	if sh.CPU > room.CPU {
		if room.CPU < tenthCPU {
			return fmt.Sprintf("each serving pod requests %s CPU and %s has %s left beside the requests of its other pods", milliCPU(sh.CPU), n.Name, milliCPU(max(room.CPU, 0)))
		}
		sh.CPU = room.CPU / tenthCPU * tenthCPU
	}
	if sh.Memory > room.Memory {
		if room.Memory < mib {
			return fmt.Sprintf("each serving pod requests %s of memory and %s has %s left beside the requests of its other pods", humanBytes(sh.Memory), n.Name, humanBytes(max(room.Memory, 0)))
		}
		sh.Memory = room.Memory / mib * mib
	}
	return ""
}

// requestsNote words requests capped below the preset's; "" when none is.
func requestsNote(p *servingPreset, sh servingShape, node string) string {
	cpu, mem := presetRequests(p)
	var out string
	if sh.CPU > 0 && sh.CPU < cpu {
		out = fmt.Sprintf("requests %s CPU there, capped from the preset's %s to what %s has left", milliCPU(sh.CPU), milliCPU(cpu), node)
	}
	if sh.Memory > 0 && sh.Memory < mem {
		if out != "" {
			out += "; "
		}
		out += fmt.Sprintf("requests %s of memory there, capped from the preset's %s to what %s has left", humanBytes(sh.Memory), humanBytes(mem), node)
	}
	return out
}

func milliCPU(m int64) string {
	return resource.NewMilliQuantity(m, resource.DecimalSI).String()
}
