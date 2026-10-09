// Package gitops tells an object Flux applies from git from one written
// live. An object carrying Flux's provenance labels — a Kustomization's, or a
// HelmRelease's for an object a chart Flux installs renders — is GitOps-owned
// whatever its app.kubernetes.io/managed-by says: a manifest committed by
// model-manager carries model-manager's own labels, so the inventory still
// recognises it as the model's wiring, and the provenance is what makes it
// read-only live. A write changes such an object in git (mode commit) or not
// at all; a live change would be reverted on Flux's next reconciliation.
package gitops

import (
	"fmt"

	"github.com/giantswarm/model-manager/internal/backend"
)

// The labels Flux sets on every object it applies.
const (
	LabelKustomizeName      = "kustomize.toolkit.fluxcd.io/name"
	LabelKustomizeNamespace = "kustomize.toolkit.fluxcd.io/namespace"
	LabelHelmName           = "helm.toolkit.fluxcd.io/name"
	LabelHelmNamespace      = "helm.toolkit.fluxcd.io/namespace"
)

// The kinds of Flux object that apply an object.
const (
	KindKustomization = "Kustomization"
	KindHelmRelease   = "HelmRelease"
)

// Owner is the Flux object that applies an object from git; the backend
// package declares it so a served model reports its owner (LoadedModel).
type Owner = backend.GitOpsOwner

// OwnerOf is the Flux object the labels name, nil for an object Flux does not
// apply. A Kustomization's labels win over a HelmRelease's: the Kustomization
// is where the file lives.
func OwnerOf(labels map[string]string) *Owner {
	if name := labels[LabelKustomizeName]; name != "" {
		return &Owner{Kind: KindKustomization, Namespace: labels[LabelKustomizeNamespace], Name: name}
	}
	if name := labels[LabelHelmName]; name != "" {
		return &Owner{Kind: KindHelmRelease, Namespace: labels[LabelHelmNamespace], Name: name}
	}
	return nil
}

// Refusal is the error text of a live write on an object Flux applies: what
// it is, who applies it, and the way out.
func Refusal(kind, namespace, name string, owner *Owner) string {
	if owner.Kind == KindHelmRelease {
		return fmt.Sprintf("%s %s/%s is rendered by %s: a live change would be reverted by Flux; change the release's values in git", kind, namespace, name, owner)
	}
	return fmt.Sprintf("%s %s/%s is applied from git by %s: a live change would be reverted by Flux; use mode commit to change it in its repository", kind, namespace, name, owner)
}
