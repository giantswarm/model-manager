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
// The ModelConfig is written in the API group and version the apiserver
// serves: api.kagent.dev from kagent 1.3 on, kagent.dev before it. The
// cut-over to api.kagent.dev leaves the kagent.dev CRD in place, so the
// wirer prefers api.kagent.dev whenever it serves ModelConfigs. kagent API v2
// serves v1alpha3 only (no v1alpha2, no conversion webhook).
// apiKeyPassthrough exists in v1alpha3 only; the other spec fields
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
// its front, the system prompt and the tool schemas, without an error. The
// ModelConfig of a model with the thinking capability also carries the chat
// request's think field (spec.ollama.think, AgentEndpoint.Think), when the
// served ModelConfig schema has it (kagent 1.0.3 and later; servedSchema):
// the apiserver prunes a field its CRD lacks.
package wiring

import (
	"context"
	stderrors "errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"maps"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/openapi"

	"github.com/giantswarm/model-manager/internal/backend"
	"github.com/giantswarm/model-manager/internal/gitops"
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
	// InstanceLabel names the model-manager instance that created the
	// ModelConfig. It is written only when the ModelConfig is created, and
	// only that instance deletes it: ManagedByLabel is shared by every
	// model-manager and copied with a manifest, so it marks what
	// model-manager writes, not who may delete it.
	InstanceLabel = "model-manager.giantswarm.io/instance"

	// KagentGroup is the API group kagent serves ModelConfigs in from 1.3 on;
	// LegacyKagentGroup the one before it, which the cut-over leaves served.
	KagentGroup         = "api.kagent.dev"
	LegacyKagentGroup   = "kagent.dev"
	ModelConfigResource = "modelconfigs"
	// DefaultAPIVersion is used when discovery is unavailable.
	DefaultAPIVersion = KagentGroup + "/v1alpha3"
	// discoveryTTL is how long a discovered API version holds: a group that
	// appears under the running process is used within it. A miss of the
	// version in use re-discovers at once.
	discoveryTTL = time.Minute

	placeholderSecretKey   = "OPENAI_API_KEY" // #nosec G101 -- env var name, not a credential
	placeholderSecretValue = "placeholder"
	acceptedCondition      = "Accepted"
	resolvedRefsCondition  = "ResolvedRefs"
	maxNameLength          = 63
	// ollamaNumCtx is the spec.ollama.options key of the context window. The
	// CRD's options are strings; the ADK sends num_ctx as an integer.
	ollamaNumCtx = "num_ctx"
	// ollamaThink is the spec.ollama field of the chat request's think.
	ollamaThink = "think"
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
	// GitOps is the Flux object that applies the ModelConfig from git; a
	// ModelConfig with one is changed only in git (mode commit), whatever
	// Managed says.
	GitOps *gitops.Owner `json:"gitops,omitempty"`
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
	// CreatedBy is the model-manager instance that created the ModelConfig
	// (InstanceLabel), the only one that deletes it. Empty on a ModelConfig
	// without the label: model-manager adopts it but never deletes it.
	CreatedBy string `json:"createdBy,omitempty"`
	// Backend is the driver that produced the ModelConfig (the
	// model-manager.giantswarm.io/backend label); together with Model it
	// identifies the ModelConfig when one model-manager runs several
	// backends. Empty on a ModelConfig without the label.
	Backend backend.Name `json:"backend,omitempty"`
	// ContextLength is the context window in tokens agents run the model at:
	// spec.ollama.options.num_ctx. 0 when the ModelConfig sets none, and the
	// server's default applies.
	ContextLength int64 `json:"contextLength,omitempty"`
	// Think is the chat request's think field agents send: spec.ollama.think.
	// Nil when the ModelConfig sets none, and the server's default applies.
	Think *bool `json:"think,omitempty"`
}

// Carries reports whether the ModelConfig reads back with the settings ep
// has agents run the model at: its context window and think. The rest of the
// endpoint (provider, host, API-key shape) is not compared.
func (r ModelConfigRef) Carries(ep backend.AgentEndpoint) bool {
	sameThink := (r.Think == nil) == (ep.Think == nil) && (r.Think == nil || *r.Think == *ep.Think)
	return r.ContextLength == ep.ContextLength && sameThink
}

// NotOwnedError is a managed ModelConfig this model-manager instance did not
// create: another instance created it (CreatedBy), or it carries no
// InstanceLabel (CreatedBy empty: written before the label existed, and
// adopted by a wire since). Remove leaves it in place and Ensure does not
// write another instance's. It is a backend.ErrConflict.
type NotOwnedError struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	CreatedBy string `json:"createdBy,omitempty"`
	Message   string `json:"message"`
}

