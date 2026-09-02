package kserve

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/giantswarm/model-manager/internal/backend"
)

const (
	weightsSourceIndex  = "safetensors-index"
	weightsSourceTree   = "tree"
	weightsSourcePreset = "preset"
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
	model := strings.TrimSpace(req.Model)
	if model == "" && req.Preset == "" {
		return nil, fmt.Errorf("%w: model or preset is required", backend.ErrInvalid)
	}
	repo, revision := splitRevision(model)

	presets, _, err := b.presets(ctx)
	if err != nil {
		return nil, err
	}
	idx := indexPresets(presets)
	p, err := idx.resolve(repo, req.Preset)
	if err != nil {
		return nil, err
	}
	if p != nil && (repo == "" || repo == p.name()) {
		repo = p.Spec.Model.ID
	}
	if !isRepoID(repo) {
		return nil, fmt.Errorf("%w: %q is neither a Hugging Face repository (owner/name) nor a preset name", backend.ErrInvalid, model)
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

	// Size from the hub; the preset is the fallback when the hub cannot tell.
	hub, err := b.hub.Model(ctx, repo)
	switch {
	case err == nil:
		plan.Hub = hub
		res.Gated = hub.isGated()
		res.Private = hub.Private
		files, err := b.hub.Tree(ctx, repo, revision)
		if err != nil {
			return nil, err
		}
		plan.Files = files
		res.DownloadBytes = downloadTotal(files, b.opts.DownloadIgnorePatterns)
		total, err := b.hub.SafetensorsTotal(ctx, repo, revision, files)
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
		// Gated without token, hub down: the preset's numbers still allow a fit check.
		b.log.Warn("hub lookup failed; using the preset's requirements", "model", repo, "error", err)
		res.WeightsBytes, res.WeightsSource = p.weightsBytes(), weightsSourcePreset
		res.Gated = errors.Is(err, backend.ErrInvalid)
	default:
		return nil, err
	}
	if res.WeightsBytes <= 0 {
		return nil, fmt.Errorf("%w: cannot determine the weight size of %s (no safetensors index, no weight files, no preset)", backend.ErrInvalid, repo)
	}
	if p != nil {
		res.OverheadBytes = p.overheadBytes(b.opts.DefaultOverheadGiB)
	} else {
		res.OverheadBytes = gibToBytes(b.opts.DefaultOverheadGiB)
	}
	res.RequiredBytes = res.WeightsBytes + res.OverheadBytes

	// Target node.
	loc, err := b.cacheNodes(ctx)
	if err != nil {
		return nil, err
	}
	plan.CacheLocal = len(loc.Nodes) > 0
	nodes, err := b.nodes(ctx)
	if err != nil {
		return nil, err
	}
	reserved := map[string]int64{}
	if forServe {
		reserved = b.reservedByNode(ctx, idx, p)
	}
	candidates, why := b.candidateNodes(ctx, nodes, req.Node, loc, p)
	if len(candidates) == 0 {
		res.Fits = false
		res.Reason = why
		return plan, nil
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
		return plan, nil
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
	res.Cached = b.isCached(ctx, best.Name, plan.Dir, repo, loc)
	return plan, nil
}

func reservedNote(reserved int64) string {
	if reserved <= 0 {
		return ""
	}
	return ", " + humanBytes(reserved) + " reserved by running models"
}

// candidateNodes narrows the nodes a model may be served on: an explicit node,
// else the ready nodes matching the discovery and preset node selectors,
// preferring the nodes that hold the cache.
func (b *Backend) candidateNodes(ctx context.Context, nodes []nodeBudget, explicit string, loc cacheLocation, p *servingPreset) ([]nodeBudget, string) {
	if explicit != "" {
		for _, n := range nodes {
			if n.Name == explicit {
				return []nodeBudget{n}, ""
			}
		}
		return nil, fmt.Sprintf("node %q not found", explicit)
	}
	s := b.cfg.settings(ctx)
	selector := map[string]string{}
	for k, v := range s.NodeSelector {
		selector[k] = v
	}
	if p != nil {
		for k, v := range p.Spec.Scheduling.NodeSelector {
			selector[k] = v
		}
	}
	var eligible []nodeBudget
	for _, n := range nodes {
		if n.Ready && matchesSelector(n.Labels, selector) {
			eligible = append(eligible, n)
		}
	}
	if len(eligible) == 0 {
		if len(selector) > 0 {
			return nil, fmt.Sprintf("no ready node matches the node selector %v", selector)
		}
		return nil, "no ready node"
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
