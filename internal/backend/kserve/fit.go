package kserve

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/giantswarm/model-manager/internal/backend"
)

const (
	weightsSourceIndex  = "safetensors-index"
	weightsSourceShards = "safetensors-shards"
	weightsSourceTree   = "tree"
	weightsSourcePreset = "preset"
	// weightsSourceModelImage: the weights label of the model image a preset
	// is served from (giantswarm/model-manager#189).
	weightsSourceModelImage = "model-image"
	// budgetSourcePoolScaleFromZero marks a fit answered without a node: the
	// GPU pool (spec.gpuPool.nodeSelector) has no node yet and scales from
	// zero once a predictor is pending, so the fit is against the pool, not
	// a node's budget (giantswarm/model-manager#90).
	budgetSourcePoolScaleFromZero = "pool-scale-from-zero"
)

// fitPlan is a fit check plus everything the caller needs afterwards.
type fitPlan struct {
	Result   backend.FitResult
	Preset   *servingPreset
	Repo     string
	Revision string
	Hub      *hubModel
	Files    []hubFile
	// Dir is the cache directory the model lives in (preset name when a
	// preset serves it, else derived from the repository id).
	Dir string
	// CacheLocal is true when the cache claim is pinned to nodes.
	CacheLocal bool
	// KV is the KV cache check the placement runs on each GPU it judges.
	KV *kvCheck
	// Image is the model image of a preset served from one, as its registry
	// described it; nil for every other model, or when it did not answer.
	Image *modelImage
	// Nodes are the nodes the placement judged; Own is what the preset
	// itself already holds on the nodes it serves on.
	Nodes []nodeBudget
	Own   map[string]int64
}

// fitCheck sizes a model (hub, falling back to the preset) and compares
// weights + overhead with the free budget of the target node — what the
// node's running LLMInferenceServices already need subtracted — and its KV
// cache with the GPU: the answer of check_fit and the check of load_model,
// one verdict for both.
func (b *Backend) fitCheck(ctx context.Context, req backend.FitRequest) (*fitPlan, error) {
	plan, idx, err := b.resolveFit(ctx, req)
	if err != nil {
		return nil, err
	}
	if err := b.judgeFit(ctx, plan, idx, req, true); err != nil {
		return nil, err
	}
	return plan, nil
}

// judgeFit completes a resolved plan: sizes the model, reads its KV cache
// layout, places it, and notes in the answer when the preset stood in for
// the hub. Split from fitCheck so a caller can look at the resolved preset
// before the hub is asked (Pull). forServe judges against the node's free
// budget; a pull only asks whether the model can ever be served there. The
// reservation is answered either way.
func (b *Backend) judgeFit(ctx context.Context, plan *fitPlan, idx presetIndex, req backend.FitRequest, forServe bool) error {
	note, err := b.sizeModel(ctx, plan)
	if err != nil {
		return err
	}
	plan.KV = b.kvCheckFor(ctx, plan)
	if err := b.placeModel(ctx, plan, idx, req, forServe); err != nil {
		return err
	}
	if note = joinNotes(note, placementNote(plan)); note != "" {
		plan.Result.Reason += "; " + note
	}
	return nil
}

// resolveFit turns the request into the plan's skeleton: the repository (the
// model reference, else the preset's model), the preset that serves it and
// the cache directory — plus the preset index the placement reads
// reservations from.
func (b *Backend) resolveFit(ctx context.Context, req backend.FitRequest) (*fitPlan, presetIndex, error) {
	model := strings.TrimSpace(req.Model)
	if model == "" && req.Preset == "" {
		return nil, presetIndex{}, fmt.Errorf("%w: model or preset is required", backend.ErrInvalid)
	}
	repo, revision := splitRevision(model)

	presets, _, err := b.presets(ctx)
	if err != nil {
		return nil, presetIndex{}, err
	}
	idx := indexPresets(presets)
	p, err := idx.resolve(repo, req.Preset)
	if err != nil {
		return nil, idx, err
	}
	if p != nil && (repo == "" || repo == p.name()) {
		repo = p.Spec.Model.ID
	}
	if !isRepoID(repo) {
		return nil, idx, fmt.Errorf("%w: %q is neither a Hugging Face repository (owner/name) nor a preset name", backend.ErrInvalid, model)
	}

	plan := &fitPlan{Preset: p, Repo: repo, Revision: revision}
	res := &plan.Result
	res.Model = repo
	res.TokenConfigured = b.tokenConfigured(ctx)
	for _, m := range idx.forModel(repo) {
		res.Presets = append(res.Presets, m.name())
	}
	if p != nil {
		res.Preset = p.name()
		plan.Dir = p.name()
	} else {
		plan.Dir = dnsLabel(repo)
	}
	return plan, idx, nil
}

