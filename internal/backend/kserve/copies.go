package kserve

import (
	"context"
	"fmt"
	"slices"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/model-manager/internal/backend"
)

// Copies (giantswarm/model-manager#194): a model served as copies on several
// nodes is one LLMInferenceService with one replica per node, behind the one
// endpoint its workload Service fronts. Each replica holds the whole model, so
// each node is judged on its own as for a single copy; the replicas are pinned
// to their nodes by affinity, one per node. Serving an already-served model on
// more nodes adds copies there.

// The component labels of an LLMInferenceService's workload pods: a
// single-node object's, and a split's workers.
const (
	workloadComponent = "llminferenceservice-workload"
	workerComponent   = "llminferenceservice-workload-worker"
)

// onNodes are the nodes a served object runs on: those recorded on it (a
// split's, the copies'), else its pod's node; none while it waits for one.
func (sv served) onNodes() []string {
	if len(sv.Nodes) > 0 {
		return sv.Nodes
	}
	if sv.Node != "" {
		return []string{sv.Node}
	}
	return nil
}

// copiesCheck is fitCheck for placement copies: one copy on each node of
// req.Nodes when it names any, else one on req.Node or the node the
// placement picks. With verdicts, a check that names no node answers the
// verdict of one copy on each of the cluster's nodes too (FitResult.Copies).
func (b *Backend) copiesCheck(ctx context.Context, req backend.FitRequest, forServe, verdicts bool) (*fitPlan, error) {
	plan, idx, err := b.resolveFit(ctx, req)
	if err != nil {
		return nil, err
	}
	note, err := b.sizeModel(ctx, plan)
	if err != nil {
		return nil, err
	}
	plan.KV = b.kvCheckFor(ctx, plan)
	plan.Compute = b.computeNeedFor(ctx, plan)
	sized := *plan
	if len(req.Nodes) > 0 {
		err = b.judgeCopies(ctx, plan, idx, req.Nodes, forServe)
	} else if err = b.placeModel(ctx, plan, idx, req, forServe); err == nil && verdicts && req.Node == "" && len(plan.Nodes) > 1 {
		names := make([]string, 0, len(plan.Nodes))
		for _, n := range plan.Nodes {
			names = append(names, n.Name)
		}
		var each []fitPlan
		if each, err = b.judgeEach(ctx, &sized, idx, names, forServe); err == nil {
			plan.Result.Copies = nodeFits(names, each)
		}
	}
	if err != nil {
		return nil, err
	}
	if note = joinNotes(note, placementNote(plan)); note != "" {
		plan.Result.Reason += "; " + note
	}
	return plan, nil
}

// judgeEach places one copy of a sized plan on each node, every node on a
// fresh copy of the plan.
func (b *Backend) judgeEach(ctx context.Context, sized *fitPlan, idx presetIndex, nodes []string, forServe bool) ([]fitPlan, error) {
	out := make([]fitPlan, 0, len(nodes))
	for _, n := range nodes {
		per := *sized
		req := backend.FitRequest{Model: sized.Repo, Preset: sized.Result.Preset, Node: n}
		if err := b.placeModel(ctx, &per, idx, req, forServe); err != nil {
			return nil, err
		}
		out = append(out, per)
	}
	return out, nil
}

func nodeFits(nodes []string, each []fitPlan) []backend.NodeFit {
	out := make([]backend.NodeFit, len(each))
	for i, per := range each {
		out[i] = backend.NodeFit{Node: nodes[i], Fits: per.Result.Fits, Reason: per.Result.Reason}
	}
	return out
}

