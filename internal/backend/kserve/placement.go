package kserve

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/model-manager/internal/backend"
)

// Placement (giantswarm/model-manager#190): a served model is either split
// across the nodes of one fast link — one vLLM instance, tensor parallel over
// the link — or served as copies. KServe's LLMInferenceService runs a model
// across nodes only as a LeaderWorkerSet whose size comes from data or
// pipeline parallelism, and the only multi-node template the platform ships
// runs data parallel; tensor parallelism alone gives it no worker group. A
// split is therefore composed in the data-parallel shape — parallelism
// {data: N, dataLocal: 1}, a leader template and a worker template — with
// the main container's command of both replaced by vLLM's own multi-node
// launch: the leader serves the API as node rank 0, the workers join headless
// with their LeaderWorkerSet index as node rank, all of them reaching the
// leader at LWS_LEADER_ADDRESS. Without a leader template's labels on the
// workers, the workload Service selects the leader alone — the only pod that
// answers.

const (
	// PlacementAnnotation and NodesAnnotation record how model-manager
	// placed a model on its LLMInferenceService: the placement and, for a
	// split, its nodes in rank order, for copies on several nodes, the
	// nodes (comma-separated).
	PlacementAnnotation = "model-manager.giantswarm.io/placement"
	NodesAnnotation     = "model-manager.giantswarm.io/nodes"

	// networksAnnotation is Multus' pod annotation naming the network
	// attachments a pod joins.
	networksAnnotation = "k8s.v1.cni.cncf.io/networks"
	// modelRoutingAnnotation is the annotation KServe's data-parallel
	// template sets to route by the model header; a split answers on one
	// leader like a single-node model, so it is turned off again.
	modelRoutingAnnotation = "serving.kserve.io/model-based-routing-enabled"
	// splitMasterPort is the port vLLM's multi-node rendezvous listens on at
	// the leader.
	splitMasterPort = 29500
)

// validatePlacement checks the placement of a request and returns it
// normalized: "" is copies. No node may be named twice.
func validatePlacement(placement string, nodes []string) (string, error) {
	p := strings.TrimSpace(placement)
	if p == "" {
		p = backend.PlacementCopies
	}
	if p != backend.PlacementCopies && p != backend.PlacementSplit {
		return "", fmt.Errorf("%w: placement %q: want %s or %s", backend.ErrInvalid, placement, backend.PlacementSplit, backend.PlacementCopies)
	}
	seen := map[string]bool{}
	for _, n := range nodes {
		if seen[n] {
			return "", fmt.Errorf("%w: node %s is named twice", backend.ErrInvalid, n)
		}
		seen[n] = true
	}
	if p == backend.PlacementSplit && len(nodes) == 1 {
		return "", fmt.Errorf("%w: a split needs two or more nodes", backend.ErrInvalid)
	}
	return p, nil
}

// fastLinkOf returns the fast link a node belongs to.
func (s settings) fastLinkOf(node string) (backend.FastLink, bool) {
	for _, l := range s.FastLinks {
		if l.Has(node) {
			return l, true
		}
	}
	return backend.FastLink{}, false
}

// splitLink resolves the fast link and the nodes of a split: the nodes asked
// for, which must all belong to one fast link, or — none asked for — the
// nodes of each fast link in turn. It returns the candidates in the order to
// try; an error when the asked nodes share no fast link.
func (s settings) splitLinks(nodes []string) ([]backend.FastLink, error) {
	if len(s.FastLinks) == 0 {
		return nil, fmt.Errorf("no fast link joins nodes on this cluster: a split runs only across nodes that share one (the discovery document's spec.fastLinks)")
	}
	if len(nodes) == 0 {
		return s.FastLinks, nil
	}
	link, ok := s.fastLinkOf(nodes[0])
	if !ok {
		return nil, fmt.Errorf("node %s shares no fast link with another node; a split runs only across nodes of one fast link, never over the cluster network", nodes[0])
	}
	for _, n := range nodes[1:] {
		if !link.Has(n) {
			return nil, fmt.Errorf("nodes %s and %s share no fast link; a split runs only across nodes of one fast link (%s: %s)", nodes[0], n, link.Name, strings.Join(link.Nodes, ", "))
		}
	}
	link.Nodes = nodes
	return []backend.FastLink{link}, nil
}