// sizeModel resolves the weights and what a pull downloads: from the hub —
// the safetensors index (its total_size, or the shards it names when they
// contradict it), else the file tree — within the hub lookup timeout,
// and from the preset's requirements when the hub cannot tell (gated without
// a token, unreachable, not answering in time). What a preset served from a
// model image downloads is the image's layers, read from its registry and
// nowhere else. It returns the note the answer carries when the preset stood
// in for the hub or the image's size could not be read.
func (b *Backend) sizeModel(ctx context.Context, plan *fitPlan) (string, error) {
	res, p, repo := &plan.Result, plan.Preset, plan.Repo
	if p != nil && !p.storesInCache() {
		return b.sizeModelImage(ctx, plan)
	}
	// Bounded on the caller's context: a hub whose packets an egress policy
	// drops must leave the fallback time to answer within the caller's
	// meta-tool deadline (giantswarm/model-manager#88).
	hctx, hubBudget, cancel := b.hubContext(ctx)
	defer cancel()
	var note string
	hub, err := b.hub.Model(hctx, repo)
	switch {
	case err == nil:
		plan.Hub = hub
		res.Gated = hub.isGated()
		res.Private = hub.Private
		files, err := b.hub.Tree(hctx, repo, plan.Revision)
		if err != nil {
			return "", hubFailure(err, hubBudget)
		}
		plan.Files = files
		res.DownloadBytes = downloadTotal(files, b.opts.DownloadIgnorePatterns)
		idx, err := b.hub.SafetensorsIndex(hctx, repo, plan.Revision, files)
		if err != nil {
			b.log.Warn("reading the safetensors index failed; summing the tree instead", "model", repo, "error", err)
		}
		fromIndex, indexSource := weightsFromIndex(idx, files)
		if indexSource == weightsSourceShards {
			b.log.Info("the safetensors index declares a total_size the shards its weight_map names contradict; sized from the shards", "model", repo, "total_size", idx.TotalSize, "shards", fromIndex, "shardFiles", len(idx.Shards))
		}
		switch {
		case fromIndex > 0:
			res.WeightsBytes, res.WeightsSource = fromIndex, indexSource
		case weightsFromTree(files) > 0:
			res.WeightsBytes, res.WeightsSource = weightsFromTree(files), weightsSourceTree
		case p != nil:
			res.WeightsBytes, res.WeightsSource = p.weightsBytes(), weightsSourcePreset
		}
	case p != nil:
		// Gated without token, hub down or silent: the preset's numbers still
		// allow a fit check — and the answer says so.
		b.log.Warn("hub lookup failed; using the preset's requirements", "model", repo, "error", err)
		res.WeightsBytes, res.WeightsSource = p.weightsBytes(), weightsSourcePreset
		res.Gated = errors.Is(err, backend.ErrInvalid)
		note = "weights from the preset's requirements: " + describeHubFailure(err, hubBudget)
	default:
		return "", hubFailure(err, hubBudget)
	}
	if res.WeightsBytes <= 0 {
		return "", fmt.Errorf("%w: cannot determine the weight size of %s (no safetensors index, no weight files, no preset)", backend.ErrInvalid, repo)
	}
	b.addOverhead(res, p)
	return note, nil
}

// sizeModelImage sizes a preset served from an OCI model image from the
// image alone (giantswarm/model-manager#189): the weights from its label,
// else the preset's requirements; the download from its layers; the KV cache
// layout from the config.json it carries (kvCheckFor). The Hub is not where
// its weights come from and is not asked. A registry that does not answer
// leaves the download unknown and the preset's numbers standing, and says so.
func (b *Backend) sizeModelImage(ctx context.Context, plan *fitPlan) (string, error) {
	res, p := &plan.Result, plan.Preset
	ictx, _, cancel := b.hubContext(ctx)
	defer cancel()
	var note string
	img, err := b.modelImage(ictx, p.Spec.Model.StorageURI)
	if err != nil {
		b.log.Warn("reading the model image failed", "model", plan.Repo, "image", p.Spec.Model.StorageURI, "error", err)
		note = "the download size is unknown: " + err.Error()
	} else {
		plan.Image = &img
		res.DownloadBytes = img.Bytes
	}
	res.WeightsBytes, res.WeightsSource = p.weightsBytes(), weightsSourcePreset
	if img.WeightsBytes > 0 {
		res.WeightsBytes, res.WeightsSource = img.WeightsBytes, weightsSourceModelImage
	}
	if res.WeightsBytes <= 0 {
		return "", fmt.Errorf("%w: cannot determine the weight size of %s (the model image %s carries no weights label and the preset declares no requirements.weightsGiB)", backend.ErrInvalid, plan.Repo, p.Spec.Model.StorageURI)
	}
	b.addOverhead(res, p)
	return note, nil
}

