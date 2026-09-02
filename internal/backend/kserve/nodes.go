package kserve

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/giantswarm/model-manager/internal/backend"
)

// GPU feature-discovery labels (NVIDIA GPU operator / gpu-feature-discovery).
const (
	labelGPUCount   = "nvidia.com/gpu.count"
	labelGPUMemory  = "nvidia.com/gpu.memory" // MiB
	labelGPUProduct = "nvidia.com/gpu.product"
	labelHostname   = "kubernetes.io/hostname"
	mib             = int64(1 << 20)

	// BudgetAnnotation on a Node overrides its memory budget for fit checks,
	// in GiB (decimals allowed), whatever the configured budget source: the
	// most specific setting wins. Unified-memory nodes without GPU memory
	// labels (allocatable memory overstates what a model may use) and MIG or
	// shared-GPU setups need it. Zero, negative or unparsable values are
	// ignored and reported in the node's message.
	BudgetAnnotation = "model-manager.giantswarm.io/memory-budget-gib"
)

// nodeBudget is the serving-relevant view of one node.
type nodeBudget struct {
	Name         string
	Ready        bool
	Architecture string
	Labels       map[string]string
	Allocatable  int64
	GPUCount     int64
	GPUMemory    int64
	GPUProduct   string
	Budget       int64
	BudgetSource string
	// Message notes a budget derivation problem (an ignored annotation).
	Message string
}

// budgetOf derives the memory budget of a node: GPU memory (labels x count)
// when the node advertises GPUs and the source allows it, else the allocatable
// memory (unified-memory nodes, or nodes without feature-discovery labels).
// A valid BudgetAnnotation replaces the result of either source.
func budgetOf(n *corev1.Node, gpuResource, source string) nodeBudget {
	nb := nodeBudget{Name: n.Name, Labels: n.Labels, Architecture: n.Status.NodeInfo.Architecture}
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			nb.Ready = c.Status == corev1.ConditionTrue
		}
	}
	if q, ok := n.Status.Allocatable[corev1.ResourceMemory]; ok {
		nb.Allocatable = q.Value()
	}
	if q, ok := n.Status.Allocatable[corev1.ResourceName(gpuResource)]; ok {
		nb.GPUCount = q.Value()
	}
	if v, err := strconv.ParseInt(n.Labels[labelGPUCount], 10, 64); err == nil && v > 0 {
		nb.GPUCount = v
	}
	if v, err := strconv.ParseInt(n.Labels[labelGPUMemory], 10, 64); err == nil && v > 0 {
		nb.GPUMemory = v * mib
	}
	nb.GPUProduct = n.Labels[labelGPUProduct]

	gpuBudget := int64(0)
	if nb.GPUMemory > 0 {
		count := nb.GPUCount
		if count < 1 {
			count = 1
		}
		gpuBudget = nb.GPUMemory * count
	}
	switch source {
	case budgetSourceGPULabels:
		nb.Budget, nb.BudgetSource = gpuBudget, budgetSourceGPULabels
	case budgetSourceAllocatable:
		nb.Budget, nb.BudgetSource = nb.Allocatable, budgetSourceAllocatable
	default:
		if gpuBudget > 0 {
			nb.Budget, nb.BudgetSource = gpuBudget, budgetSourceGPULabels
		} else {
			nb.Budget, nb.BudgetSource = nb.Allocatable, budgetSourceAllocatable
		}
	}
	if raw, ok := n.Annotations[BudgetAnnotation]; ok {
		if v, err := strconv.ParseFloat(strings.TrimSpace(raw), 64); err != nil || v <= 0 || math.IsInf(v, 0) {
			nb.Message = fmt.Sprintf("ignoring annotation %s=%q: want a positive number of GiB; budget taken from %s", BudgetAnnotation, raw, nb.BudgetSource)
		} else {
			nb.Budget, nb.BudgetSource = gibToBytes(v), budgetSourceAnnotation
		}
	}
	return nb
}

