package kserve

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/giantswarm/model-manager/internal/backend"
)

const (
	weightsSourceIndex  = "safetensors-index"
	weightsSourceTree   = "tree"
	weightsSourcePreset = "preset"
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
}

// fitCheck sizes a model (hub, falling back to the preset) and compares
// weights + overhead with the budget of the target node. forServe subtracts
// what the node's running InferenceServices already need; a pull only asks
// whether the model can ever be served there.
func (b *Backend) fitCheck(ctx context.Context, req backend.FitRequest, forServe bool) (*fitPlan, error) {
	plan, idx, err := b.resolveFit(ctx, req)
	if err != nil {
		return nil, err
	}
	note, err := b.sizeModel(ctx, plan)
	if err != nil {
		return nil, err
	}
	if err := b.placeModel(ctx, plan, idx, req, forServe); err != nil {
		return nil, err
	}
	if note != "" {
		plan.Result.Reason += "; " + note
	}
	return plan, nil
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
// the safetensors index, else the file tree — within the hub lookup timeout,
// and from the preset's requirements when the hub cannot tell (gated without
// a token, unreachable, not answering in time). It returns the note the
// answer carries when the preset stood in for the hub.
func (b *Backend) sizeModel(ctx context.Context, plan *fitPlan) (string, error) {
	res, p, repo := &plan.Result, plan.Preset, plan.Repo
	// Bounded on the caller's context: a hub whose packets an egress policy
	// drops must leave the fallback time to answer within the caller's
	// meta-tool deadline (giantswarm/model-manager#88).
	hctx, cancel := b.hubContext(ctx)
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
			return "", hubFailure(err, b.opts.HFTimeout)
		}
		plan.Files = files
		res.DownloadBytes = downloadTotal(files, b.opts.DownloadIgnorePatterns)
		total, err := b.hub.SafetensorsTotal(hctx, repo, plan.Revision, files)
		if err != nil {
			b.log.Warn("reading the safetensors index failed; summing the tree instead", "model", repo, "error", err)
		}
		switch {
		case total > 0:
			res.WeightsBytes, res.WeightsSource = total, weightsSourceIndex
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
		note = "weights from the preset's requirements: " + describeHubFailure(err, b.opts.HFTimeout)
	default:
		return "", hubFailure(err, b.opts.HFTimeout)
	}
	if res.WeightsBytes <= 0 {
		return "", fmt.Errorf("%w: cannot determine the weight size of %s (no safetensors index, no weight files, no preset)", backend.ErrInvalid, repo)
	}
	if p != nil {
		res.OverheadBytes = p.overheadBytes(b.opts.DefaultOverheadGiB)
	} else {
		res.OverheadBytes = gibToBytes(b.opts.DefaultOverheadGiB)
	}
	res.RequiredBytes = res.WeightsBytes + res.OverheadBytes
	return note, nil
}

