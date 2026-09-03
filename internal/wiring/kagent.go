// Package wiring creates kagent ModelConfigs for models a backend serves, so
// agents can use them without manual steps. The Ollama backend maps to
// kagent's native keyless Ollama provider; OpenAI-compatible endpoints get a
// placeholder API-key secret (kagent's OpenAI runtime refuses to start without
// one even when the endpoint never checks it).
package wiring

import (
	"context"
	"fmt"
	"hash/fnv"
	"strings"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"

	"github.com/giantswarm/model-manager/internal/backend"
)

const (
	// ManagedByLabel / ManagedByValue mark the CRs model-manager owns; unwire
	// and prune never touch anything else.
	ManagedByLabel = "app.kubernetes.io/managed-by"
	ManagedByValue = "model-manager"
	// BackendLabel records which driver produced the ModelConfig.
	BackendLabel = "model-manager.giantswarm.io/backend"
	// ModelAnnotation carries the exact model reference (label values cannot
	// hold ':' or '/').
	ModelAnnotation = "model-manager.giantswarm.io/model"

	// KagentGroup / ModelConfigResource identify the CRD.
	KagentGroup         = "kagent.dev"
	ModelConfigResource = "modelconfigs"
	// DefaultAPIVersion is used when discovery is unavailable.
	DefaultAPIVersion = "v1alpha2"

	placeholderSecretKey   = "OPENAI_API_KEY" // #nosec G101 -- env var name, not a credential
	placeholderSecretValue = "placeholder"
	acceptedCondition      = "Accepted"
	maxNameLength          = 63
)

var secretGVR = schema.GroupVersionResource{Version: "v1", Resource: "secrets"}

// ModelConfigRef is what clients get about a model's agent wiring.
type ModelConfigRef struct {
	Name       string `json:"name"`
	Namespace  string `json:"namespace"`
	APIVersion string `json:"apiVersion,omitempty"`
	Provider   string `json:"provider,omitempty"`
	Model      string `json:"model,omitempty"`
	// Ready mirrors the kagent Accepted condition.
	Ready   bool   `json:"ready"`
	Message string `json:"message,omitempty"`
	// Managed is true when model-manager created the ModelConfig; others
	// (the portal's, hand-written ones) are reported but never modified.
	Managed bool `json:"managed"`
	// ProviderModel is spec.model — the name the provider serves the model
	// under (kserve: the InferenceService name); Model is the backend's
	// reference.
	ProviderModel string `json:"providerModel,omitempty"`
	// Endpoint is the provider endpoint: openAI.baseUrl or ollama.host.
	Endpoint string `json:"endpoint,omitempty"`
}

// Wirer manages the agent-facing configuration for models.
type Wirer interface {
	// Ensure creates or updates the ModelConfig for model (idempotent).
	Ensure(ctx context.Context, model string, ep backend.AgentEndpoint) (*ModelConfigRef, error)
	// Remove deletes the ModelConfig for model; absent is not an error. Only
	// model-manager's own ModelConfigs are ever deleted.
	Remove(ctx context.Context, model string) error
	// Lookup returns the ModelConfig for model, or nil when none exists.
	Lookup(ctx context.Context, model string) (*ModelConfigRef, error)
	// List returns all model-manager-owned ModelConfigs keyed by model reference.
	List(ctx context.Context) (map[string]ModelConfigRef, error)
	// ListAll returns every ModelConfig in the namespace, whoever created it,
	// so callers can recognise a model that is already wired by someone else.
	ListAll(ctx context.Context) ([]ModelConfigRef, error)
}

// Kagent is the Wirer over the kagent.dev ModelConfig CRD.
type Kagent struct {
	client    dynamic.Interface
	clientFor func(ctx context.Context) dynamic.Interface
	gvr       schema.GroupVersionResource
	namespace string
	prefix    string
	backend   backend.Name
}

// NewKagent builds a Wirer writing into namespace with the given API version.
func NewKagent(client dynamic.Interface, namespace, apiVersion, prefix string, b backend.Name) *Kagent {
	if apiVersion == "" {
		apiVersion = DefaultAPIVersion
	}
	return &Kagent{
		client:    client,
		gvr:       schema.GroupVersionResource{Group: KagentGroup, Version: apiVersion, Resource: ModelConfigResource},
		namespace: namespace,
		prefix:    prefix,
		backend:   b,
	}
}