// placeSplit judges a split: every node of the fast link must be a serving
// target, hold its share of the weights (weights / nodes) beside the preset's
// overhead within its free budget, and fit its share of the KV cache — the
// tensor parallel degree is the preset's times the number of nodes. With no
// nodes asked for, the first fast link whose nodes all host the model is
// chosen. The answer's figures are those of the tightest node.
func (b *Backend) placeSplit(ctx context.Context, plan *fitPlan, idx presetIndex, req backend.FitRequest, forServe bool) error {
	res, p := &plan.Result, plan.Preset
	res.Placement = backend.PlacementSplit
	if p == nil {
		res.Reason = "a split is composed from a serving preset; no preset serves " + plan.Repo
		return nil
	}
	if p.cpu() {
		res.Reason = "preset " + p.name() + " requests no GPU; a split runs tensor parallel across GPUs"
		return nil
	}
	s := b.cfg.settings(ctx).forPreset(p)
	links, err := s.splitLinks(req.Nodes)
	if err != nil {
		res.Reason = err.Error()
		return nil
	}
	var loc cacheLocation
	if p.storesInCache() {
		if loc, err = b.cacheNodes(ctx); err != nil {
			return err
		}
	}
	plan.CacheLocal = len(loc.Nodes) > 0
	nodes, err := b.nodes(ctx, loc, p)
	if err != nil {
		return err
	}
	reserved, own, _ := b.reservedByNode(ctx, idx, p, nodes)
	plan.Nodes, plan.Own = nodes, own
	var refusals []string
	for _, link := range links {
		v := b.judgeSplit(ctx, plan, link, nodes, reserved, loc, forServe)
		if v.fits || len(links) == 1 {
			v.apply(res)
			if res.Fits && res.Gated && !res.TokenConfigured {
				res.Reason += "; the repository is gated and no hub token is configured"
			}
			return nil
		}
		refusals = append(refusals, link.Name+": "+v.reason)
	}
	res.Fits = false
	res.Reason = "no fast link hosts a split: " + strings.Join(refusals, "; ")
	return nil
}

// splitVerdict is the judgment of one fast link.
type splitVerdict struct {
	link   backend.FastLink
	fits   bool
	reason string
	// tight is the tightest node's budget, the one the answer reports.
	tight              nodeBudget
	reserved, required int64
	limit              int64
	kv                 kvVerdict
	shape              servingShape
	cached             bool
	cacheSource        string
}

func (v splitVerdict) apply(res *backend.FitResult) {
	res.Placement = backend.PlacementSplit
	res.FastLink = v.link.Name
	res.Nodes = v.link.Nodes
	res.Fits = v.fits
	res.Reason = v.reason
	if v.tight.Name == "" {
		return
	}
	res.Node = v.link.Nodes[0]
	res.BudgetBytes = v.tight.Budget
	res.BudgetSource = v.tight.BudgetSource
	res.ReservedBytes = v.reserved
	if v.required > 0 {
		// A split's requirement is per node: its share of the weights and the
		// overhead; WeightsBytes stays the whole model's.
		res.RequiredBytes = v.required
	}
	res.FreeBytes = max(v.tight.Budget-v.reserved, 0)
	if v.kv.Skip != "" || v.kv.MaxModelLen > 0 {
		applyKV(res, v.kv)
	}
	res.Cached, res.CacheSource = v.cached, v.cacheSource
	if v.fits && v.shape.Devices > 0 {
		res.DevicesPerPod, res.TensorParallel, res.GPUMemoryUtilization = v.shape.Devices, v.shape.TensorParallel, v.shape.Utilization
		res.CPURequestMillis, res.MemoryRequestBytes = v.shape.CPU, v.shape.Memory
	}
}