// addOverhead completes a sized answer: the preset's declared weights and
// overhead (the default without a preset) and the sum required.
func (b *Backend) addOverhead(res *backend.FitResult, p *servingPreset) {
	if p != nil {
		res.DeclaredWeightsBytes = p.weightsBytes()
		res.OverheadBytes = p.overheadBytes(b.opts.DefaultOverheadGiB)
	} else {
		res.OverheadBytes = gibToBytes(b.opts.DefaultOverheadGiB)
	}
	res.RequiredBytes = res.WeightsBytes + res.OverheadBytes
}

// placementNote completes a placed answer with what the nodes say beyond
// the budget: which nodes already hold the model image of a preset served
// from one — nothing to download when the model goes to one of them — and
// where the preset already serves, holding what (giantswarm/model-manager#189).
func placementNote(plan *fitPlan) string {
	res, p := &plan.Result, plan.Preset
	var notes []string
	if p != nil && !p.storesInCache() {
		res.PrePulledNodes = nil
		for _, n := range plan.Nodes {
			if imageOnNode(p.Spec.Model.StorageURI, n.Images) {
				res.PrePulledNodes = append(res.PrePulledNodes, n.Name)
			}
		}
		switch {
		case res.Node != "" && containsString(res.PrePulledNodes, res.Node):
			res.DownloadBytes = 0
			notes = append(notes, "served from the model image, pre-pulled on "+strings.Join(res.PrePulledNodes, ", "))
		case len(res.PrePulledNodes) > 0:
			notes = append(notes, "the model image is pre-pulled on "+strings.Join(res.PrePulledNodes, ", ")+", not on the node it goes to")
		}
	}
	res.ServingNodes = nil
	for node := range plan.Own {
		res.ServingNodes = append(res.ServingNodes, node)
	}
	sort.Strings(res.ServingNodes)
	for _, node := range res.ServingNodes {
		notes = append(notes, fmt.Sprintf("%s already serves on %s, holding %s there (left out of this answer: serving it again is a no-op)", p.name(), node, humanBytes(plan.Own[node])))
	}
	return joinNotes(notes...)
}

// hubContext bounds the hub lookups of one call (fit check, search) by
// opts.HFTimeout — less when the caller's deadline is nearer: the hub gets what
// the deadline leaves after deadlineReserve, so the Kubernetes side of the
// call still answers in time (giantswarm/model-manager#104). The budget is
// returned for the message a failed lookup carries.
func (b *Backend) hubContext(ctx context.Context) (context.Context, time.Duration, context.CancelFunc) {
	budget := b.opts.HFTimeout
	if dl, ok := ctx.Deadline(); ok {
		if left := time.Until(dl) - deadlineReserve; left < budget {
			budget = max(left, 0)
		}
	}
	hctx, cancel := context.WithTimeout(ctx, budget)
	return hctx, budget, cancel
}

// hubFailure is the error a call answers when a hub lookup fails: a hub that
// did not answer within the lookup timeout is named as such; every other
// error passes through.
func hubFailure(err error, timeout time.Duration) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%s: %w", describeHubFailure(err, timeout), err)
	}
	return err
}

// describeHubFailure words a failed hub lookup for a person: the hub did not
// answer in time, refused the repository (gated or private), or failed.
func describeHubFailure(err error, timeout time.Duration) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Sprintf("the Hugging Face Hub did not answer within %s", timeout)
	case errors.Is(err, backend.ErrInvalid):
		return "the Hugging Face Hub refused the repository metadata (gated or private)"
	default:
		return "the Hugging Face Hub lookup failed (" + err.Error() + ")"
	}
}

