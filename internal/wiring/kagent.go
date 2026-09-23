// Package wiring creates kagent ModelConfigs for models a backend serves, so
// agents can use them without manual steps. The Ollama backend maps to
// kagent's native keyless Ollama provider. An OpenAI-compatible endpoint
// presents one of three API-key shapes (backend.AgentEndpoint): the caller's
// Bearer token forwarded (apiKeyPassthrough — an endpoint that admits a
// person's token, such as a route on the kserve backend's models Gateway), a
// static key in a Secret of the caller's (apiKeySecret), or the placeholder
// Secret model-manager creates (kagent's OpenAI runtime refuses to start
// without a key even when the endpoint never checks it). apiKeyPassthrough
// and apiKeySecret are mutually exclusive on the ModelConfig.
//
// The ModelConfig is written in the kagent.dev API version the apiserver
// serves — kagent API v2 serves v1alpha3 only (no v1alpha2, no conversion
// webhook). apiKeyPassthrough exists in v1alpha3 only; the other spec fields
// model-manager writes (provider, model, ollama.host, ollama.options,
// openAI.baseUrl, apiKeySecret/apiKeySecretKey) are the same in v1alpha2 and
// v1alpha3. The status is not: v1alpha3 reports Accepted (the spec is valid)
// and ResolvedRefs (the referenced Secret exists and holds the key) as
// separate conditions, so a ModelConfig is ready only when both hold.
//
// An Ollama ModelConfig carries the context window agents run the model at
// (spec.ollama.options.num_ctx, AgentEndpoint.ContextLength): without it
// every request runs at the server's default, which Ollama derives from the
// host's VRAM — 4,096 tokens below 24 GiB — and a longer agent prompt loses
// its front, the system prompt and the tool schemas, without an error.
package wiring

import (
	"context"
	"fmt"
	"hash/fnv"
	"strconv"
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
	// DefaultAPIVersion is used when discovery is unavailable: the version
	// kagent API v2 serves.
	DefaultAPIVersion = "v1alpha3"

	placeholderSecretKey   = "OPENAI_API_KEY" // #nosec G101 -- env var name, not a credential
	placeholderSecretValue = "placeholder"
	acceptedCondition      = "Accepted"
	resolvedRefsCondition  = "ResolvedRefs"
	maxNameLength          = 63
	// ollamaNumCtx is the spec.ollama.options key of the context window. The
	// CRD's options are strings; the ADK sends num_ctx as an integer.
	ollamaNumCtx = "num_ctx"
)

var secretGVR = schema.GroupVersionResource{Version: "v1", Resource: "secrets"}

// ModelConfigRef is what clients get about a model's agent wiring.
type ModelConfigRef struct {
	Name       string `json:"name"`
	Namespace  string `json:"namespace"`
	APIVersion string `json:"apiVersion,omitempty"`
	Provider   string `json:"provider,omitempty"`
	Model      string `json:"model,omitempty"`
	// Ready is true when kagent has accepted the ModelConfig (condition
	// Accepted) and, where the controller reports it, resolved what it
	// references (condition ResolvedRefs: the API-key Secret exists and holds
	// the key). A status without ResolvedRefs (kagent 0.x) is judged by
	// Accepted alone.
	Ready bool `json:"ready"`
	// Message is the message of the condition holding Ready back, or the
	// Accepted message once ready.
	Message string `json:"message,omitempty"`
	// Managed is true when model-manager created the ModelConfig; others
	// (the portal's, hand-written ones) are reported but never modified.
	Managed bool `json:"managed"`
	// ProviderModel is spec.model — the name the provider serves the model
	// under (kserve: the LLMInferenceService name); Model is the backend's
	// reference.
	ProviderModel string `json:"providerModel,omitempty"`
	// Endpoint is the provider endpoint: openAI.baseUrl or ollama.host.
	Endpoint string `json:"endpoint,omitempty"`
	// APIKeyPassthrough is spec.apiKeyPassthrough: the agent forwards the
	// caller's Bearer token to the endpoint as the API key. APIKeySecret is
	// spec.apiKeySecret: the Secret a static key is read from — the
	// placeholder Secret or the caller's own. At most one is set.
	APIKeyPassthrough bool   `json:"apiKeyPassthrough,omitempty"`
	APIKeySecret      string `json:"apiKeySecret,omitempty"`
	// Backend is the driver that produced the ModelConfig (the
	// model-manager.giantswarm.io/backend label); together with Model it
	// identifies the ModelConfig when one model-manager runs several
	// backends. Empty on a ModelConfig without the label.
	Backend backend.Name `json:"backend,omitempty"`
	// ContextLength is the context window in tokens agents run the model at:
	// spec.ollama.options.num_ctx. 0 when the ModelConfig sets none, and the
	// server's default applies.
	ContextLength int64 `json:"contextLength,omitempty"`
}