// WithClientFor makes every call pick its client from ctx (downstream OAuth:
// the caller's own clients when the request carries the caller's token, the
// ServiceAccount's otherwise). fn returning nil falls back to the base client.
func (k *Kagent) WithClientFor(fn func(ctx context.Context) dynamic.Interface) *Kagent {
	k.clientFor = fn
	return k
}

// dyn is the client for this call.
func (k *Kagent) dyn(ctx context.Context) dynamic.Interface {
	if k.clientFor != nil {
		if c := k.clientFor(ctx); c != nil {
			return c
		}
	}
	return k.client
}

// Namespace returns the target namespace.
func (k *Kagent) Namespace() string { return k.namespace }

// APIVersion returns the CRD version in use.
func (k *Kagent) APIVersion() string { return k.gvr.Version }

// DiscoverAPIVersion returns the API server's preferred version of the
// ModelConfig CRD.
func DiscoverAPIVersion(dc discovery.DiscoveryInterface) (string, error) {
	groups, err := dc.ServerGroups()
	if err != nil {
		return "", fmt.Errorf("discover API groups: %w", err)
	}
	for _, g := range groups.Groups {
		if g.Name != KagentGroup {
			continue
		}
		candidates := []string{g.PreferredVersion.Version}
		for _, v := range g.Versions {
			if v.Version != g.PreferredVersion.Version {
				candidates = append(candidates, v.Version)
			}
		}
		for _, v := range candidates {
			res, err := dc.ServerResourcesForGroupVersion(KagentGroup + "/" + v)
			if err != nil {
				continue
			}
			for _, r := range res.APIResources {
				if r.Name == ModelConfigResource {
					return v, nil
				}
			}
		}
		return "", fmt.Errorf("group %s has no %s resource", KagentGroup, ModelConfigResource)
	}
	return "", fmt.Errorf("API group %s not found (is kagent installed?)", KagentGroup)
}

// Ensure implements Wirer.
func (k *Kagent) Ensure(ctx context.Context, model string, ep backend.AgentEndpoint) (*ModelConfigRef, error) {
	if strings.TrimSpace(model) == "" {
		return nil, fmt.Errorf("%w: empty model name", backend.ErrInvalid)
	}
	name := ModelConfigName(k.prefix, model)
	if ep.Name != "" {
		name = k.prefixed(ep.Name)
	}
	desired := k.build(name, model, ep)

	res := k.dyn(ctx).Resource(k.gvr).Namespace(k.namespace)
	// Converge: an owned ModelConfig for this model under another name (an
	// earlier naming rule) is replaced, never duplicated.
	if old, err := k.find(ctx, model); err != nil {
		return nil, err
	} else if old != nil && old.GetName() != name {
		if err := k.removeObj(ctx, old.GetName()); err != nil {
			return nil, err
		}
	}
	existing, err := res.Get(ctx, name, metav1.GetOptions{})
	switch {
	case errors.IsNotFound(err):
		if ep.PlaceholderAPIKey {
			if err := k.ensurePlaceholderSecret(ctx, name); err != nil {
				return nil, err
			}
		}
		created, err := res.Create(ctx, desired, metav1.CreateOptions{FieldManager: ManagedByValue})
		if err != nil {
			return nil, fmt.Errorf("create ModelConfig %s/%s: %w", k.namespace, name, err)
		}
		return toRef(created), nil
	case err != nil:
		return nil, fmt.Errorf("get ModelConfig %s/%s: %w", k.namespace, name, err)
	}
	if existing.GetLabels()[ManagedByLabel] != ManagedByValue {
		return nil, fmt.Errorf("%w: ModelConfig %s/%s exists but is not managed by %s", backend.ErrConflict, k.namespace, name, ManagedByValue)
	}
	if ep.PlaceholderAPIKey {
		if err := k.ensurePlaceholderSecret(ctx, name); err != nil {
			return nil, err
		}
	}
	// Preserve server-side metadata, replace what we own.
	desired.SetResourceVersion(existing.GetResourceVersion())
	desired.SetUID(existing.GetUID())
	labels := existing.GetLabels()
	for key, v := range desired.GetLabels() {
		labels[key] = v
	}
	desired.SetLabels(labels)
	annotations := existing.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	for key, v := range desired.GetAnnotations() {
		annotations[key] = v
	}
	desired.SetAnnotations(annotations)
	updated, err := res.Update(ctx, desired, metav1.UpdateOptions{FieldManager: ManagedByValue})
	if err != nil {
		return nil, fmt.Errorf("update ModelConfig %s/%s: %w", k.namespace, name, err)
	}
	// Update drops status from the returned object on some servers; keep the
	// observed one.
	if _, ok := existing.Object["status"]; ok {
		updated.Object["status"] = existing.Object["status"]
	}
	return toRef(updated), nil
}