// placeModel picks the node the model is checked against and writes the
// verdict into the plan: the explicit node, else the eligible node with the
// most free budget, cache nodes first; a GPU pool at scale-to-zero answers
// without a node. With several pools and none pinning every predictor
// (settings.GPUPools), the verdict names the pool the model goes to — the
// chosen node's, or a pool with no node yet whose size hosts the model when
// no node does — and load_model pins the predictor there
// (giantswarm/model-manager#152). The cache claim has a say for a preset that stores in it
// (storesInCache) only: an oci:// preset is judged without the claim's
// location — no node pin, no cache-node preference, no scan — and its cache
// verdict is the oci-image one (giantswarm/model-manager#123).
func (b *Backend) placeModel(ctx context.Context, plan *fitPlan, idx presetIndex, req backend.FitRequest, forServe bool) error {
	res, p := &plan.Result, plan.Preset
	var loc cacheLocation
	if p.storesInCache() {
		var err error
		if loc, err = b.cacheNodes(ctx); err != nil {
			return err
		}
	}
	plan.CacheLocal = len(loc.Nodes) > 0
	nodes, err := b.nodes(ctx, loc, p)
	if err != nil {
		return err
	}
	reserved, own := b.reservedByNode(ctx, idx, p, nodes)
	plan.Nodes, plan.Own = nodes, own
	candidates, why := b.candidateNodes(ctx, nodes, req.Node, loc, p)
	if len(candidates) == 0 && b.cfg.recheckDiscovery(ctx) {
		// The discovery document appeared, or named the GPU pool, since the
		// settings were cached (giantswarm/model-manager#127): the nodes and
		// their eligibility are judged again on what it says — the pool
		// above all.
		if nodes, err = b.nodes(ctx, loc, p); err != nil {
			return err
		}
		plan.Nodes = nodes
		candidates, why = b.candidateNodes(ctx, nodes, req.Node, loc, p)
	}
	if len(candidates) == 0 {
		// A GPU pool at scale-to-zero (giantswarm/model-manager#90): the pool
		// selector names a pool no node belongs to yet. Serving is what
		// brings the node — the predictor carries the pool's toleration and
		// selector, goes Pending, and the autoscaler launches it — so the
		// answer is given without a node: judged against the pool's instance
		// shapes when they are known (giantswarm/model-manager#97), else yes
		// and unverified. An explicit node, a pool with nodes that do not
		// fit, or no pool at all keep the refusal — and a CPU preset knows
		// no pool (settings.forPreset): its refusal names the nodes.
		s := b.cfg.settings(ctx).forPreset(p)
		if pool := s.GPUPool; req.Node == "" && len(pool.NodeSelector) > 0 && !anyNodeMatches(nodes, pool.NodeSelector) {
			// The claim may hold the weights from an earlier serve
			// (giantswarm/model-manager#110): a shared claim is asked
			// without a node, a pinned one on its node.
			res.Cached, res.CacheSource = b.cacheVerdict(ctx, "", plan, loc)
			return b.placeOnPool(plan, pool, nil)
		}
		// A pool whose nodes all still start competes as a pool with no node
		// yet: its node is arriving.
		if name, pool, ok, err := emptyPoolFor(plan, s, settledNodes(nodes), req.Node); err != nil || ok {
			if err != nil {
				return err
			}
			res.Cached, res.CacheSource = b.cacheVerdict(ctx, "", plan, loc)
			res.Pool = name
			return b.placeOnPool(plan, pool, startingIn(nodes, pool.NodeSelector))
		}
		// A node that is no serving target only because it still starts
		// (start-up taints, not ready yet: starting) is capacity arriving,
		// not a refusal: the predictor waits for it, judged like a pool with
		// no node yet (giantswarm/model-manager#243).
		if arriving := startingIn(nodes, presetSelector(p)); req.Node == "" && len(arriving) > 0 {
			pool := s.GPUPool
			if name := arriving[0].Labels[labelMachinePool]; len(pool.NodeSelector) == 0 && name != "" {
				if known, ok := s.GPUPools[name]; ok {
					pool, res.Pool = known, name
					pool.NodeSelector, pool.Taint = map[string]string{labelMachinePool: name}, s.GPUPool.Taint
					arriving = startingIn(arriving, pool.NodeSelector)
				}
			}
			res.Cached, res.CacheSource = b.cacheVerdict(ctx, "", plan, loc)
			return b.placeOnPool(plan, pool, arriving)
		}
		res.Fits = false
		res.Reason = why
		// Without the discovery document, or with one that does not name
		// the pool whose shapes are known yet, the driver knows no pool: the
		// answer names what the document lacks and says to retry — "no
		// accelerator node" is the verdict for a document that names no
		// pool. The nodes' own reasons stay when there are nodes.
		if missing := b.cfg.discoveryMissing(s); req.Node == "" && missing != "" {
			res.Reason = missing
			if len(nodes) > 0 {
				res.Reason += "; " + why
			}
			res.Retryable = true
		}
		return nil
	}
	best := candidates[0]
	bestFree := best.Budget - reserved[best.Name]
	for _, n := range candidates[1:] {
		if free := n.Budget - reserved[n.Name]; free > bestFree {
			best, bestFree = n, free
		}
	}
	res.Node = best.Name
	res.BudgetBytes = best.Budget
	res.BudgetSource = best.BudgetSource
	res.ReservedBytes = reserved[best.Name]
	res.FreeBytes = best.Budget - res.ReservedBytes
	if res.FreeBytes < 0 {
		res.FreeBytes = 0
	}
	if best.Budget <= 0 {
		res.Reason = fmt.Sprintf("node %s reports no memory budget (%s)", best.Name, best.BudgetSource)
		return nil
	}
	limit := res.BudgetBytes
	if forServe {
		limit = res.FreeBytes
	}
	res.Fits = res.RequiredBytes <= limit
	if res.Fits {
		res.Reason = fmt.Sprintf("%s fit within %s on %s (%s%s)",
			weightsNeed(res), humanBytes(limit), best.Name, best.BudgetSource, reservedNote(res.ReservedBytes))
	} else {
		res.Reason = fmt.Sprintf("%s exceed the %s available on %s (%s budget %s%s)",
			weightsNeed(res), humanBytes(limit), best.Name, best.BudgetSource, humanBytes(res.BudgetBytes), reservedNote(res.ReservedBytes))
	}
	running := int64(0)
	if forServe {
		running = res.ReservedBytes
	}
	sh, uv := b.shapeOn(p, best, running, p.residentBytes(res.WeightsBytes)+res.OverheadBytes, 1)
	applyUnified(res, uv)
	applyShape(res, p, sh, best, b.roomOn(ctx, best, p))
	applyKV(res, plan.KV.shaped(sh).judgeOn(best))
	if res.Gated && !res.TokenConfigured {
		res.Reason += "; the repository is gated and no hub token is configured"
	}
	res.Cached, res.CacheSource = b.cacheVerdict(ctx, best.Name, plan, loc)
	s := b.cfg.settings(ctx).forPreset(p)
	if len(s.GPUPool.NodeSelector) > 0 || len(s.GPUPools) == 0 {
		return nil
	}
	res.Pool = best.Labels[labelMachinePool]
	if res.Fits {
		return nil
	}
	// The best node does not fit; a pool that has no node yet may.
	name, pool, ok, err := emptyPoolFor(plan, s, settledNodes(nodes), req.Node)
	if err != nil || !ok {
		return err
	}
	node, why := res.Node, res.Reason
	res.Node, res.ReservedBytes = "", 0
	res.Cached, res.CacheSource = b.cacheVerdict(ctx, "", plan, loc)
	res.Pool = name
	if err := b.placeOnPool(plan, pool, startingIn(nodes, pool.NodeSelector)); err != nil {
		return err
	}
	res.Reason += fmt.Sprintf("; the ready node %s does not host it (%s)", node, why)
	return nil
}