func (e *NotOwnedError) Error() string { return e.Message }

// Unwrap makes a NotOwnedError a backend.ErrConflict.
func (e *NotOwnedError) Unwrap() error { return backend.ErrConflict }

// Wirer manages the agent-facing configuration for models. A ModelConfig is
// identified by (backend, model): the backend label plus the model
// annotation, so the same reference on two backends is two ModelConfigs.
type Wirer interface {
	// Ensure creates or updates the ModelConfig for model on ep.Backend
	// (idempotent); one Flux applies from git is refused (ErrGitOpsOwned).
	Ensure(ctx context.Context, model string, ep backend.AgentEndpoint) (*ModelConfigRef, error)
	// Remove deletes the ModelConfig for model on backend b; absent is not an
	// error. Only a ModelConfig this instance created is deleted (any other is
	// a *NotOwnedError, left in place), and never one Flux applies from git
	// (ErrGitOpsOwned).
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
	// Render returns what Ensure would write for model on ep.Backend, and
	// the Flux object applying the ModelConfig it would update; nothing is
	// written. A dry run shows it, commit mode lands it in git.
	Render(ctx context.Context, model string, ep backend.AgentEndpoint) (*Rendered, error)
	// Removal returns what Remove would delete for model on backend b; no
	// objects when nothing is wired, or when the ModelConfig is one Remove
	// leaves (Rendered.Left). Nothing is deleted.
	Removal(ctx context.Context, b backend.Name, model string) (*Rendered, error)
	// Writable returns ep as Ensure writes it: without the settings the
	// served ModelConfig schema lacks, which the apiserver would prune — so
	// a comparison with what a ModelConfig reads back (Carries) holds once
	// it is written.
	Writable(ctx context.Context, ep backend.AgentEndpoint) (backend.AgentEndpoint, error)
}

// Kagent is the Wirer over kagent's ModelConfig CRD.
type Kagent struct {
	client    dynamic.Interface
	clientFor func(ctx context.Context) dynamic.Interface
	openAPI   openapi.ClientWithContext
	namespace string
	prefix    string
	// instance is the InstanceLabel value this process writes and deletes by.
	instance string
	// discover, when set, re-discovers the served version every
	// discoveryTTL and after a call misses it (WithDiscovery).
	discover func() (string, error)
	log      *slog.Logger

	mu         sync.RWMutex
	gvr        schema.GroupVersionResource
	schema     *servedSchema
	discovered time.Time
}

// NewKagent builds a Wirer writing into namespace with the given API version,
// a group/version ParseAPIVersion accepts (empty: DefaultAPIVersion).
// The backend a ModelConfig belongs to comes with every AgentEndpoint, so one
// wirer serves every backend of the process. openAPI is the apiserver's
// OpenAPI v3, which tells the ModelConfig fields it serves. instance names
// this model-manager among those writing into the namespace (InstanceLabel,
// made a valid label value by InstanceName); it must not be empty.
func NewKagent(client dynamic.Interface, openAPI openapi.ClientWithContext, namespace, apiVersion, prefix, instance string) *Kagent {
	if apiVersion == "" {
		apiVersion = DefaultAPIVersion
	}
	k := &Kagent{client: client, openAPI: openAPI, namespace: namespace, prefix: prefix, instance: InstanceName(instance)}
	k.setVersion(apiVersion)
	return k
}

// ParseAPIVersion validates a ModelConfig API version: a kagent group and a
// version, such as api.kagent.dev/v1alpha3 or kagent.dev/v1alpha2.
func ParseAPIVersion(apiVersion string) (schema.GroupVersion, error) {
	gv, err := schema.ParseGroupVersion(apiVersion)
	if err != nil {
		return schema.GroupVersion{}, fmt.Errorf("kagent API version %q: %w", apiVersion, err)
	}
	if (gv.Group != KagentGroup && gv.Group != LegacyKagentGroup) || gv.Version == "" {
		return schema.GroupVersion{}, fmt.Errorf("kagent API version %q: want %s/<version> or %s/<version>", apiVersion, KagentGroup, LegacyKagentGroup)
	}
	return gv, nil
}

// APIVersionSetting is how the ModelConfig group/version is chosen: Pinned,
// a full group/version used as is, or discovered — at Version only when it
// is set (the group still discovered), else at the group's preferred one.
type APIVersionSetting struct {
	Pinned  string
	Version string
}

