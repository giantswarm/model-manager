package gitops

import (
	"context"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"

	"github.com/giantswarm/model-manager/internal/backend"
)

// Applying is the Flux object that applies obj now, nil when none does. The
// labels name it (OwnerOf); a Kustomization is held to its inventory
// (status.inventory, what it applied last): an object the Kustomization no
// longer lists — its file removed from git while the Kustomization does not
// prune (spec.prune false), which leaves the object on the cluster with its
// labels — is applied by nothing, and nothing reverts a live change to it. So
// is one whose Kustomization is gone. A Kustomization that has recorded no
// inventory yet has not said what it applies: the labels stand. A HelmRelease
// renders obj for as long as the labels say. dyn reads the Kustomization on
// the cluster obj lives on.
func Applying(ctx context.Context, dyn dynamic.Interface, obj *unstructured.Unstructured) (*Owner, error) {
	owner := OwnerOf(obj.GetLabels())
	if owner == nil || owner.Kind != KindKustomization {
		return owner, nil
	}
	ks, err := labelling(ctx, dyn, owner, obj)
	if ks == nil || err != nil {
		return nil, err
	}
	if lists, recorded := inventoried(ks, obj); !recorded || lists {
		return owner, nil
	}
	return nil, nil
}

// Kept is the Kustomization that left obj behind: its labels name it, it does
// not prune (spec.prune false) and its recorded inventory no longer lists obj.
// The file was removed from git, and Flux will not delete obj: only a live
// removal takes it away. nil for any other object, one git still declares
// included. dyn reads the Kustomization, on the cluster Flux runs on.
func Kept(ctx context.Context, dyn dynamic.Interface, obj *unstructured.Unstructured) (*Owner, error) {
	owner := OwnerOf(obj.GetLabels())
	if owner == nil || owner.Kind != KindKustomization {
		return nil, nil
	}
	ks, err := labelling(ctx, dyn, owner, obj)
	if ks == nil || err != nil {
		return nil, err
	}
	if prune, _, _ := unstructured.NestedBool(ks.Object, "spec", "prune"); prune {
		return nil, nil
	}
	if lists, recorded := inventoried(ks, obj); !recorded || lists {
		return nil, nil
	}
	return owner, nil
}

// labelling reads the Kustomization owner that obj's labels name; nil when it
// is gone.
func labelling(ctx context.Context, dyn dynamic.Interface, owner *Owner, obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	ks, err := dyn.Resource(KustomizationGVR).Namespace(owner.Namespace).Get(ctx, owner.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s for whether it still applies %s %s/%s: %w", owner, obj.GetKind(), obj.GetNamespace(), obj.GetName(), err)
	}
	return ks, nil
}

// inventoried reports whether the Kustomization ks has recorded an inventory
// and whether it lists obj.
func inventoried(ks, obj *unstructured.Unstructured) (lists, recorded bool) {
	entries, recorded, _ := unstructured.NestedSlice(ks.Object, "status", "inventory", "entries")
	return recorded && inventoryLists(entries, obj), recorded
}

// Refuse is ErrGitOpsOwned for an object Flux applies from git (Applying),
// nil for one written live or left behind by its Kustomization.
func Refuse(ctx context.Context, dyn dynamic.Interface, obj *unstructured.Unstructured) error {
	owner, err := Applying(ctx, dyn, obj)
	if err != nil {
		return err
	}
	if owner == nil {
		return nil
	}
	return fmt.Errorf("%w: %s", backend.ErrGitOpsOwned, Refusal(obj.GetKind(), obj.GetNamespace(), obj.GetName(), owner))
}

// inventoryLists reports whether the inventory entries name obj: an entry's
// id is kustomize-controller's namespace_name_group_kind, the group empty
// for the core API.
func inventoryLists(entries []any, obj *unstructured.Unstructured) bool {
	id := inventoryID(obj)
	for _, e := range entries {
		entry, _ := e.(map[string]any)
		if entry["id"] == id {
			return true
		}
	}
	return false
}

func inventoryID(obj *unstructured.Unstructured) string {
	gvk := obj.GroupVersionKind()
	return strings.Join([]string{obj.GetNamespace(), obj.GetName(), gvk.Group, gvk.Kind}, "_")
}