// labelMachinePool is the node label a GPU pool stamps on its nodes: the
// pool's release name, giantswarm.io/machine-pool=<cluster>-<pool>.
const labelMachinePool = "giantswarm.io/machine-pool"

// emptyPoolFor picks, among the cluster's pools (settings.GPUPools) that
// have no node yet — the caller passes the nodes that are not starting — the one the model goes to: the pool whose smallest
// hosting size is the smallest (by vCPU, then memory), the pool named by
// name order on a tie. ok is false with an explicit node, while a pool
// selector pins every predictor, or when no such pool has a size that hosts
// the model — the caller's verdict then stands. The pool comes back with
// its label as the selector and the slice's taint.
func emptyPoolFor(plan *fitPlan, s settings, nodes []nodeBudget, explicit string) (string, backend.GPUPool, bool, error) {
	if explicit != "" || len(s.GPUPool.NodeSelector) > 0 || len(s.GPUPools) == 0 {
		return "", backend.GPUPool{}, false, nil
	}
	needs, err := needsOf(plan)
	if err != nil {
		return "", backend.GPUPool{}, false, err
	}
	names := make([]string, 0, len(s.GPUPools))
	for name := range s.GPUPools {
		names = append(names, name)
	}
	sort.Strings(names)
	var bestName string
	var bestShape *backend.InstanceShape
	for _, name := range names {
		if anyNodeMatches(nodes, map[string]string{labelMachinePool: name}) {
			continue
		}
		for _, shape := range sortedShapes(s.GPUPools[name].Instances) {
			if !hosts(shape, needs) {
				continue
			}
			if bestShape == nil || smallerShape(shape, *bestShape) {
				bestName, bestShape = name, &shape
			}
			break
		}
	}
	if bestShape == nil {
		return "", backend.GPUPool{}, false, nil
	}
	pool := s.GPUPools[bestName]
	pool.NodeSelector = map[string]string{labelMachinePool: bestName}
	pool.Taint = s.GPUPool.Taint
	return bestName, pool, true, nil
}

// smallerShape orders sizes smallest first: by vCPU, then memory.
func smallerShape(a, b backend.InstanceShape) bool {
	if a.VCPU != b.VCPU {
		return a.VCPU < b.VCPU
	}
	return a.MemoryGiB < b.MemoryGiB
}