// ParseAPIVersionSetting reads a setting: `auto` (or empty) discovers group
// and version, a bare version such as `v1alpha3` discovers the group serving
// it, a group/version ParseAPIVersion accepts pins both.
func ParseAPIVersionSetting(s string) (APIVersionSetting, error) {
	switch {
	case s == "" || s == "auto":
		return APIVersionSetting{}, nil
	case strings.Contains(s, "/"):
		if _, err := ParseAPIVersion(s); err != nil {
			return APIVersionSetting{}, err
		}
		return APIVersionSetting{Pinned: s}, nil
	case !kubeVersion.MatchString(s):
		return APIVersionSetting{}, fmt.Errorf("kagent API version %q: want auto, a version such as v1alpha3, or %s/<version>", s, KagentGroup)
	}
	return APIVersionSetting{Version: s}, nil
}

// Fallback is the group/version used when discovery fails.
func (s APIVersionSetting) Fallback() string {
	switch {
	case s.Pinned != "":
		return s.Pinned
	case s.Version != "":
		return KagentGroup + "/" + s.Version
	}
	return DefaultAPIVersion
}

// kubeVersion is a Kubernetes API version: v1, v1alpha3, v2beta1.
var kubeVersion = regexp.MustCompile(`^v[1-9][0-9]*((alpha|beta)[1-9][0-9]*)?$`)

// WithDiscovery makes the wirer follow the API version the apiserver serves.
// A call re-runs discover once the last discovery is discoveryTTL old, so a
// group kagent starts serving under the running process (api.kagent.dev
// next to a kagent.dev CRD the cut-over leaves) is used within it. A call
// that fails NotFound — the version it uses is no longer served — re-runs
// discover at once and, when the served version changed, is retried once at
// the new one. Without it (an explicit version) the version never changes.
func (k *Kagent) WithDiscovery(discover func() (string, error), log *slog.Logger) *Kagent {
	if log == nil {
		log = slog.Default()
	}
	k.mu.Lock()
	k.discover, k.log, k.discovered = discover, log, time.Now()
	k.mu.Unlock()
	return k
}