// hubContext bounds the hub lookups of one call (fit check, search) by
// opts.HFTimeout.
func (b *Backend) hubContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, b.opts.HFTimeout)
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
// without a node.
func (b *Backend) placeModel(ctx context.Context, plan *fitPlan, idx presetIndex, req backend.FitRequest, forServe bool) error {
	res, p := &plan.Result, plan.Preset
	loc, err := b.cacheNodes(ctx)
	if err != nil {
		return err
	}
	plan.CacheLocal = len(loc.Nodes) > 0
	nodes, err := b.nodes(ctx, loc)
	if err != nil {
		return err
	}
	reserved := map[string]int64{}
	if forServe {
		reserved = b.reservedByNode(ctx, idx, p)
	}
	candidates, why := b.candidateNodes(ctx, nodes, req.Node, loc, p)
	if len(candidates) == 0 {
		// A GPU pool at scale-to-zero (giantswarm/model-manager#90): the pool
		// selector names a pool no node belongs to yet. Serving is what
		// brings the node — the predictor carries the pool's toleration and
		// selector, goes Pending, and the autoscaler launches it — so the
		// answer is given without a node: judged against the pool's instance
		// shapes when they are known (giantswarm/model-manager#97), else yes
		// and unverified. An explicit node, a pool with nodes that do not
		// fit, or no pool at all keep the refusal.
		if pool := b.cfg.settings(ctx).GPUPool; req.Node == "" && len(pool.NodeSelector) > 0 && !anyNodeMatches(nodes, pool.NodeSelector) {
			return b.placeOnPool(plan, pool)
		}
		res.Fits = false
		res.Reason = why
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
		res.Reason = fmt.Sprintf("%s weights + %s overhead = %s fit within %s on %s (%s%s)",
			humanBytes(res.WeightsBytes), humanBytes(res.OverheadBytes), humanBytes(res.RequiredBytes), humanBytes(limit), best.Name, best.BudgetSource, reservedNote(res.ReservedBytes))
	} else {
		res.Reason = fmt.Sprintf("%s weights + %s overhead = %s exceed the %s available on %s (%s budget %s%s)",
			humanBytes(res.WeightsBytes), humanBytes(res.OverheadBytes), humanBytes(res.RequiredBytes), humanBytes(limit), best.Name, best.BudgetSource, humanBytes(res.BudgetBytes), reservedNote(res.ReservedBytes))
	}
	if res.Gated && !res.TokenConfigured {
		res.Reason += "; the repository is gated and no hub token is configured"
	}
	res.Cached = b.isCached(ctx, best.Name, plan.Dir, plan.Repo, loc)
	return nil
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
// the cache. The nodes are the accelerator nodes (nodes), eligibility judged.
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
		return nil, fmt.Sprintf("node %q not found: it does not exist or is not an accelerator node (no %s resource, no %s label)", explicit, s.GPUResourceName, labelGPUPresent)
	}
	presetSelector := map[string]string{}
	if p != nil {
		presetSelector = p.Spec.Scheduling.NodeSelector
	}
	var eligible []nodeBudget
	for _, n := range nodes {
		if n.Eligible && matchesSelector(n.Labels, presetSelector) {
			eligible = append(eligible, n)
		}
	}
	if len(eligible) == 0 {
		if len(nodes) == 0 {
			return nil, fmt.Sprintf("no accelerator node: no node advertises %s or carries the %s label", s.GPUResourceName, labelGPUPresent)
		}
		why := make([]string, 0, len(nodes))
		for _, n := range nodes {
			reason := n.EligibilityReason
			if reason == "" {
				reason = "outside the preset's node selector (" + formatSelector(presetSelector) + ")"
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

// reservedByNode sums what the running InferenceServices need per node, from
// their presets. The preset being (re)loaded is not counted against itself.
func (b *Backend) reservedByNode(ctx context.Context, idx presetIndex, loading *servingPreset) map[string]int64 {
	out := map[string]int64{}
	servedList, err := b.listServed(ctx)
	if err != nil {
		b.log.Warn("listing InferenceServices for the fit check failed", "error", err)
		return out
	}
	for _, sv := range servedList {
		if sv.Node == "" || sv.Deleting {
			continue
		}
		if loading != nil && sv.Name == loading.name() {
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
		out[sv.Node] += p.weightsBytes() + p.overheadBytes(b.opts.DefaultOverheadGiB)
	}
	return out
}

// isCached reports whether the model is already in the node's cache.
func (b *Backend) isCached(ctx context.Context, node, dir, repo string, loc cacheLocation) bool {
	if loc.Missing || (!loc.Bound && len(loc.Nodes) == 0) {
		return false
	}
	scanNode := node
	if loc.Shared {
		scanNode = ""
	}
	snap := b.inv.snapshot(ctx, scanNode, b.opts.InventoryTTL, false, b.scan)
	for _, e := range snap.Entries {
		if e.Dir == dir && e.Files > 0 {
			return true
		}
		if e.Marker != nil && strings.EqualFold(e.Marker.Model, repo) && e.Files > 0 {
			return true
		}
	}
	return false
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