// cacheVerdict is the fit answer's Cached / CacheSource pair: what the cache
// says for a model that stores in it (isCached); for one served from an OCI
// model image, not cached — nothing of it is in the claim — from the
// oci-image source.
func (b *Backend) cacheVerdict(ctx context.Context, node string, plan *fitPlan, loc cacheLocation) (bool, string) {
	if !plan.Preset.storesInCache() {
		return false, backend.CacheSourceOCIImage
	}
	return b.isCached(ctx, node, plan.Dir, plan.Repo, loc)
}

// anyNodeMatches says whether one of the nodes carries every label of the
// selector.
func anyNodeMatches(nodes []nodeBudget, selector map[string]string) bool {
	for _, n := range nodes {
		if matchesSelector(n.Labels, selector) {
			return true
		}
	}
	return false
}

// weightsNeed is the "weights + overhead = required" clause of a fit verdict,
// naming where the weight size came from.
func weightsNeed(res *backend.FitResult) string {
	return fmt.Sprintf("%s weights (%s) + %s overhead = %s", humanBytes(res.WeightsBytes), res.WeightsSource, humanBytes(res.OverheadBytes), humanBytes(res.RequiredBytes))
}

func reservedNote(reserved int64) string {
	if reserved <= 0 {
		return ""
	}
	return ", " + humanBytes(reserved) + " reserved by running models"
}

// candidateNodes narrows the nodes a model may be served on: an explicit node
// — refused with its eligibility reason when it is not a serving target, so
// nothing gets scheduled onto a node that cannot run it — else the eligible
// nodes matching the preset's node selector, preferring the nodes that hold
// the cache. The nodes are the preset's serving capacity (nodes): the
// accelerator nodes, or every node for a CPU preset; eligibility judged.
func (b *Backend) candidateNodes(ctx context.Context, nodes []nodeBudget, explicit string, loc cacheLocation, p *servingPreset) ([]nodeBudget, string) {
	s := b.cfg.settings(ctx)
	if explicit != "" {
		for _, n := range nodes {
			if n.Name != explicit {
				continue
			}
			if !n.Eligible {
				return nil, fmt.Sprintf("node %s is not a serving target: %s", n.Name, n.EligibilityReason)
			}
			return []nodeBudget{n}, ""
		}
		if p.cpu() {
			return nil, fmt.Sprintf("node %q not found", explicit)
		}
		return nil, fmt.Sprintf("node %q not found: it does not exist or is not an accelerator node (no %s resource, no %s label)", explicit, s.GPUResourceName, labelGPUPresent)
	}
	var eligible []nodeBudget
	for _, n := range nodes {
		if n.Eligible && matchesSelector(n.Labels, presetSelector(p)) {
			eligible = append(eligible, n)
		}
	}
	if len(eligible) == 0 {
		if len(nodes) == 0 {
			if p.cpu() {
				return nil, "no node: the cluster reports none"
			}
			return nil, fmt.Sprintf("no accelerator node: no node advertises %s or carries the %s label", s.GPUResourceName, labelGPUPresent)
		}
		why := make([]string, 0, len(nodes))
		for _, n := range nodes {
			reason := n.EligibilityReason
			if reason == "" {
				reason = "outside the preset's node selector (" + formatSelector(presetSelector(p)) + ")"
			}
			why = append(why, n.Name+": "+reason)
		}
		return nil, "no eligible node: " + strings.Join(why, "; ")
	}
	if len(loc.Nodes) > 0 {
		var onCache []nodeBudget
		for _, n := range eligible {
			if containsString(loc.Nodes, n.Name) {
				onCache = append(onCache, n)
			}
		}
		if len(onCache) > 0 {
			return onCache, ""
		}
	}
	return eligible, ""
}

// presetSelector is the node selector of the preset's own scheduling; empty
// without a preset.
func presetSelector(p *servingPreset) map[string]string {
	if p == nil {
		return nil
	}
	return p.Spec.Scheduling.NodeSelector
}

// reservedByNode sums what the running LLMInferenceServices need per node, from
// their presets: the weights and overhead — a copy's in full on each of its
// nodes, a split's share on each of its — and on a unified-memory node — GPUs
// that report no memory of their own, the node's memory being theirs — at
// least the share vLLM claims at start, --gpu-memory-utilization of the
// node's budget, whatever the requests say. The preset being (re)loaded is not
// counted against itself: what it holds is own, per node it serves on.
func (b *Backend) reservedByNode(ctx context.Context, idx presetIndex, loading *servingPreset, nodes []nodeBudget) (reserved, own map[string]int64) {
	out, own := map[string]int64{}, map[string]int64{}
	byName := make(map[string]nodeBudget, len(nodes))
	for _, n := range nodes {
		byName[n.Name] = n
	}
	servedList, err := b.listServed(ctx)
	if err != nil {
		b.log.Warn("listing LLMInferenceServices for the fit check failed", "error", err)
		return out, own
	}
	for _, sv := range servedList {
		on := sv.onNodes()
		if len(on) == 0 || sv.Deleting {
			continue
		}
		parts := int64(1)
		if sv.Placement == backend.PlacementSplit {
			parts = int64(len(on))
		}
		if loading != nil && sv.Name == loading.name() {
			for _, n := range on {
				own[n] += presetShare(loading, byName[n], b.opts.DefaultOverheadGiB, parts, sv.Utilization)
			}
			continue
		}
		p, ok := idx.byName[sv.Preset]
		if !ok {
			if matches := idx.forModel(sv.Model); len(matches) == 1 {
				p = matches[0]
			}
		}
		if p == nil {
			continue
		}
		for _, n := range on {
			out[n] += presetShare(p, byName[n], b.opts.DefaultOverheadGiB, parts, sv.Utilization)
		}
	}
	return out, own
}