// setVersion switches to apiVersion; one ParseAPIVersion refuses is a bug
// of the caller's.
func (k *Kagent) setVersion(apiVersion string) {
	gv, err := ParseAPIVersion(apiVersion)
	if err != nil {
		panic(err)
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.gvr = gv.WithResource(ModelConfigResource)
	k.schema = &servedSchema{client: k.openAPI, gv: gv, now: time.Now}
}

// resource is the ModelConfig resource in the version in use.
func (k *Kagent) resource() schema.GroupVersionResource {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return k.gvr
}

func (k *Kagent) served() *servedSchema {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return k.schema
}

// rediscovered reports whether err is a miss of the version in use and
// discovery found another one, which the wirer now uses.
func (k *Kagent) rediscovered(err error) bool {
	missed := errors.IsNotFound(err) || stderrors.Is(err, errVersionNotServed)
	if k.discover == nil || !missed {
		return false
	}
	k.mu.Lock()
	k.discovered = time.Now()
	k.mu.Unlock()
	return k.rediscover()
}

// refresh re-discovers the served version once the last discovery is
// discoveryTTL old. One call in a window does it; the others go on at the
// version in use.
func (k *Kagent) refresh() {
	if k.discover == nil {
		return
	}
	k.mu.Lock()
	due := time.Since(k.discovered) >= discoveryTTL
	if due {
		k.discovered = time.Now()
	}
	k.mu.Unlock()
	if due {
		k.rediscover()
	}
}

// rediscover runs discover and switches to the version it finds; it reports
// whether that changed the version in use.
func (k *Kagent) rediscover() bool {
	old := k.APIVersion()
	v, err := k.discover()
	if err != nil {
		k.log.Warn("kagent API re-discovery failed", "apiVersion", old, "error", err)
		return false
	}
	if v == old {
		return false
	}
	if _, err := ParseAPIVersion(v); err != nil {
		k.log.Warn("kagent API re-discovery found an unusable version", "apiVersion", old, "error", err)
		return false
	}
	k.setVersion(v)
	k.log.Info("kagent API version changed", "from", old, "to", v)
	return true
}

// retried runs call at the version discovery holds current, and once more
// after a re-discovery its error caused.
func retried[T any](k *Kagent, call func() (T, error)) (T, error) {
	k.refresh()
	v, err := call()
	if err != nil && k.rediscovered(err) {
		return call()
	}
	return v, err
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

// Instance returns the InstanceLabel value this wirer writes and deletes by.
func (k *Kagent) Instance() string { return k.instance }

// InstanceName makes an instance name a label value: the DNS-label form
// ModelConfigName derives, so "kagent/model-manager" becomes
// "kagent-model-manager".
func InstanceName(name string) string { return ModelConfigName("", name) }

// createdHere reports whether this instance created obj.
func (k *Kagent) createdHere(obj *unstructured.Unstructured) bool {
	labels := obj.GetLabels()
	return labels[ManagedByLabel] == ManagedByValue && labels[InstanceLabel] == k.instance
}

// Tails of a NotOwnedError: what happened to the ModelConfig instead.
const (
	leftInPlace = "it is left in place: model-manager only deletes the ModelConfigs it created; delete it with kubectl once nothing references it"
	notWritten  = "nothing was written: only the instance that created it writes it"
)

// notOwned is the *NotOwnedError for obj, a managed ModelConfig this
// instance did not create; outcome says what happened to it instead.
func (k *Kagent) notOwned(obj *unstructured.Unstructured, outcome string) *NotOwnedError {
	e := &NotOwnedError{Namespace: obj.GetNamespace(), Name: obj.GetName(), CreatedBy: obj.GetLabels()[InstanceLabel]}
	if e.Namespace == "" {
		e.Namespace = k.namespace
	}
	who := "model-manager instance " + e.CreatedBy + " created it"
	if e.CreatedBy == "" {
		who = "it carries no " + InstanceLabel + " label (written before model-manager recorded its creator, or by hand)"
	}
	e.Message = fmt.Sprintf("ModelConfig %s/%s was not created by this model-manager (instance %s): %s; %s", e.Namespace, e.Name, k.instance, who, outcome)
	return e
}

// APIVersion returns the ModelConfig group/version in use.
func (k *Kagent) APIVersion() string { return k.resource().GroupVersion().String() }

// DiscoverAPIVersion returns the group/version the API server serves
// ModelConfigs in: api.kagent.dev when it serves them, else kagent.dev —
// within the group, version when it is set, else its preferred version when
// it has the resource, else the first other version that does. So kagent 1.3
// yields api.kagent.dev/v1alpha3 (whether or not the kagent.dev CRD is still
// there), kagent API v2 before 1.3 kagent.dev/v1alpha3 and kagent 0.x
// kagent.dev/v1alpha2. Without either (kagent not installed, discovery
// unreachable) it errs and the caller falls back to DefaultAPIVersion.
func DiscoverAPIVersion(dc discovery.DiscoveryInterface, version string) (string, error) {
	groups, err := dc.ServerGroups()
	if err != nil {
		return "", fmt.Errorf("discover API groups: %w", err)
	}
	var found []string
	for _, name := range []string{KagentGroup, LegacyKagentGroup} {
		for _, g := range groups.Groups {
			if g.Name != name {
				continue
			}
			found = append(found, name)
			if v := servedVersion(dc, g, version); v != "" {
				return name + "/" + v, nil
			}
		}
	}
	if len(found) > 0 && version != "" {
		return "", fmt.Errorf("API group %s has no %s resource at %s", strings.Join(found, ", "), ModelConfigResource, version)
	}
	if len(found) > 0 {
		return "", fmt.Errorf("API group %s has no %s resource", strings.Join(found, ", "), ModelConfigResource)
	}
	return "", fmt.Errorf("API groups %s and %s not found (is kagent installed?)", KagentGroup, LegacyKagentGroup)
}

// servedVersion is the version of g that serves ModelConfigs, preferred
// first, or only want when it is set; empty when none does.
func servedVersion(dc discovery.DiscoveryInterface, g metav1.APIGroup, want string) string {
	candidates := []string{g.PreferredVersion.Version}
	for _, v := range g.Versions {
		if v.Version != g.PreferredVersion.Version {
			candidates = append(candidates, v.Version)
		}
	}
	if want != "" {
		candidates = []string{want}
	}
	for _, v := range candidates {
		res, err := dc.ServerResourcesForGroupVersion(g.Name + "/" + v)
		if err != nil {
			continue
		}
		for _, r := range res.APIResources {
			if r.Name == ModelConfigResource {
				return v
			}
		}
	}
	return ""
}

// Rendered is what a wiring write lands: the objects it creates or updates
// (the ModelConfig, then its placeholder Secret where it needs one) or, for a
// removal, the objects it deletes. Name is the ModelConfig's name, GitOps the
// Flux object that applies the ModelConfig as it exists now (nil: none, or
// written live). Replaces names an older ModelConfig of the same model the
// write removes (the backend-chosen name converging, Ensure). Left is the
// ModelConfig a removal leaves because this instance did not create it.
type Rendered struct {
	Name     string
	Objects  []*unstructured.Unstructured
	GitOps   *gitops.Owner
	Replaces string
	Left     *NotOwnedError
}

// ModelConfig is the rendered ModelConfig, nil for a removal with nothing to
// remove.
func (r *Rendered) ModelConfig() *unstructured.Unstructured {
	for _, o := range r.Objects {
		if o.GetKind() == "ModelConfig" {
			return o
		}
	}
	return nil
}

// Render implements Wirer: Ensure's decision without the write.
func (k *Kagent) Render(ctx context.Context, model string, ep backend.AgentEndpoint) (*Rendered, error) {
	return retried(k, func() (*Rendered, error) { return k.render(ctx, model, ep) })
}

func (k *Kagent) render(ctx context.Context, model string, ep backend.AgentEndpoint) (*Rendered, error) {
	p, err := k.plan(ctx, model, ep)
	if err != nil {
		return nil, err
	}
	r := p.rendered()
	if obj := p.provenance(); obj != nil {
		if r.GitOps, err = gitops.Applying(ctx, k.dyn(ctx), obj); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// wirePlan is one Ensure decided against the cluster: the name, the desired
// objects, the ModelConfig as it exists (nil: none), the older one a
// backend-chosen name replaces, and the one Flux applies at kagent.dev that
// the desired one moves to api.kagent.dev (legacyGitOps).
type wirePlan struct {
	name     string
	desired  *unstructured.Unstructured
	secret   *unstructured.Unstructured
	existing *unstructured.Unstructured
	replaces *unstructured.Unstructured
	legacy   *unstructured.Unstructured
}

// rendered is the plan's objects; the Flux provenance of the ModelConfig
// it writes over is resolved by render (gitops.Applying).
func (p *wirePlan) rendered() *Rendered {
	r := &Rendered{Name: p.name, Objects: []*unstructured.Unstructured{p.desired}}
	if p.secret != nil {
		r.Objects = append(r.Objects, p.secret)
	}
	if p.replaces != nil {
		r.Replaces = p.replaces.GetName()
	}
	return r
}

// provenance is the ModelConfig whose Flux provenance the plan reports: the
// one it updates, else the one at kagent.dev it moves (legacy); nil for a
// fresh create.
func (p *wirePlan) provenance() *unstructured.Unstructured {
	if p.existing != nil {
		return p.existing
	}
	return p.legacy
}

// plan decides an Ensure: the name the ModelConfig gets and what it holds,
// refused when the name belongs to a ModelConfig model-manager does not
// manage. Nothing is written.
func (k *Kagent) plan(ctx context.Context, model string, ep backend.AgentEndpoint) (*wirePlan, error) {
	if strings.TrimSpace(model) == "" {
		return nil, fmt.Errorf("%w: empty model name", backend.ErrInvalid)
	}
	if err := ep.Validate(); err != nil {
		return nil, err
	}
	ep, err := k.writable(ctx, ep)
	if err != nil {
		return nil, err
	}
	target := ModelConfigName(k.prefix, model)
	if ep.Name != "" {
		target = k.prefixed(ep.Name)
	}
	res := k.dyn(ctx).Resource(k.resource()).Namespace(k.namespace)
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
	// never duplicating it — and only one this instance created, since the
	// replacement deletes it.
	p := &wirePlan{name: target}
	old, err := k.find(ctx, ep.Backend, model)
	if err != nil {
		return nil, err
	}
	switch {
	case old != nil && old.GetAPIVersion() != k.APIVersion():
		// Its file in git moves to the version in use under the name it has.
		p.legacy, p.name = old, old.GetName()
	case old != nil:
		if ep.Name != "" && old.GetName() != target && k.createdHere(old) {
			p.replaces = old
		} else {
			p.name = old.GetName()
		}
	}
	p.desired = k.build(p.name, model, ep)
	if placeholderNeeded(ep) {
		p.secret = k.placeholderSecret(p.name)
	}
	existing, err := res.Get(ctx, p.name, metav1.GetOptions{})
	switch {
	case errors.IsNotFound(err):
		return p, nil
	case err != nil:
		return nil, fmt.Errorf("get ModelConfig %s/%s: %w", k.namespace, p.name, err)
	}
	if existing.GetLabels()[ManagedByLabel] != ManagedByValue {
		return nil, fmt.Errorf("%w: ModelConfig %s/%s exists but is not managed by %s", backend.ErrConflict, k.namespace, p.name, ManagedByValue)
	}
	// Another instance's ModelConfig is its own to write; one without the
	// label is adopted — written, but never marked as created here.
	if creator := existing.GetLabels()[InstanceLabel]; creator != "" && creator != k.instance {
		return nil, k.notOwned(existing, notWritten)
	}
	if !k.createdHere(existing) {
		labels := p.desired.GetLabels()
		delete(labels, InstanceLabel)
		p.desired.SetLabels(labels)
	}
	p.existing = existing
	return p, nil
}

// Ensure implements Wirer. A ModelConfig Flux applies from git — the one it
// would update or the older one it would replace — is never written live:
// the answer is ErrGitOpsOwned.
func (k *Kagent) Ensure(ctx context.Context, model string, ep backend.AgentEndpoint) (*ModelConfigRef, error) {
	return retried(k, func() (*ModelConfigRef, error) { return k.ensure(ctx, model, ep) })
}

func (k *Kagent) ensure(ctx context.Context, model string, ep backend.AgentEndpoint) (*ModelConfigRef, error) {
	p, err := k.plan(ctx, model, ep)
	if err != nil {
		return nil, err
	}
	for _, obj := range []*unstructured.Unstructured{p.existing, p.replaces, p.legacy} {
		if err := k.refuseGitOps(ctx, obj); err != nil {
			return nil, err
		}
	}
	if p.replaces != nil {
		if err := k.removeObj(ctx, p.replaces.GetName()); err != nil {
			return nil, err
		}
	}
	res := k.dyn(ctx).Resource(k.resource()).Namespace(k.namespace)
	name, desired, existing := p.name, p.desired, p.existing
	if existing == nil {
		if p.secret != nil {
			if err := k.ensurePlaceholderSecret(ctx, name); err != nil {
				return nil, err
			}
		}
		created, err := res.Create(ctx, desired, metav1.CreateOptions{FieldManager: ManagedByValue})
		if err != nil {
			return nil, fmt.Errorf("create ModelConfig %s/%s: %w", k.namespace, name, err)
		}
		return k.ref(ctx, created)
	}
	// The placeholder Secret follows the shape: created for it, removed when a
	// re-wire moves the ModelConfig off it (a stale placeholder would otherwise
	// outlive the ModelConfig's need for it).
	if p.secret != nil {
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
	maps.Copy(labels, desired.GetLabels())
	desired.SetLabels(labels)
	annotations := existing.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	maps.Copy(annotations, desired.GetAnnotations())
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
	return k.ref(ctx, updated)
}

// refuseGitOps is ErrGitOpsOwned for an object Flux applies from git
// (gitops.Refuse: its Kustomization still lists it), nil otherwise and for
// nil.
func (k *Kagent) refuseGitOps(ctx context.Context, obj *unstructured.Unstructured) error {
	if obj == nil {
		return nil
	}
	return gitops.Refuse(ctx, k.dyn(ctx), obj)
}

// Writable implements Wirer. Only a setting that needs the served schema
// consults it: think, which kagent's ModelConfig has from 1.0.3 on.
func (k *Kagent) Writable(ctx context.Context, ep backend.AgentEndpoint) (backend.AgentEndpoint, error) {
	return retried(k, func() (backend.AgentEndpoint, error) { return k.writable(ctx, ep) })
}

func (k *Kagent) writable(ctx context.Context, ep backend.AgentEndpoint) (backend.AgentEndpoint, error) {
	if ep.Provider != "Ollama" || ep.Think == nil {
		return ep, nil
	}
	fields, err := k.served().ollamaFields(ctx)
	if err != nil {
		return ep, fmt.Errorf("read the served ModelConfig schema: %w", err)
	}
	if !fields[ollamaThink] {
		ep.Think = nil
	}
	return ep, nil
}

// Removal implements Wirer: Remove's decision without the write — the
// owned ModelConfig for model on backend b and the placeholder Secret
// model-manager created for it; no objects when nothing is wired.
func (k *Kagent) Removal(ctx context.Context, b backend.Name, model string) (*Rendered, error) {
	return retried(k, func() (*Rendered, error) { return k.removal(ctx, b, model) })
}

func (k *Kagent) removal(ctx context.Context, b backend.Name, model string) (*Rendered, error) {
	obj, err := k.find(ctx, b, model)
	if err != nil || obj == nil {
		return &Rendered{}, err
	}
	// One Flux applies from git is removed in git (commit mode), whoever
	// rendered it; a live one — or one its Kustomization left behind — only
	// by the instance that created it.
	owner, err := gitops.Applying(ctx, k.dyn(ctx), obj)
	if err != nil {
		return nil, err
	}
	if owner == nil && !k.createdHere(obj) {
		return &Rendered{Name: obj.GetName(), Left: k.notOwned(obj, leftInPlace)}, nil
	}
	r := &Rendered{Name: obj.GetName(), Objects: []*unstructured.Unstructured{obj}, GitOps: owner}
	sec, err := k.dyn(ctx).Resource(secretGVR).Namespace(k.namespace).Get(ctx, placeholderSecretName(obj.GetName()), metav1.GetOptions{})
	switch {
	case err == nil && sec.GetLabels()[ManagedByLabel] == ManagedByValue:
		r.Objects = append(r.Objects, sec)
	case err != nil && !errors.IsNotFound(err):
		return nil, fmt.Errorf("get Secret %s/%s: %w", k.namespace, placeholderSecretName(obj.GetName()), err)
	}
	return r, nil
}

// Remove implements Wirer. A ModelConfig Flux applies from git is never
// deleted live: the answer is ErrGitOpsOwned. One this instance did not
// create is left: the answer is its *NotOwnedError.
func (k *Kagent) Remove(ctx context.Context, b backend.Name, model string) error {
	_, err := retried(k, func() (struct{}, error) { return struct{}{}, k.remove(ctx, b, model) })
	return err
}

func (k *Kagent) remove(ctx context.Context, b backend.Name, model string) error {
	obj, err := k.find(ctx, b, model)
	if err != nil {
		return err
	}
	if obj == nil {
		return nil
	}
	if err := k.refuseGitOps(ctx, obj); err != nil {
		return err
	}
	if !k.createdHere(obj) {
		return k.notOwned(obj, leftInPlace)
	}
	return k.removeObj(ctx, obj.GetName())
}

// removeObj deletes a ModelConfig this instance created and its placeholder
// Secret.
func (k *Kagent) removeObj(ctx context.Context, name string) error {
	res := k.dyn(ctx).Resource(k.resource()).Namespace(k.namespace)
	if err := res.Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("delete ModelConfig %s/%s: %w", k.namespace, name, err)
	}
	return k.removePlaceholderSecret(ctx, name)
}

// removePlaceholderSecret deletes the placeholder Secret of the ModelConfig
// named mcName when model-manager created it live or its Kustomization left
// it behind; a Secret of anyone else's, one Flux applies from git, or none
// at all is left alone.
func (k *Kagent) removePlaceholderSecret(ctx context.Context, mcName string) error {
	secrets := k.dyn(ctx).Resource(secretGVR).Namespace(k.namespace)
	secretName := placeholderSecretName(mcName)
	sec, err := secrets.Get(ctx, secretName, metav1.GetOptions{})
	if err != nil || sec.GetLabels()[ManagedByLabel] != ManagedByValue {
		return nil
	}
	owner, err := gitops.Applying(ctx, k.dyn(ctx), sec)
	if err != nil || owner != nil {
		return err
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
	return retried(k, func() (*ModelConfigRef, error) { return k.lookup(ctx, b, model) })
}

func (k *Kagent) lookup(ctx context.Context, b backend.Name, model string) (*ModelConfigRef, error) {
	obj, err := k.find(ctx, b, model)
	if err != nil || obj == nil {
		return nil, err
	}
	return k.ref(ctx, obj)
}

// List implements Wirer.
func (k *Kagent) List(ctx context.Context) ([]ModelConfigRef, error) {
	return retried(k, func() ([]ModelConfigRef, error) { return k.list(ctx) })
}

func (k *Kagent) list(ctx context.Context) ([]ModelConfigRef, error) {
	list, err := k.dyn(ctx).Resource(k.resource()).Namespace(k.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: ManagedByLabel + "=" + ManagedByValue,
	})
	if err != nil {
		return nil, fmt.Errorf("list ModelConfigs in %s: %w", k.namespace, err)
	}
	items, err := k.withLegacyGitOps(ctx, list.Items, ManagedByLabel+"="+ManagedByValue)
	if err != nil {
		return nil, err
	}
	out := make([]ModelConfigRef, 0, len(items))
	for i := range items {
		ref, err := k.ref(ctx, &items[i])
		if err != nil {
			return nil, err
		}
		if ref.Model == "" {
			continue
		}
		out = append(out, *ref)
	}
	return out, nil
}

// ListAll implements Wirer.
func (k *Kagent) ListAll(ctx context.Context) ([]ModelConfigRef, error) {
	return retried(k, func() ([]ModelConfigRef, error) { return k.listAll(ctx) })
}

func (k *Kagent) listAll(ctx context.Context) ([]ModelConfigRef, error) {
	list, err := k.dyn(ctx).Resource(k.resource()).Namespace(k.namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list ModelConfigs in %s: %w", k.namespace, err)
	}
	items, err := k.withLegacyGitOps(ctx, list.Items, "")
	if err != nil {
		return nil, err
	}
	out := make([]ModelConfigRef, 0, len(items))
	for i := range items {
		ref, err := k.ref(ctx, &items[i])
		if err != nil {
			return nil, err
		}
		out = append(out, *ref)
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
// annotation first and the derived name second, in the version in use and
// then among the kagent.dev ones Flux applies (legacyGitOps). A ModelConfig
// without the backend label (written before the label existed) matches any
// backend; an empty b matches any label.
func (k *Kagent) find(ctx context.Context, b backend.Name, model string) (*unstructured.Unstructured, error) {
	selector := ManagedByLabel + "=" + ManagedByValue
	list, err := k.dyn(ctx).Resource(k.resource()).Namespace(k.namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, fmt.Errorf("list ModelConfigs in %s: %w", k.namespace, err)
	}
	if obj := k.match(list.Items, b, model); obj != nil {
		return obj, nil
	}
	legacy, err := k.legacyGitOps(ctx, selector)
	if err != nil {
		return nil, err
	}
	return k.match(legacy, b, model), nil
}

func (k *Kagent) match(items []unstructured.Unstructured, b backend.Name, model string) *unstructured.Unstructured {
	for i := range items {
		if items[i].GetAnnotations()[ModelAnnotation] == model && backendMatches(&items[i], b) {
			return &items[i]
		}
	}
	name := ModelConfigName(k.prefix, model)
	for i := range items {
		if items[i].GetName() == name && backendMatches(&items[i], b) {
			return &items[i]
		}
	}
	return nil
}

// legacyGitOps returns the kagent.dev ModelConfigs Flux applies from git
// while the wirer writes api.kagent.dev, matching selector. kagent 1.3 reads
// none of them, but the cut-over leaves them in place, and a live twin in
// api.kagent.dev would shadow a file in git: each is changed in git (mode
// commit), where the write moves it to api.kagent.dev under the same name.
// The kagent.dev CRD is read at the version in use, the one the API v2 line
// serves there; none when the wirer writes kagent.dev, the CRD is gone, or
// the client may not read it (a caller whose RBAC has api.kagent.dev only).
func (k *Kagent) legacyGitOps(ctx context.Context, selector string) ([]unstructured.Unstructured, error) {
	gvr := k.resource()
	if gvr.Group != KagentGroup {
		return nil, nil
	}
	gvr.Group = LegacyKagentGroup
	list, err := k.dyn(ctx).Resource(gvr).Namespace(k.namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	switch {
	case errors.IsNotFound(err), errors.IsForbidden(err):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("list %s ModelConfigs in %s: %w", LegacyKagentGroup, k.namespace, err)
	}
	var out []unstructured.Unstructured
	for _, item := range list.Items {
		if gitops.OwnerOf(item.GetLabels()) != nil {
			out = append(out, item)
		}
	}
	return out, nil
}

// withLegacyGitOps is items followed by the legacyGitOps ModelConfigs whose
// name items do not hold.
func (k *Kagent) withLegacyGitOps(ctx context.Context, items []unstructured.Unstructured, selector string) ([]unstructured.Unstructured, error) {
	legacy, err := k.legacyGitOps(ctx, selector)
	if err != nil {
		return nil, err
	}
	names := make(map[string]bool, len(items))
	for i := range items {
		names[items[i].GetName()] = true
	}
	for _, item := range legacy {
		if !names[item.GetName()] {
			items = append(items, item)
		}
	}
	return items, nil
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
		if ep.Think != nil {
			ollama[ollamaThink] = *ep.Think
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
	labels := map[string]any{ManagedByLabel: ManagedByValue, InstanceLabel: k.instance}
	if ep.Backend != "" {
		labels[BackendLabel] = string(ep.Backend)
	}
	gvr := k.resource()
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": gvr.Group + "/" + gvr.Version,
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

// ref is obj as reported, its GitOps owner held to the Kustomization's
// inventory (gitops.Applying) as the write tools hold it: a ModelConfig its
// Kustomization left behind is reported as written live.
func (k *Kagent) ref(ctx context.Context, obj *unstructured.Unstructured) (*ModelConfigRef, error) {
	ref := toRef(obj)
	owner, err := gitops.Applying(ctx, k.dyn(ctx), obj)
	if err != nil {
		return nil, err
	}
	ref.GitOps = owner
	return ref, nil
}

// toRef is obj as reported, without its GitOps owner (ref resolves it).
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
	ref.CreatedBy = obj.GetLabels()[InstanceLabel]
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
	if think, found, _ := unstructured.NestedBool(obj.Object, "spec", "ollama", ollamaThink); found {
		ref.Think = &think
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
