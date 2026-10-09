package kserve

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/giantswarm/model-manager/internal/backend"
)

// Node claims (giantswarm/model-manager#294): a cache claim pinned to one
// node (a local volume) holds the Hugging Face downloads there only, so a
// model split across a fast link has no weights on the other nodes. Where the
// platform's redirect policy mounts the claim a model pod names
// (settings.CacheNodeClaims), every node outside the claim's reach gets a
// cache claim of its own, <claim>-<node>, of the claim's class, access modes
// and size: pull_model with a node creates it and its download Job fills it,
// a split's leader and worker pods name the claim of their node
// (CacheClaimAnnotation, WorkerCacheClaimAnnotation), and the claim of each
// node is scanned on its own. The redirect policy reads the annotations;
// without them a pod mounts the cache claim itself.

const (
	// CacheClaimAnnotation and WorkerCacheClaimAnnotation name, on an
	// LLMInferenceService's pods (spec.annotations), the cache claim the
	// redirect policy mounts into the leader (a single-node model's pod) and
	// into a split's workers.
	CacheClaimAnnotation       = "model-manager.giantswarm.io/cache-claim"
	WorkerCacheClaimAnnotation = "model-manager.giantswarm.io/worker-cache-claim"
	// CacheNodeAnnotation names the node a node claim holds the cache of.
	CacheNodeAnnotation = "model-manager.giantswarm.io/cache-node"
	// nodeCacheComponent is the component label of a node claim.
	nodeCacheComponent = "node-cache"
	// selectedNodeAnnotation is the scheduler's choice of node on a claim
	// that binds with its first consumer: the provisioner creates the volume
	// there. A download Job's pod names its node and bypasses the scheduler,
	// so a node claim carries it from the start.
	selectedNodeAnnotation = "volume.kubernetes.io/selected-node"
)

// perNode reports whether every node outside the pinned claim's reach gets a
// cache claim of its own.
func (l cacheLocation) perNode() bool {
	return l.NodeClaims && l.pinned()
}

// claimOn names the cache claim a pod on node mounts: the cache claim itself
// on a node that mounts it, on any node of a shared or unpinned claim, and
// without a node; the node's own claim (nodeClaimName) otherwise.
func (l cacheLocation) claimOn(node string) string {
	if node == "" || !l.perNode() || containsString(l.Nodes, node) {
		return l.Claim
	}
	return nodeClaimName(l.Claim, node)
}

// hasCacheOn reports whether a cache exists that a pod on node mounts.
func (l cacheLocation) hasCacheOn(node string) bool {
	if l.claimOn(node) == l.Claim {
		return true
	}
	_, ok := l.NodeCaches[node]
	return ok
}

// cacheNodeNames are the nodes that hold a cache: the claim's and the nodes
// with a claim of their own (by name), in that order.
func (l cacheLocation) cacheNodeNames() []string {
	out := append([]string(nil), l.Nodes...)
	for _, node := range slices.Sorted(maps.Keys(l.NodeCaches)) {
		if !containsString(out, node) {
			out = append(out, node)
		}
	}
	return out
}

// claimOn names the cache claim a cache pod on node mounts, the cache
// located afresh.
func (b *Backend) claimOn(ctx context.Context, node string) (string, error) {
	loc, err := b.claimLocation(ctx, b.cfg.settings(ctx))
	if err != nil {
		return "", err
	}
	return loc.claimOn(node), nil
}

// nodeClaimName is the cache claim of a node: <claim>-<node>, within the
// label limit.
func nodeClaimName(claim, node string) string {
	return truncateLabel(claim+"-"+node, claim+"-"+node)
}

// nodeCaches lists the node claims that exist, by node: model-manager's
// claims of the node-cache component whose name is the node claim of the
// cache claim.
func (b *Backend) nodeCaches(ctx context.Context, s settings) (map[string]string, error) {
	sel := labels.SelectorFromSet(b.labels(map[string]string{ComponentLabel: nodeCacheComponent})).String()
	list, err := b.k8s(ctx).CoreV1().PersistentVolumeClaims(s.Namespace).List(ctx, metav1.ListOptions{LabelSelector: sel})
	if err != nil {
		return nil, fmt.Errorf("list node cache claims in %s: %w", s.Namespace, err)
	}
	out := map[string]string{}
	for _, pvc := range list.Items {
		node := pvc.Annotations[CacheNodeAnnotation]
		if node != "" && pvc.DeletionTimestamp == nil && pvc.Name == nodeClaimName(s.CacheClaim, node) {
			out[node] = pvc.Name
		}
	}
	return out, nil
}