// presetReserve is what one served preset holds on its node: its weights and
// overhead, or on a unified-memory GPU node (GPUs, no GPU memory label) the
// share of the node's memory vLLM claims at start (vllmClaim), whichever is
// larger.
func presetReserve(p *servingPreset, n nodeBudget, defaultOverheadGiB float64) int64 {
	return presetShare(p, n, defaultOverheadGiB, 1, 0)
}

// presetShare is presetReserve for one of parts nodes a split spreads the
// weights over: its share of the weights beside the whole overhead; on a
// unified-memory node the claim is at utilization, the one the object was
// composed with (served.Utilization), when it is known.
func presetShare(p *servingPreset, n nodeBudget, defaultOverheadGiB float64, parts int64, utilization float64) int64 {
	need := ceilDiv(p.weightsBytes(), max(parts, 1)) + p.overheadBytes(defaultOverheadGiB)
	if claim, memory, ok := vllmClaim(p, n); ok {
		if utilization > 0 {
			claim = int64(utilization * float64(memory))
		}
		need = max(need, claim)
	}
	return need
}

// vllmClaim is what vLLM claims at start on a unified-memory GPU node — GPUs
// that report no memory of their own, the node's memory being theirs (a
// GB10) — and of what: --gpu-memory-utilization of the memory
// torch.cuda.mem_get_info reports there, the node's capacity (its budget
// when it reports none). The claim is outside the pod's memory limit and the
// kubelet's accounting: nothing on the Kubernetes side refuses or evicts it.
// ok is false on a node whose GPUs have memory of their own, on a CPU-only
// node, for a CPU preset and without a preset (a bare model reference is
// sized, never served: load_model needs a preset).
func vllmClaim(p *servingPreset, n nodeBudget) (claim, memory int64, ok bool) {
	if p == nil || p.cpu() || n.GPUCount == 0 || n.GPUMemory > 0 {
		return 0, 0, false
	}
	memory = n.Capacity
	if memory <= 0 {
		memory = n.Budget
	}
	if memory <= 0 {
		return 0, 0, false
	}
	return int64(p.utilization() * float64(memory)), memory, true
}

// unifiedVerdict is the check of vLLM's claim on a unified-memory node: the
// claim (vllmClaim) must fit what the node leaves models — its budget, at
// most its memory less the host headroom — less what running models
// reserve there, and hold the weights and overhead it serves (Need). On a
// refusal Fit is the --gpu-memory-utilization that would fit and hold them:
// the highest for a claim the node cannot leave, the lowest for one short of
// the need (0: none).
type unifiedVerdict struct {
	Checked     bool
	Fits        bool
	Utilization float64
	// Preset is the preset's own utilization when Utilization was sized
	// down from it (sized); 0 otherwise.
	Preset    float64
	Claim     int64
	Memory    int64
	Headroom  int64
	Available int64
	Reserved  int64
	Fit       float64
	// Need is the weights and overhead the claim must hold, and Holds the
	// lowest utilization whose claim does (0: none of the node's memory).
	Need  int64
	Holds float64
}

// short reports a claim that fits the node but cannot hold the need.
func (v unifiedVerdict) short() bool {
	return v.Checked && v.Claim <= v.Available && v.Claim < v.Need
}