// nodes lists every node's budget, sorted by name.
func (b *Backend) nodes(ctx context.Context) ([]nodeBudget, error) {
	list, err := b.cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	s := b.cfg.settings(ctx)
	out := make([]nodeBudget, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, budgetOf(&list.Items[i], s.GPUResourceName, b.opts.BudgetSource))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// matchesSelector reports whether the node carries every label of sel.
func matchesSelector(labels, sel map[string]string) bool {
	for k, v := range sel {
		if labels[k] != v {
			return false
		}
	}
	return true
}

// cacheLocation says where the download cache lives.
type cacheLocation struct {
	Claim string
	// Nodes are the nodes that can mount the claim; empty with Shared=false
	// means "not known yet" (unbound claim with late binding).
	Nodes []string
	// Shared is true for a claim without node affinity (network storage).
	Shared bool
	// Bound is false when the claim has no volume yet.
	Bound bool
	// Missing is true when the claim does not exist.
	Missing bool
}

// cacheNodes locates the cache: the explicit override, else the node affinity
// of the PersistentVolume bound to the claim (a static local PV or a
// local-path volume pins the cache to one node).
func (b *Backend) cacheNodes(ctx context.Context) (cacheLocation, error) {
	s := b.cfg.settings(ctx)
	loc := cacheLocation{Claim: s.CacheClaim}
	if len(b.opts.CacheNodes) > 0 {
		loc.Nodes = append(loc.Nodes, b.opts.CacheNodes...)
		loc.Bound = true
		return loc, nil
	}
	pvc, err := b.cs.CoreV1().PersistentVolumeClaims(s.Namespace).Get(ctx, s.CacheClaim, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		loc.Missing = true
		return loc, nil
	}
	if err != nil {
		return loc, fmt.Errorf("get cache claim %s/%s: %w", s.Namespace, s.CacheClaim, err)
	}
	if pvc.Spec.VolumeName == "" {
		return loc, nil
	}
	loc.Bound = true
	pv, err := b.cs.CoreV1().PersistentVolumes().Get(ctx, pvc.Spec.VolumeName, metav1.GetOptions{})
	if err != nil {
		if errors.IsForbidden(err) || errors.IsNotFound(err) {
			// Without PV access the cache is treated as shared; the scan runs
			// wherever the claim can be mounted.
			loc.Shared = true
			return loc, nil
		}
		return loc, fmt.Errorf("get volume %s: %w", pvc.Spec.VolumeName, err)
	}
	loc.Nodes = pvNodes(pv)
	loc.Shared = len(loc.Nodes) == 0
	return loc, nil
}

// pvNodes extracts the hostnames a volume's node affinity allows.
func pvNodes(pv *corev1.PersistentVolume) []string {
	if pv.Spec.NodeAffinity == nil || pv.Spec.NodeAffinity.Required == nil {
		return nil
	}
	var out []string
	for _, term := range pv.Spec.NodeAffinity.Required.NodeSelectorTerms {
		for _, expr := range term.MatchExpressions {
			if expr.Key == labelHostname && expr.Operator == corev1.NodeSelectorOpIn {
				out = append(out, expr.Values...)
			}
		}
	}
	sort.Strings(out)
	return out
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// nodeView converts a budget plus cache state to the API form.
func nodeView(nb nodeBudget, reserved int64, cache *backend.NodeCache) backend.NodeInfo {
	info := backend.NodeInfo{
		Name:                   nb.Name,
		Ready:                  nb.Ready,
		Architecture:           nb.Architecture,
		AllocatableMemoryBytes: nb.Allocatable,
		GPUCount:               nb.GPUCount,
		GPUMemoryBytes:         nb.GPUMemory,
		GPUProduct:             nb.GPUProduct,
		BudgetBytes:            nb.Budget,
		BudgetSource:           nb.BudgetSource,
		Message:                nb.Message,
		ReservedBytes:          reserved,
		FreeBytes:              nb.Budget - reserved,
		Cache:                  cache,
	}
	if info.FreeBytes < 0 {
		info.FreeBytes = 0
	}
	return info
}