// judgeSplit judges one fast link's nodes: each must hold its share and, on
// unified memory, vLLM's claim; then the split's shape — the most devices a
// node needs on every pod, the lowest utilization a node leaves — must be
// schedulable on every node and fit its KV cache there.
func (b *Backend) judgeSplit(ctx context.Context, plan *fitPlan, link backend.FastLink, nodes []nodeBudget, reserved map[string]int64, loc cacheLocation, forServe bool) splitVerdict {
	res := &plan.Result
	v := splitVerdict{link: link}
	n := int64(len(link.Nodes))
	share := ceilDiv(res.WeightsBytes, n) + res.OverheadBytes
	var tightFree int64 = -1
	judged := make([]nodeBudget, 0, len(link.Nodes))
	var sh servingShape
	for _, name := range link.Nodes {
		candidates, why := b.candidateNodes(ctx, nodes, name, loc, plan.Preset)
		if len(candidates) == 0 {
			v.reason = why
			return v
		}
		node := candidates[0]
		if plan.CacheLocal && !slices.Contains(loc.Nodes, name) {
			v.reason = fmt.Sprintf("node %s cannot mount the cache claim %s, which holds the weights on %s; a split needs the weights on every node", name, b.cfg.settings(ctx).CacheClaim, strings.Join(loc.Nodes, ", "))
			return v
		}
		if node.Budget <= 0 {
			v.reason = fmt.Sprintf("node %s reports no memory budget (%s)", name, node.BudgetSource)
			return v
		}
		limit := node.Budget
		if forServe {
			limit = node.Budget - reserved[name]
		}
		if tightFree < 0 || limit < tightFree {
			tightFree = limit
			v.tight, v.reserved, v.limit = node, reserved[name], limit
		}
		if share > limit {
			v.tight, v.reserved, v.limit, v.required = node, reserved[name], limit, share
			v.reason = fmt.Sprintf("its share of a split across %s — %s of weights and %s overhead — exceeds the %s available on %s (%s budget %s%s)",
				strings.Join(link.Nodes, ", "), humanBytes(ceilDiv(res.WeightsBytes, n)), humanBytes(res.OverheadBytes), humanBytes(limit), name, node.BudgetSource, humanBytes(node.Budget), reservedNote(reserved[name]))
			return v
		}
		running := int64(0)
		if forServe {
			running = reserved[name]
		}
		nodeShape, uv := b.shapeOn(plan.Preset, node, running, ceilDiv(plan.Preset.residentBytes(res.WeightsBytes), n)+res.OverheadBytes, n)
		if uv.Checked && !uv.Fits {
			v.tight, v.reserved, v.limit, v.required = node, reserved[name], limit, share
			v.reason = fmt.Sprintf("a split across %s fits the weights, but on %s %s", strings.Join(link.Nodes, ", "), name, uv.clause())
			return v
		}
		if refusal := nodeShape.fitRequests(plan.Preset, node, b.roomOn(ctx, node, plan.Preset)); refusal != "" {
			v.reason = fmt.Sprintf("a split across %s fits the weights, but %s", strings.Join(link.Nodes, ", "), refusal)
			return v
		}
		if len(judged) == 0 {
			sh = nodeShape
		}
		sh.Devices = max(sh.Devices, nodeShape.Devices)
		sh.Utilization = min(sh.Utilization, nodeShape.Utilization)
		sh.CPU, sh.Memory = min(sh.CPU, nodeShape.CPU), min(sh.Memory, nodeShape.Memory)
		judged = append(judged, node)
		cached, source := b.cacheVerdict(ctx, name, plan, loc)
		if name == link.Nodes[0] || !cached {
			v.cached, v.cacheSource = cached, source
		}
	}
	sh.TensorParallel = sh.Devices * n
	v.shape = sh
	kv := plan.KV.shaped(sh)
	for _, node := range judged {
		if clause := devicesClause(sh.Devices, node); clause != "" {
			v.reason = fmt.Sprintf("a split across %s fits the weights, but %s", strings.Join(link.Nodes, ", "), clause)
			return v
		}
		if kvv := kv.judgeOn(node); kvv.Skip == "" && !kvv.Fits {
			v.kv = kvv
			v.reason = fmt.Sprintf("a split across %s fits the weights, but on %s %s", strings.Join(link.Nodes, ", "), node.Name, kvv.clause())
			return v
		} else {
			v.kv = kvv
		}
	}
	v.fits, v.required = true, share
	v.reason = fmt.Sprintf("split across %s (fast link %s): %s of weights and %s overhead per node fit within %s on the tightest node %s (%s%s)",
		strings.Join(link.Nodes, ", "), link.Name, humanBytes(ceilDiv(res.WeightsBytes, n)), humanBytes(res.OverheadBytes), humanBytes(v.limit), v.tight.Name, v.tight.BudgetSource, reservedNote(v.reserved))
	if p := plan.Preset; p != nil {
		if sh.Utilization > 0 && sh.Utilization != p.utilization() {
			v.reason += fmt.Sprintf("; %s=%s, sized down from the preset's %s so the unified-memory nodes keep their host headroom", flagGPUMemoryUtilization, trimFloat2(sh.Utilization), trimFloat2(p.utilization()))
		}
		if note := requestsNote(p, sh, "the tightest node"); note != "" {
			v.reason += "; " + note
		}
	}
	return v
}