// Remove implements Wirer.
func (k *Kagent) Remove(ctx context.Context, model string) error {
	obj, err := k.find(ctx, model)
	if err != nil {
		return err
	}
	if obj == nil {
		return nil
	}
	return k.removeObj(ctx, obj.GetName())
}

// removeObj deletes an owned ModelConfig and its placeholder Secret.
func (k *Kagent) removeObj(ctx context.Context, name string) error {
	res := k.dyn(ctx).Resource(k.gvr).Namespace(k.namespace)
	if err := res.Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("delete ModelConfig %s/%s: %w", k.namespace, name, err)
	}
	secrets := k.dyn(ctx).Resource(secretGVR).Namespace(k.namespace)
	secretName := placeholderSecretName(name)
	sec, err := secrets.Get(ctx, secretName, metav1.GetOptions{})
	if err == nil && sec.GetLabels()[ManagedByLabel] == ManagedByValue {
		if err := secrets.Delete(ctx, secretName, metav1.DeleteOptions{}); err != nil && !errors.IsNotFound(err) {
			return fmt.Errorf("delete Secret %s/%s: %w", k.namespace, secretName, err)
		}
	}
	return nil
}

// Lookup implements Wirer.
func (k *Kagent) Lookup(ctx context.Context, model string) (*ModelConfigRef, error) {
	obj, err := k.find(ctx, model)
	if err != nil || obj == nil {
		return nil, err
	}
	return toRef(obj), nil
}

// List implements Wirer.
func (k *Kagent) List(ctx context.Context) (map[string]ModelConfigRef, error) {
	list, err := k.dyn(ctx).Resource(k.gvr).Namespace(k.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: ManagedByLabel + "=" + ManagedByValue,
	})
	if err != nil {
		return nil, fmt.Errorf("list ModelConfigs in %s: %w", k.namespace, err)
	}
	out := make(map[string]ModelConfigRef, len(list.Items))
	for i := range list.Items {
		item := &list.Items[i]
		model := item.GetAnnotations()[ModelAnnotation]
		if model == "" {
			model, _, _ = unstructured.NestedString(item.Object, "spec", "model")
		}
		if model == "" {
			continue
		}
		out[model] = *toRef(item)
	}
	return out, nil
}

// ListAll implements Wirer.
func (k *Kagent) ListAll(ctx context.Context) ([]ModelConfigRef, error) {
	list, err := k.dyn(ctx).Resource(k.gvr).Namespace(k.namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list ModelConfigs in %s: %w", k.namespace, err)
	}
	out := make([]ModelConfigRef, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, *toRef(&list.Items[i]))
	}
	return out, nil
}

// prefixed applies the configured name prefix to a backend-chosen name.
func (k *Kagent) prefixed(name string) string {
	if k.prefix == "" {
		return name
	}
	return ModelConfigName(k.prefix, name)
}

// find returns the owned ModelConfig for model, matching the annotation first
// and the derived name second.
func (k *Kagent) find(ctx context.Context, model string) (*unstructured.Unstructured, error) {
	res := k.dyn(ctx).Resource(k.gvr).Namespace(k.namespace)
	list, err := res.List(ctx, metav1.ListOptions{LabelSelector: ManagedByLabel + "=" + ManagedByValue})
	if err != nil {
		return nil, fmt.Errorf("list ModelConfigs in %s: %w", k.namespace, err)
	}
	for i := range list.Items {
		if list.Items[i].GetAnnotations()[ModelAnnotation] == model {
			return &list.Items[i], nil
		}
	}
	name := ModelConfigName(k.prefix, model)
	for i := range list.Items {
		if list.Items[i].GetName() == name {
			return &list.Items[i], nil
		}
	}
	return nil, nil
}

