package kserve

import (
	"context"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/giantswarm/model-manager/internal/backend"
)

// deadlineReserve is what a call keeps of its context deadline for the
// Kubernetes side after a hub lookup — placement, composing and creating the
// serving object, reading its state. The hub gets the rest, at most
// opts.HFTimeout (giantswarm/model-manager#104).
const deadlineReserve = 2 * time.Second

// scanVerdict says whether a cache scan may create a pod now, and if not why.
type scanVerdict struct {
	Allowed bool
	Reason  string
}

// scanAllowed decides whether a scan pod may be created for the cache at loc.
// Never while the claim is unbound: a WaitForFirstConsumer claim binds to the
// first pod that mounts it, and on a scale-to-zero GPU pool that pod launched
// the GPU node without a predictor ever running (giantswarm/model-manager#104)
// — the predictor is the claim's first consumer. A shared claim (a volume
// without node affinity) scans only while a node of the GPU pool exists; else
// the pod's pool toleration and selector make the autoscaler launch one. A
// claim pinned to nodes scans on them: a pinned pod bypasses the scheduler and
// the autoscaler alike. Without a GPU pool nothing here can launch a node.
func (b *Backend) scanAllowed(ctx context.Context, loc cacheLocation) scanVerdict {
	switch {
	case loc.Missing:
		return scanVerdict{Reason: fmt.Sprintf("cache claim %s does not exist", loc.Claim)}
	case !loc.Bound:
		return scanVerdict{Reason: fmt.Sprintf("cache claim %s is not bound yet; the first predictor binds it", loc.Claim)}
	case loc.pinned():
		return scanVerdict{Allowed: true}
	}
	selector := b.cfg.settings(ctx).GPUPool.NodeSelector
	if len(selector) == 0 {
		return scanVerdict{Allowed: true}
	}
	exists, err := b.poolNodeExists(ctx, selector)
	switch {
	case err != nil:
		return scanVerdict{Reason: "cannot tell whether a node of the GPU pool exists: " + err.Error()}
	case !exists:
		return scanVerdict{Reason: fmt.Sprintf("no node of the GPU pool (%s) exists; a predictor brings the first", formatSelector(selector))}
	}
	return scanVerdict{Allowed: true}
}

// poolNodeExists reports whether a node carries every label of the pool
// selector.
func (b *Backend) poolNodeExists(ctx context.Context, selector map[string]string) (bool, error) {
	list, err := b.k8s(ctx).CoreV1().Nodes().List(ctx, metav1.ListOptions{LabelSelector: labels.SelectorFromSet(selector).String(), Limit: 1})
	if err != nil {
		return false, fmt.Errorf("list nodes: %w", err)
	}
	return len(list.Items) > 0, nil
}

// cacheSnapshotFor is the scan of node a call is answered with: the one
// younger than opts.InventoryTTL when there is one; else a new scan when
// scanAllowed permits a pod and the caller's deadline leaves room for one
// (opts.InventoryTimeout is a scan's budget; a caller without a deadline
// waits); else the last scan whatever its age — or nothing — marked Pending,
// with the scan refreshing it in the background when it is permitted. A
// muster meta-tool call has about ten seconds: it never waits for a pod
// (giantswarm/model-manager#104).
func (b *Backend) cacheSnapshotFor(ctx context.Context, node string, loc cacheLocation) *cacheSnapshot {
	if snap := b.inv.fresh(node, b.opts.InventoryTTL); snap != nil {
		return snap
	}
	verdict := b.scanAllowed(ctx, loc)
	if verdict.Allowed && deadlineAllows(ctx, b.opts.InventoryTimeout) {
		return b.inv.snapshot(ctx, node, b.opts.InventoryTTL, false, b.scan)
	}
	if verdict.Allowed {
		verdict.Reason = fmt.Sprintf("the caller's deadline leaves less than %s for a scan pod; scanning in the background", b.opts.InventoryTimeout)
		if skip := b.callerless(ctx, "background cache scan"); skip != "" {
			verdict.Reason = skip
		} else {
			b.inv.refresh(ctx, node, b.opts.InventoryTTL, b.opts.InventoryTimeout, b.scan)
		}
	}
	snap := b.inv.last(node)
	snap.Pending, snap.PendingReason = true, verdict.Reason
	return snap
}

// refreshInventory drops the cached scans after a call changed what the
// cache holds or serves and, where scanAllowed permits a scan pod, rescans
// the cache in the background so the next read is answered from the cache
// as it is now; the answer says whether a scan runs and, when none can, why.
// The call never waits for it: the scan is the inventory after the change,
// not the change (giantswarm/model-manager#119).
func (b *Backend) refreshInventory(ctx context.Context) backend.InventoryRefresh {
	b.inv.invalidate()
	if skip := b.callerless(ctx, "background cache scan"); skip != "" {
		return backend.InventoryRefresh{Reason: skip}
	}
	if !b.cfg.settings(ctx).CacheEnabled {
		return backend.InventoryRefresh{Reason: "no cache claim is configured; there is no inventory to rescan"}
	}
	loc, err := b.cacheNodes(ctx)
	if err != nil {
		return backend.InventoryRefresh{Reason: "cannot locate the cache claim: " + err.Error()}
	}
	if verdict := b.scanAllowed(ctx, loc); !verdict.Allowed {
		return backend.InventoryRefresh{Reason: verdict.Reason}
	}
	nodes := loc.Nodes
	if len(nodes) == 0 {
		nodes = []string{""}
	}
	for _, node := range nodes {
		b.inv.refresh(ctx, node, b.opts.InventoryTTL, b.opts.InventoryTimeout, b.scan)
	}
	return backend.InventoryRefresh{Refreshing: true}
}

// deadlineAllows reports whether ctx leaves at least budget before its
// deadline; always without one.
func deadlineAllows(ctx context.Context, budget time.Duration) bool {
	dl, ok := ctx.Deadline()
	return !ok || time.Until(dl) >= budget
}