// recommend adds the backend's recommended placement to a fit answer: split
// across the first fast link whose nodes all host the model, copies on the
// judged node otherwise (giantswarm/model-manager#190). A request that asked
// for a split carries its own verdict as the recommendation when it fits.
func (b *Backend) recommend(ctx context.Context, res *backend.FitResult, req backend.FitRequest) {
	if res.Placement == backend.PlacementSplit && res.Fits {
		res.Recommended, res.RecommendedNodes = backend.PlacementSplit, res.Nodes
		return
	}
	s := b.cfg.settings(ctx)
	if len(s.FastLinks) > 0 {
		split := req
		split.Placement, split.Nodes = backend.PlacementSplit, nil
		if plan, err := b.splitCheck(ctx, split, false); err == nil && plan.Result.Fits {
			res.Recommended, res.RecommendedNodes = backend.PlacementSplit, plan.Result.Nodes
			return
		}
	}
	res.Recommended = backend.PlacementCopies
	if res.Node != "" {
		res.RecommendedNodes = []string{res.Node}
	}
}

// splitCheck is fitCheck for placement split.
func (b *Backend) splitCheck(ctx context.Context, req backend.FitRequest, forServe bool) (*fitPlan, error) {
	plan, idx, err := b.resolveFit(ctx, req)
	if err != nil {
		return nil, err
	}
	note, err := b.sizeModel(ctx, plan)
	if err != nil {
		return nil, err
	}
	plan.KV = b.kvCheckFor(ctx, plan)
	if err := b.placeSplit(ctx, plan, idx, req, forServe); err != nil {
		return nil, err
	}
	if note = joinNotes(note, placementNote(plan)); note != "" {
		plan.Result.Reason += "; " + note
	}
	return plan, nil
}