// judgeCopies writes the verdict of one copy on each of nodes into the plan:
// it fits when every copy fits. The answer's figures are the tightest node's
// — the first that refuses, else the one with the least room to spare — and
// its pool only when all the nodes share it.
func (b *Backend) judgeCopies(ctx context.Context, plan *fitPlan, idx presetIndex, nodes []string, forServe bool) error {
	each, err := b.judgeEach(ctx, plan, idx, nodes, forServe)
	if err != nil {
		return err
	}
	tight, pools := 0, map[string]bool{}
	var refusals []string
	for i, per := range each {
		pools[per.Result.Pool] = true
		if !per.Result.Fits {
			refusals = append(refusals, nodes[i]+": "+per.Result.Reason)
		}
		if tighter(per.Result, each[tight].Result) {
			tight = i
		}
	}
	*plan = each[tight]
	res := &plan.Result
	res.Placement, res.Nodes, res.Copies = backend.PlacementCopies, slices.Clone(nodes), nodeFits(nodes, each)
	if res.Node == "" { // a node that is no serving target
		res.Node = nodes[tight]
	}
	if len(pools) > 1 {
		res.Pool = ""
	}
	res.Fits = len(refusals) == 0
	// One LLMInferenceService runs one pod template: copies on nodes of
	// different device counts would leave some of them unschedulable; the
	// copies run at the lowest utilization a node leaves.
	shapes := map[servingShape][]string{}
	for i, per := range each {
		sh := servingShape{Devices: per.Result.DevicesPerPod, TensorParallel: per.Result.TensorParallel}
		shapes[sh] = append(shapes[sh], nodes[i])
		if u := per.Result.GPUMemoryUtilization; u > 0 {
			res.GPUMemoryUtilization = min(res.GPUMemoryUtilization, u)
		}
		res.CPURequestMillis = min(res.CPURequestMillis, per.Result.CPURequestMillis)
		res.MemoryRequestBytes = min(res.MemoryRequestBytes, per.Result.MemoryRequestBytes)
	}
	switch {
	case res.Fits && len(shapes) > 1:
		res.Fits = false
		res.Reason = fmt.Sprintf("the nodes need different serving shapes (%s) and copies share one; serve copies on nodes of one shape", describeShapes(shapes))
	case res.Fits:
		res.Reason = fmt.Sprintf("%d copies on %s; the tightest, %s: %s", len(nodes), strings.Join(nodes, ", "), res.Node, res.Reason)
	default:
		res.Reason = fmt.Sprintf("%d of %d nodes cannot host a copy — %s", len(refusals), len(nodes), strings.Join(refusals, "; "))
	}
	return nil
}

// describeShapes words the shapes of copies' nodes, in node order.
func describeShapes(shapes map[servingShape][]string) string {
	parts := make([]string, 0, len(shapes))
	for sh, on := range shapes {
		parts = append(parts, fmt.Sprintf("%s: %s, tensor parallel %d", strings.Join(on, ", "), plural(sh.Devices, "GPU device"), sh.TensorParallel))
	}
	slices.Sort(parts)
	return strings.Join(parts, "; ")
}

// tighter says whether verdict a leaves less room than b: a refusal before a
// fit, then the smaller free budget beyond what the copy requires.
func tighter(a, b backend.FitResult) bool {
	if a.Fits != b.Fits {
		return !a.Fits
	}
	return a.FreeBytes-a.RequiredBytes < b.FreeBytes-b.RequiredBytes
}

// applyCopies makes a composed or served LLMInferenceService run one copy on
// each of nodes: as many replicas, pinned to the nodes by affinity, one per
// node, and the placement recorded on the object.
func applyCopies(obj *unstructured.Unstructured, nodes []string) {
	spec, _ := obj.Object["spec"].(map[string]any)
	spec["replicas"] = int64(len(nodes))
	template, _ := spec["template"].(map[string]any)
	if template == nil {
		template = map[string]any{}
		spec["template"] = template
	}
	if ns, _ := template["nodeSelector"].(map[string]any); ns != nil {
		delete(ns, labelHostname)
		if len(ns) == 0 {
			delete(template, "nodeSelector")
		}
	}
	template["affinity"] = pinnedAffinity(obj.GetName(), workloadComponent, nodes)
	meta := obj.GetAnnotations()
	if meta == nil {
		meta = map[string]string{}
	}
	meta[PlacementAnnotation] = backend.PlacementCopies
	meta[NodesAnnotation] = strings.Join(nodes, ",")
	obj.SetAnnotations(meta)
}

// addCopies serves an already-served model on more nodes: the object's
// replicas grow to the nodes it runs on and the new ones, in that order. The
// fit was judged on the asked nodes by the caller.
func (b *Backend) addCopies(ctx context.Context, existing *unstructured.Unstructured, current, asked []string, dryRun bool) (*unstructured.Unstructured, []string, error) {
	nodes := slices.Clone(current)
	for _, n := range asked {
		if !slices.Contains(nodes, n) {
			nodes = append(nodes, n)
		}
	}
	obj := existing.DeepCopy()
	applyCopies(obj, nodes)
	if dryRun {
		return obj, nodes, nil
	}
	if _, err := b.dynamic(ctx).Resource(llmisvcGVR).Namespace(obj.GetNamespace()).Update(ctx, obj, metav1.UpdateOptions{FieldManager: ManagedByValue}); err != nil {
		return nil, nil, fmt.Errorf("update %s %s/%s: %w", kindLLMInferenceService, obj.GetNamespace(), obj.GetName(), err)
	}
	return obj, nodes, nil
}