func (k *Kagent) build(name, model string, ep backend.AgentEndpoint) *unstructured.Unstructured {
	spec := map[string]any{
		"provider": ep.Provider,
		"model":    ep.Model,
	}
	if spec["model"] == "" {
		spec["model"] = model
	}
	switch ep.Provider {
	case "Ollama":
		spec["ollama"] = map[string]any{"host": ep.Host}
	case "OpenAI":
		spec["openAI"] = map[string]any{"baseUrl": ep.BaseURL}
	}
	if ep.PlaceholderAPIKey {
		spec["apiKeySecret"] = placeholderSecretName(name)
		spec["apiKeySecretKey"] = placeholderSecretKey
	}
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": k.gvr.Group + "/" + k.gvr.Version,
		"kind":       "ModelConfig",
		"metadata": map[string]any{
			"name":      name,
			"namespace": k.namespace,
			"labels": map[string]any{
				ManagedByLabel: ManagedByValue,
				BackendLabel:   string(k.backend),
			},
			"annotations": map[string]any{
				ModelAnnotation: model,
			},
		},
		"spec": spec,
	}}
	return obj
}

func (k *Kagent) ensurePlaceholderSecret(ctx context.Context, mcName string) error {
	name := placeholderSecretName(mcName)
	secrets := k.dyn(ctx).Resource(secretGVR).Namespace(k.namespace)
	_, err := secrets.Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		return nil
	}
	if !errors.IsNotFound(err) {
		return fmt.Errorf("get Secret %s/%s: %w", k.namespace, name, err)
	}
	sec := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata": map[string]any{
			"name":      name,
			"namespace": k.namespace,
			"labels":    map[string]any{ManagedByLabel: ManagedByValue},
		},
		"type":       "Opaque",
		"stringData": map[string]any{placeholderSecretKey: placeholderSecretValue},
	}}
	if _, err := secrets.Create(ctx, sec, metav1.CreateOptions{FieldManager: ManagedByValue}); err != nil && !errors.IsAlreadyExists(err) {
		return fmt.Errorf("create Secret %s/%s: %w", k.namespace, name, err)
	}
	return nil
}

func placeholderSecretName(mcName string) string {
	const suffix = "-api-key"
	if len(mcName)+len(suffix) > maxNameLength {
		mcName = mcName[:maxNameLength-len(suffix)]
	}
	return strings.TrimRight(mcName, "-") + suffix
}

func toRef(obj *unstructured.Unstructured) *ModelConfigRef {
	ref := &ModelConfigRef{
		Name:       obj.GetName(),
		Namespace:  obj.GetNamespace(),
		APIVersion: obj.GetAPIVersion(),
	}
	ref.Provider, _, _ = unstructured.NestedString(obj.Object, "spec", "provider")
	ref.Model, _, _ = unstructured.NestedString(obj.Object, "spec", "model")
	ref.ProviderModel = ref.Model
	if m := obj.GetAnnotations()[ModelAnnotation]; m != "" {
		ref.Model = m
	}
	ref.Managed = obj.GetLabels()[ManagedByLabel] == ManagedByValue
	if u, _, _ := unstructured.NestedString(obj.Object, "spec", "openAI", "baseUrl"); u != "" {
		ref.Endpoint = u
	} else if h, _, _ := unstructured.NestedString(obj.Object, "spec", "ollama", "host"); h != "" {
		ref.Endpoint = h
	}
	conds, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	for _, c := range conds {
		cm, ok := c.(map[string]any)
		if !ok || cm["type"] != acceptedCondition {
			continue
		}
		ref.Ready = cm["status"] == "True"
		if msg, ok := cm["message"].(string); ok {
			ref.Message = msg
		}
	}
	if len(conds) == 0 {
		ref.Message = "not yet reconciled"
	}
	return ref
}

// ModelConfigName derives a DNS-1123 label from a model reference:
// "smollm2:135m" -> "smollm2-135m", "hf.co/org/Repo:Q4_K_M" ->
// "hf-co-org-repo-q4-k-m". Names longer than 63 characters keep a prefix plus
// a short hash of the full reference so distinct models never collide.
func ModelConfigName(prefix, model string) string {
	raw := strings.ToLower(strings.TrimSpace(model))
	var b strings.Builder
	lastDash := true // trims leading dashes
	for _, r := range raw {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	name := strings.TrimRight(b.String(), "-")
	if name == "" {
		name = "model"
	}
	if name[0] >= '0' && name[0] <= '9' {
		name = "m-" + name
	}
	if prefix != "" {
		name = strings.TrimRight(prefix, "-") + "-" + name
	}
	if len(name) > maxNameLength {
		h := fnv.New32a()
		_, _ = h.Write([]byte(raw))
		suffix := fmt.Sprintf("-%08x", h.Sum32())
		name = strings.TrimRight(name[:maxNameLength-len(suffix)], "-") + suffix
	}
	return name
}