// composeSplit builds the LLMInferenceService of a split across nodes (in
// rank order): the single-node composition as the leader template, pinned to
// the first node, a copy of it as the worker template, pinned to the others
// and spread one per node, both running vLLM's multi-node launch, joined to
// the fast link's networks, requesting its devices, with its environment and
// the preset's split environment over it (by name: a later source replaces an
// entry of the same name);
// image the runtime of the single-node template (settings.TemplateImage),
// which a split runs too — the multi-node template names the stock image, an
// installation's runtime override only the single-node one — unless the
// preset names one.
func (b *Backend) composeSplit(p *servingPreset, s settings, link backend.FastLink, sh servingShape) *unstructured.Unstructured {
	nodes := link.Nodes
	obj := b.composeLLM(p, s, nodes[0], sh)
	spec := obj.Object["spec"].(map[string]any)
	leader := spec["template"].(map[string]any)
	worker := deepCopyMap(leader)

	for i, tpl := range []map[string]any{leader, worker} {
		main := templateMain(tpl)
		main["command"] = splitCommand(len(nodes), sh.TensorParallel, i == 1)
		args := withoutFlag(shapeArgs(p, sh), flagTensorParallelSize)
		if i == 0 { // the leader serves the API; a worker runs headless
			args = append(args, servedNameArgs(p, s.Namespace)...)
		}
		if len(args) > 0 {
			main["args"] = toAnySlice(args)
		} else {
			delete(main, "args")
		}
		if _, set := main["image"]; !set && s.TemplateImage != "" {
			main["image"] = s.TemplateImage
		}
		mergeEnv(main, linkEnv(link.Env))
		mergeEnv(main, p.Spec.Split.Env)
		addResources(main, link.Resources)
		main["securityContext"] = splitSecurityContext()
	}
	ns, _ := worker["nodeSelector"].(map[string]any)
	delete(ns, labelHostname)
	if len(ns) == 0 {
		delete(worker, "nodeSelector")
	}
	worker["affinity"] = pinnedAffinity(obj.GetName(), workerComponent, nodes[1:])

	spec["worker"] = worker
	spec["parallelism"] = map[string]any{"data": int64(len(nodes)), "dataLocal": int64(1)}
	annotations := map[string]any{modelRoutingAnnotation: "false"}
	if len(link.Networks) > 0 {
		annotations[networksAnnotation] = strings.Join(link.Networks, ",")
	}
	spec["annotations"] = annotations

	meta := obj.GetAnnotations()
	meta[PlacementAnnotation] = backend.PlacementSplit
	meta[NodesAnnotation] = strings.Join(nodes, ",")
	obj.SetAnnotations(meta)
	return obj
}

// splitCommand is the main container's command of a split of n nodes
// running tensor parallel tp (at least n: one device per pod):
// vLLM's multi-node launch with the leader's address resolved to an IP (the
// rendezvous binds to it), the leader serving the API on the template's port
// and TLS as KServe's single-node template does, a worker headless. The
// preset's arguments follow as the container's args ("$@").
func splitCommand(n int, tp int64, worker bool) []any {
	rank, serve := "0", `--served-model-name "{{ .Spec.Model.Name }}" "publishers/{{ .ObjectMeta.Namespace }}/models/{{ .Spec.Model.Name }}" \
  --port `+strconv.Itoa(llmisvcWorkloadPort)+` \
  {{ if .GlobalConfig.EnableTLS }}--enable-ssl-refresh --ssl-certfile /var/run/kserve/tls/tls.crt --ssl-keyfile /var/run/kserve/tls/tls.key{{ end }}`
	if worker {
		rank, serve = `"${LWS_WORKER_INDEX}"`, "--headless"
	}
	script := `for i in $(seq 1 60); do
  MASTER_ADDR=$(getent hosts "${LWS_LEADER_ADDRESS}" | cut -d' ' -f1)
  [ -n "$MASTER_ADDR" ] && break
  echo "waiting for ${LWS_LEADER_ADDRESS} to resolve ($i)"; sleep 2
done
[ -n "$MASTER_ADDR" ] || { echo "the leader address ${LWS_LEADER_ADDRESS} did not resolve"; exit 1; }
eval "set -- $*"
exec vllm serve /mnt/models \
  --tensor-parallel-size ` + strconv.FormatInt(max(tp, int64(n)), 10) + ` --nnodes ` + strconv.Itoa(n) + ` --node-rank ` + rank + ` \
  --master-addr "$MASTER_ADDR" --master-port ` + strconv.Itoa(splitMasterPort) + ` \
  ` + serve + ` \
  "$@"`
	return []any{"/bin/bash", "-c", script, "--"}
}

// splitSecurityContext is the main container's security context of a split:
// KServe's multi-node template adds IPC_LOCK, SYS_RAWIO and NET_RAW, which
// the baseline Pod Security Standard refuses; NCCL over the fast link needs
// none of them (RDMA pins its buffers within the container's memlock limit),
// so a split runs with the capabilities a single-node model pod has. The
// list replaces the template's (KServe merges it as a whole).
func splitSecurityContext() map[string]any {
	return map[string]any{
		"allowPrivilegeEscalation": false,
		"capabilities": map[string]any{
			"drop": []any{"ALL"},
			"add":  []any{"NET_BIND_SERVICE"},
		},
		"runAsNonRoot":   true,
		"seccompProfile": map[string]any{"type": "RuntimeDefault"},
	}
}