// unifiedClaimOn judges the preset's claim on node n, beside reserved bytes
// of running models, for a model that needs need bytes (weights and
// overhead).
func (b *Backend) unifiedClaimOn(p *servingPreset, n nodeBudget, reserved, need int64) unifiedVerdict {
	claim, memory, ok := vllmClaim(p, n)
	if !ok {
		return unifiedVerdict{}
	}
	headroom := gibToBytes(b.opts.UnifiedHostHeadroomGiB)
	v := unifiedVerdict{Checked: true, Utilization: p.utilization(), Claim: claim, Memory: memory, Headroom: headroom, Reserved: reserved, Need: need}
	v.Available = max(min(n.Budget, memory-headroom)-reserved, 0)
	v.Fits = claim <= v.Available && claim >= need
	switch {
	case claim > v.Available:
		if u := math.Floor(float64(v.Available)/float64(memory)*100) / 100; u > 0 && int64(u*float64(memory)) >= need {
			v.Fit = u
		}
	case claim < need:
		if u := math.Ceil(float64(need)/float64(memory)*100) / 100; u <= 1 {
			v.Holds = u
			if int64(u*float64(memory)) <= v.Available {
				v.Fit = u
			}
		}
	}
	return v
}

// clause words the verdict for the fit's reason.
func (v unifiedVerdict) clause() string {
	sizedFrom := ""
	if v.Preset > 0 {
		sizedFrom = ", sized down from the preset's " + trimFloat2(v.Preset) + ","
	}
	claim := fmt.Sprintf("vLLM claims %s at start (%s=%s%s of the node's %s unified memory)",
		humanBytes(v.Claim), flagGPUMemoryUtilization, trimFloat2(v.Utilization), sizedFrom, humanBytes(v.Memory))
	left := fmt.Sprintf("the %s the node leaves models beside its %s host headroom%s", humanBytes(v.Available), humanBytes(v.Headroom), reservedNote(v.Reserved))
	if v.Fits {
		return claim + ", within " + left
	}
	if v.short() {
		out := claim + ", less than the " + humanBytes(v.Need) + " of weights and overhead it must hold"
		switch {
		case v.Fit > 0:
			return out + fmt.Sprintf(": %s=%s holds them", flagGPUMemoryUtilization, trimFloat2(v.Fit))
		case v.Holds > 0:
			return out + fmt.Sprintf(": %s=%s would, more than %s", flagGPUMemoryUtilization, trimFloat2(v.Holds), left)
		}
		return out + ", more than the node's memory"
	}
	out := claim + ", more than " + left
	if v.Fit > 0 {
		return out + fmt.Sprintf(": %s=%s fits", flagGPUMemoryUtilization, trimFloat2(v.Fit))
	}
	return out
}

// applyUnified writes the verdict into a fit answer: the figures, and the
// clause of the reason — a refusal when the weights fit but the claim does
// not.
func applyUnified(res *backend.FitResult, v unifiedVerdict) {
	if !v.Checked {
		return
	}
	res.UnifiedReservationBytes, res.HostHeadroomBytes = v.Claim, v.Headroom
	if !v.Fits {
		res.FitGPUMemoryUtilization = v.Fit
	}
	if res.Fits && !v.Fits {
		res.Fits = false
		res.Reason += ", but " + v.clause()
		return
	}
	res.Reason += "; " + v.clause()
}

// isCached reports whether the model is already in the cache and how that
// was decided (backend.CacheSource*): what the scan says, and — while no scan
// can answer in this call (the pool at zero, the caller's deadline;
// cacheSnapshotFor) — what the cache index remembers: a directory an
// LLMInferenceService filled for the repository, in this claim and volume, and
// model-manager has not removed (index.go; a record bound to another cache,
// or to none, is no verdict). Neither answering is "unknown", not "no". Without a
// node the cache is asked as a whole (a pinned claim: its node; a shared
// one: any).
func (b *Backend) isCached(ctx context.Context, node, dir, repo string, loc cacheLocation) (bool, string) {
	if loc.Missing || (!loc.Bound && len(loc.Nodes) == 0) {
		return false, backend.CacheSourceUnknown
	}
	scanNode := node
	if loc.Shared {
		scanNode = ""
	} else if scanNode == "" && len(loc.Nodes) > 0 {
		scanNode = loc.Nodes[0]
	}
	snap := b.cacheSnapshotFor(ctx, scanNode, loc)
	for _, e := range snap.Entries {
		if e.Dir == dir && e.Files > 0 {
			return true, backend.CacheSourceScan
		}
		if e.Marker != nil && strings.EqualFold(e.Marker.Model, repo) && e.Files > 0 {
			return true, backend.CacheSourceScan
		}
	}
	if snap.Pending && len(snap.Entries) == 0 {
		if rec, ok := b.readIndex(ctx)[dir]; ok && strings.EqualFold(rec.Model, repo) && rec.boundTo(loc) {
			return true, backend.CacheSourceIndex
		}
		return false, backend.CacheSourceUnknown
	}
	return false, backend.CacheSourceScan
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// joinNotes joins the notes a fit answer carries, skipping empty ones.
func joinNotes(notes ...string) string {
	var out []string
	for _, n := range notes {
		if n != "" {
			out = append(out, n)
		}
	}
	return strings.Join(out, "; ")
}
