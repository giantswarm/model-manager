package kserve

import (
	"context"
	"fmt"

	"github.com/giantswarm/model-manager/internal/backend"
	"github.com/giantswarm/model-manager/internal/identity"
)

// A remote target — a kserve backend whose document names another cluster's
// apiserver — is reached with the caller's token only: model-manager holds no
// credential there. What the driver does detached from a call (a background
// rescan of the cache) therefore runs as the caller who caused it, with the
// call's values and without its cancellation, and is skipped when there is no
// caller, never made anonymously.

// errDaemonSetRemote refuses the daemonset inventory for a remote target: it
// dials the cache agents' pod IPs, which the installation cannot reach on
// another cluster; the pod mode reads its scan through the target apiserver.
func errDaemonSetRemote(t backend.Target) error {
	return fmt.Errorf("kserve target %s: inventory mode %s dials the cache agents' pod IPs, which are not reachable on another cluster; use %s (a scan pod per node, read through the target apiserver)", t.Cluster, InventoryModeDaemonSet, InventoryModePod)
}

// callerless is why detached work for ctx is skipped — the target is remote
// and ctx carries no caller token — or "" when it may run. The first skip is
// logged with the reason; the answer of the call that asked says it each time.
func (b *Backend) callerless(ctx context.Context, work string) string {
	if b.opts.Target.Local() {
		return ""
	}
	if _, ok := identity.TokenFromContext(ctx); ok {
		return ""
	}
	reason := fmt.Sprintf("no %s on the remote target %s without a caller: model-manager holds no credential there, and only a request carrying the caller's token may act on it", work, b.opts.Target.Cluster)
	b.callerlessLog.Do(func() { b.log.Warn("detached work skipped", "reason", reason) })
	return reason
}