// pinnedAffinity pins an object's pods of one component — a split's workers,
// the copies — to their nodes, one per node: the nodes by hostname, and no
// two of those pods on one node.
func pinnedAffinity(name, component string, nodes []string) map[string]any {
	return map[string]any{
		"nodeAffinity": map[string]any{
			"requiredDuringSchedulingIgnoredDuringExecution": map[string]any{
				"nodeSelectorTerms": []any{map[string]any{
					"matchExpressions": []any{map[string]any{
						"key": labelHostname, "operator": "In", "values": toAnySlice(nodes),
					}},
				}},
			},
		},
		"podAntiAffinity": map[string]any{
			"requiredDuringSchedulingIgnoredDuringExecution": []any{map[string]any{
				"topologyKey": labelHostname,
				"labelSelector": map[string]any{"matchLabels": map[string]any{
					"app.kubernetes.io/name": name, "app.kubernetes.io/component": component,
				}},
			}},
		},
	}
}

// servedPlacement reads the placement model-manager recorded on an object:
// its placement and its nodes (a split's in rank order, the copies'); copies
// without nodes for an object without.
func servedPlacement(obj *unstructured.Unstructured) (string, []string) {
	a := obj.GetAnnotations()
	placement := backend.PlacementCopies
	if a[PlacementAnnotation] == backend.PlacementSplit {
		placement = backend.PlacementSplit
	}
	var nodes []string
	for _, n := range strings.Split(a[NodesAnnotation], ",") {
		if n = strings.TrimSpace(n); n != "" {
			nodes = append(nodes, n)
		}
	}
	return placement, nodes
}

func templateMain(tpl map[string]any) map[string]any {
	containers, _ := tpl["containers"].([]any)
	for _, c := range containers {
		if cm, ok := c.(map[string]any); ok && cm["name"] == llmisvcMainContainer {
			return cm
		}
	}
	main := map[string]any{"name": llmisvcMainContainer}
	tpl["containers"] = append(containers, main)
	return main
}

// withoutFlag drops a flag and its value from vLLM arguments, in both the
// "--flag value" and the "--flag=value" form.
func withoutFlag(args []string, flag string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == flag:
			i++
		case strings.HasPrefix(args[i], flag+"="):
		default:
			out = append(out, args[i])
		}
	}
	return out
}

// mergeEnv sets env on the main container by name: an entry whose name the
// container already carries replaces it in place, the others follow in order.
// The LLMInferenceService API refuses two entries of one name.
func mergeEnv(main map[string]any, env []map[string]any) {
	if len(env) == 0 {
		return
	}
	list, _ := main["env"].([]any)
	at := make(map[string]int, len(list))
	for i, e := range list {
		if m, ok := e.(map[string]any); ok {
			if name, ok := m["name"].(string); ok {
				at[name] = i
			}
		}
	}
	for _, e := range env {
		name, _ := e["name"].(string)
		if i, ok := at[name]; ok {
			list[i] = e
			continue
		}
		at[name] = len(list)
		list = append(list, e)
	}
	main["env"] = list
}

// linkEnv is a fast link's environment as container env entries.
func linkEnv(env []backend.EnvVar) []map[string]any {
	out := make([]map[string]any, 0, len(env))
	for _, e := range env {
		out = append(out, map[string]any{"name": e.Name, "value": e.Value})
	}
	return out
}

func addResources(main map[string]any, extra map[string]string) {
	if len(extra) == 0 {
		return
	}
	res, _ := main["resources"].(map[string]any)
	if res == nil {
		res = map[string]any{}
		main["resources"] = res
	}
	for _, kind := range []string{"requests", "limits"} {
		m, _ := res[kind].(map[string]any)
		if m == nil {
			m = map[string]any{}
			res[kind] = m
		}
		for k, v := range extra {
			m[k] = v
		}
	}
}

func deepCopyMap(in map[string]any) map[string]any {
	return (&unstructured.Unstructured{Object: in}).DeepCopy().Object
}