// Wirer manages the agent-facing configuration for models. A ModelConfig is
// identified by (backend, model): the backend label plus the model
// annotation, so the same reference on two backends is two ModelConfigs.
type Wirer interface {
	// Ensure creates or updates the ModelConfig for model on ep.Backend
	// (idempotent).
	Ensure(ctx context.Context, model string, ep backend.AgentEndpoint) (*ModelConfigRef, error)
	// Remove deletes the ModelConfig for model on backend b; absent is not an
	// error. Only model-manager's own ModelConfigs are ever deleted.
	Remove(ctx context.Context, b backend.Name, model string) error
	// Lookup returns the ModelConfig for model on backend b, or nil when none
	// exists.
	Lookup(ctx context.Context, b backend.Name, model string) (*ModelConfigRef, error)
	// List returns all model-manager-owned ModelConfigs, each with its
	// backend and model reference.
	List(ctx context.Context) ([]ModelConfigRef, error)
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
}

// NewKagent builds a Wirer writing into namespace with the given API version.
// The backend a ModelConfig belongs to comes with every AgentEndpoint, so one
// wirer serves every backend of the process.
func NewKagent(client dynamic.Interface, namespace, apiVersion, prefix string) *Kagent {
	if apiVersion == "" {
		apiVersion = DefaultAPIVersion
	}
	return &Kagent{
		client:    client,
		gvr:       schema.GroupVersionResource{Group: KagentGroup, Version: apiVersion, Resource: ModelConfigResource},
		namespace: namespace,
		prefix:    prefix,
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

// DiscoverAPIVersion returns the kagent.dev version the API server serves
// ModelConfigs in: the group's preferred version when it has the resource,
// else the first other version that does. So a kagent 0.x cluster yields
// v1alpha2 and a kagent API v2 cluster v1alpha3; without the group (kagent
// not installed, discovery unreachable) it errs and the caller falls back to
// DefaultAPIVersion.
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
	if err := ep.Validate(); err != nil {
		return nil, err
	}
	target := ModelConfigName(k.prefix, model)
	if ep.Name != "" {
		target = k.prefixed(ep.Name)
	}
	res := k.dyn(ctx).Resource(k.gvr).Namespace(k.namespace)
	// The derived name may already belong to the same reference on ANOTHER
	// backend (one model-manager, several backends): that ModelConfig keeps
	// the plain name, this one gets the backend appended. The annotation
	// still names the exact reference on both.
	if taken, gerr := res.Get(ctx, target, metav1.GetOptions{}); gerr == nil && ownedByOtherBackend(taken, ep.Backend) {
		target = suffixed(target, ep.Backend)
	}
	// An owned ModelConfig for this (backend, model) keeps the name it has —
	// plain or suffixed — so a repeated Ensure never deletes and recreates
	// it. Only a backend-chosen name (ep.Name, the kserve LLMInferenceService
	// rule) converges an older derived name onto the new one, replacing it,
	// never duplicating it.
	name := target
	old, err := k.find(ctx, ep.Backend, model)
	if err != nil {
		return nil, err
	}
	if old != nil {
		if ep.Name != "" && old.GetName() != target {
			if err := k.removeObj(ctx, old.GetName()); err != nil {
				return nil, err
			}
		} else {
			name = old.GetName()
		}
	}
	desired := k.build(name, model, ep)
	existing, err := res.Get(ctx, name, metav1.GetOptions{})
	switch {
	case errors.IsNotFound(err):
		if placeholderNeeded(ep) {
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
	// The placeholder Secret follows the shape: created for it, removed when a
	// re-wire moves the ModelConfig off it (a stale placeholder would otherwise
	// outlive the ModelConfig's need for it).
	if placeholderNeeded(ep) {
		if err := k.ensurePlaceholderSecret(ctx, name); err != nil {
			return nil, err
		}
	} else if err := k.removePlaceholderSecret(ctx, name); err != nil {
		return nil, err
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
func (k *Kagent) Remove(ctx context.Context, b backend.Name, model string) error {
	obj, err := k.find(ctx, b, model)
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
	return k.removePlaceholderSecret(ctx, name)
}

// removePlaceholderSecret deletes the placeholder Secret of the ModelConfig
// named mcName when model-manager created it; a Secret of anyone else's or
// none at all is left alone.
func (k *Kagent) removePlaceholderSecret(ctx context.Context, mcName string) error {
	secrets := k.dyn(ctx).Resource(secretGVR).Namespace(k.namespace)
	secretName := placeholderSecretName(mcName)
	sec, err := secrets.Get(ctx, secretName, metav1.GetOptions{})
	if err != nil || sec.GetLabels()[ManagedByLabel] != ManagedByValue {
		return nil
	}
	if err := secrets.Delete(ctx, secretName, metav1.DeleteOptions{}); err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("delete Secret %s/%s: %w", k.namespace, secretName, err)
	}
	return nil
}

// placeholderNeeded reports whether the ModelConfig reads its key from the
// placeholder Secret: the provider insists on a key and neither the caller's
// token nor a Secret of the caller's supplies one.
func placeholderNeeded(ep backend.AgentEndpoint) bool {
	return ep.PlaceholderAPIKey && !ep.APIKeyPassthrough && ep.APIKeySecret == ""
}

// Lookup implements Wirer.
func (k *Kagent) Lookup(ctx context.Context, b backend.Name, model string) (*ModelConfigRef, error) {
	obj, err := k.find(ctx, b, model)
	if err != nil || obj == nil {
		return nil, err
	}
	return toRef(obj), nil
}

// List implements Wirer.
func (k *Kagent) List(ctx context.Context) ([]ModelConfigRef, error) {
	list, err := k.dyn(ctx).Resource(k.gvr).Namespace(k.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: ManagedByLabel + "=" + ManagedByValue,
	})
	if err != nil {
		return nil, fmt.Errorf("list ModelConfigs in %s: %w", k.namespace, err)
	}
	out := make([]ModelConfigRef, 0, len(list.Items))
	for i := range list.Items {
		ref := toRef(&list.Items[i])
		if ref.Model == "" {
			continue
		}
		out = append(out, *ref)
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

// find returns the owned ModelConfig for model on backend b, matching the
// annotation first and the derived name second. A ModelConfig without the
// backend label (written before the label existed) matches any backend; an
// empty b matches any label.
func (k *Kagent) find(ctx context.Context, b backend.Name, model string) (*unstructured.Unstructured, error) {
	res := k.dyn(ctx).Resource(k.gvr).Namespace(k.namespace)
	list, err := res.List(ctx, metav1.ListOptions{LabelSelector: ManagedByLabel + "=" + ManagedByValue})
	if err != nil {
		return nil, fmt.Errorf("list ModelConfigs in %s: %w", k.namespace, err)
	}
	for i := range list.Items {
		if list.Items[i].GetAnnotations()[ModelAnnotation] == model && backendMatches(&list.Items[i], b) {
			return &list.Items[i], nil
		}
	}
	name := ModelConfigName(k.prefix, model)
	for i := range list.Items {
		if list.Items[i].GetName() == name && backendMatches(&list.Items[i], b) {
			return &list.Items[i], nil
		}
	}
	return nil, nil
}

// backendMatches reports whether obj belongs to backend b: its backend label
// is b, or either side does not say.
func backendMatches(obj *unstructured.Unstructured, b backend.Name) bool {
	labelled := obj.GetLabels()[BackendLabel]
	return b == "" || labelled == "" || labelled == string(b)
}

// ownedByOtherBackend reports whether obj is a managed ModelConfig of a
// backend other than b (the name-collision case between backends).
func ownedByOtherBackend(obj *unstructured.Unstructured, b backend.Name) bool {
	labels := obj.GetLabels()
	return labels[ManagedByLabel] == ManagedByValue && labels[BackendLabel] != "" && labels[BackendLabel] != string(b)
}

// suffixed appends the backend to a ModelConfig name within the 63-character
// limit.
func suffixed(name string, b backend.Name) string {
	suffix := "-" + string(b)
	if len(name)+len(suffix) > maxNameLength {
		name = name[:maxNameLength-len(suffix)]
	}
	return strings.TrimRight(name, "-") + suffix
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
		ollama := map[string]any{"host": ep.Host}
		if ep.ContextLength > 0 {
			ollama["options"] = map[string]any{ollamaNumCtx: strconv.FormatInt(ep.ContextLength, 10)}
		}
		spec["ollama"] = ollama
	case "OpenAI":
		spec["openAI"] = map[string]any{"baseUrl": ep.BaseURL}
	}
	switch {
	case ep.APIKeyPassthrough:
		spec["apiKeyPassthrough"] = true
	case ep.APIKeySecret != "":
		spec["apiKeySecret"] = ep.APIKeySecret
		spec["apiKeySecretKey"] = ep.APIKeySecretKey
		if ep.APIKeySecretKey == "" {
			spec["apiKeySecretKey"] = placeholderSecretKey
		}
	case ep.PlaceholderAPIKey:
		spec["apiKeySecret"] = placeholderSecretName(name)
		spec["apiKeySecretKey"] = placeholderSecretKey
	}
	labels := map[string]any{ManagedByLabel: ManagedByValue}
	if ep.Backend != "" {
		labels[BackendLabel] = string(ep.Backend)
	}
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": k.gvr.Group + "/" + k.gvr.Version,
		"kind":       "ModelConfig",
		"metadata": map[string]any{
			"name":      name,
			"namespace": k.namespace,
			"labels":    labels,
			"annotations": map[string]any{
				ModelAnnotation: model,
			},
		},
		"spec": spec,
	}}
	return obj
}

func (k *Kagent) ensurePlaceholderSecret(ctx context.Context, mcName string) error {
	sec := k.placeholderSecret(mcName)
	name := sec.GetName()
	secrets := k.dyn(ctx).Resource(secretGVR).Namespace(k.namespace)
	_, err := secrets.Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		return nil
	}
	if !errors.IsNotFound(err) {
		return fmt.Errorf("get Secret %s/%s: %w", k.namespace, name, err)
	}
	if _, err := secrets.Create(ctx, sec, metav1.CreateOptions{FieldManager: ManagedByValue}); err != nil && !errors.IsAlreadyExists(err) {
		return fmt.Errorf("create Secret %s/%s: %w", k.namespace, name, err)
	}
	return nil
}

// placeholderSecret is the API-key Secret an OpenAI-compatible ModelConfig
// references (spec.apiKeySecret / apiKeySecretKey); kagent resolves it into
// the ResolvedRefs condition.
func (k *Kagent) placeholderSecret(mcName string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata": map[string]any{
			"name":      placeholderSecretName(mcName),
			"namespace": k.namespace,
			"labels":    map[string]any{ManagedByLabel: ManagedByValue},
		},
		"type":       "Opaque",
		"stringData": map[string]any{placeholderSecretKey: placeholderSecretValue},
	}}
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
	ref.Backend = backend.Name(obj.GetLabels()[BackendLabel])
	if u, _, _ := unstructured.NestedString(obj.Object, "spec", "openAI", "baseUrl"); u != "" {
		ref.Endpoint = u
	} else if h, _, _ := unstructured.NestedString(obj.Object, "spec", "ollama", "host"); h != "" {
		ref.Endpoint = h
	}
	ref.APIKeyPassthrough, _, _ = unstructured.NestedBool(obj.Object, "spec", "apiKeyPassthrough")
	ref.APIKeySecret, _, _ = unstructured.NestedString(obj.Object, "spec", "apiKeySecret")
	if n, _, _ := unstructured.NestedString(obj.Object, "spec", "ollama", "options", ollamaNumCtx); n != "" {
		ref.ContextLength, _ = strconv.ParseInt(n, 10, 64)
	}
	conds, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	ref.Ready, ref.Message = readiness(conds)
	return ref
}

// readiness derives Ready and Message from a ModelConfig's status conditions:
// Accepted must be True and ResolvedRefs, when the controller reports it, must
// be True too — kagent API v2 reports a missing or incomplete Secret there
// while Accepted stays True. The message is the failing condition's, or
// Accepted's when nothing fails.
func readiness(conds []any) (ready bool, message string) {
	if len(conds) == 0 {
		return false, "not yet reconciled"
	}
	var accepted, unresolved bool
	var acceptedMsg, unresolvedMsg string
	for _, c := range conds {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		msg, _ := cm["message"].(string)
		switch cm["type"] {
		case acceptedCondition:
			accepted, acceptedMsg = cm["status"] == "True", msg
		case resolvedRefsCondition:
			unresolved, unresolvedMsg = cm["status"] != "True", msg
		}
	}
	switch {
	case !accepted:
		return false, acceptedMsg
	case unresolved:
		return false, unresolvedMsg
	}
	return true, acceptedMsg
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