// ensureNodeClaim returns the cache claim a download on node fills, creating
// the node's own claim when it has none: the cache claim's class, access
// modes, volume mode and size, its volume provisioned on node. A cache claim
// bound statically (no StorageClass) cannot be copied; the refusal names the
// claim to create by hand.
func (b *Backend) ensureNodeClaim(ctx context.Context, loc cacheLocation, node string) (string, error) {
	name := loc.claimOn(node)
	if name == loc.Claim || loc.hasCacheOn(node) {
		return name, nil
	}
	s := b.cfg.settings(ctx)
	pvcs := b.k8s(ctx).CoreV1().PersistentVolumeClaims(s.Namespace)
	base, err := pvcs.Get(ctx, loc.Claim, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("get cache claim %s/%s: %w", s.Namespace, loc.Claim, err)
	}
	class := base.Spec.StorageClassName
	if class == nil || strings.TrimSpace(*class) == "" {
		return "", fmt.Errorf("%w: cache claim %s is pinned to %s and binds statically (no StorageClass), so model-manager cannot provision a cache on node %s: create claim %s in %s, bound to a volume on %s, labelled %s=%s and %s=%s and annotated %s=%s",
			backend.ErrInvalid, loc.Claim, strings.Join(loc.Nodes, ", "), node, name, s.Namespace, node, ManagedByLabel, ManagedByValue, ComponentLabel, nodeCacheComponent, CacheNodeAnnotation, node)
	}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   s.Namespace,
			Labels:      b.labels(map[string]string{ComponentLabel: nodeCacheComponent}),
			Annotations: map[string]string{CacheNodeAnnotation: node, selectedNodeAnnotation: node},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      base.Spec.AccessModes,
			StorageClassName: class,
			VolumeMode:       base.Spec.VolumeMode,
			Resources:        corev1.VolumeResourceRequirements{Requests: base.Spec.Resources.Requests},
		},
	}
	if _, err := pvcs.Create(ctx, pvc, metav1.CreateOptions{FieldManager: ManagedByValue}); err != nil && !errors.IsAlreadyExists(err) {
		return "", fmt.Errorf("create cache claim %s/%s for node %s: %w", s.Namespace, name, node, err)
	}
	b.log.Info("node cache claim created", "claim", name, "node", node, "storageClass", *class)
	return name, nil
}

// cacheClaimAnnotations names, on a model's pods, the cache claims of the
// nodes it is placed on when one of them is a node claim: the leader's (a
// single-node model's) of nodes[0], a split's workers' of the rest. nil when
// every pod mounts the cache claim itself.
func cacheClaimAnnotations(loc cacheLocation, nodes []string) map[string]string {
	if !loc.perNode() || len(nodes) == 0 {
		return nil
	}
	out := map[string]string{}
	if claim := loc.claimOn(nodes[0]); claim != loc.Claim {
		out[CacheClaimAnnotation] = claim
	}
	if len(nodes) > 1 {
		if claim := loc.claimOn(nodes[1]); claim != loc.Claim {
			out[WorkerCacheClaimAnnotation] = claim
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// sameClaim reports whether pods on every one of nodes mount one cache
// claim: copies share one pod template, a split's workers another, and a
// template names one claim.
func sameClaim(loc cacheLocation, nodes []string) bool {
	for _, n := range nodes {
		if loc.claimOn(n) != loc.claimOn(nodes[0]) {
			return false
		}
	}
	return true
}

// claimsOf lists the cache claim each node's pods mount, "node: claim".
func claimsOf(loc cacheLocation, nodes []string) string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n+": "+loc.claimOn(n))
	}
	return strings.Join(out, ", ")
}

// setPodAnnotations adds annotations to every pod of an LLMInferenceService
// (spec.annotations), beside the ones it carries.
func setPodAnnotations(obj *unstructured.Unstructured, annotations map[string]string) {
	if len(annotations) == 0 {
		return
	}
	spec, _ := obj.Object["spec"].(map[string]any)
	if spec == nil {
		spec = map[string]any{}
		obj.Object["spec"] = spec
	}
	ann, _ := spec["annotations"].(map[string]any)
	if ann == nil {
		ann = map[string]any{}
		spec["annotations"] = ann
	}
	for k, v := range annotations {
		ann[k] = v
	}
}
