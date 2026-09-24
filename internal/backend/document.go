package backend

import (
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

// A backend document registers a backend at runtime: a ConfigMap in
// model-manager's namespace carrying DocumentLabel=true, whose DocumentKey
// holds a ModelBackend document. model-manager watches the label and
// enforces the schema below when it reads a document — one that fails it is
// reported (list_backends `invalid`) and not loaded. This is the contract
// cluster-manager and the portal's Add model backend page write against;
// docs/backends.md spells it out.
const (
	// DocumentLabel marks a ConfigMap as a backend document (value "true").
	DocumentLabel = "agent-platform.giantswarm.io/model-backend"
	// DocumentSelector selects the backend documents of a namespace.
	DocumentSelector = DocumentLabel + "=true"
	// DocumentSourceLabel repeats spec.source on the ConfigMap, so a plain
	// kubectl get shows who registered a backend.
	DocumentSourceLabel = "agent-platform.giantswarm.io/model-backend-source"
	// DocumentKey is the ConfigMap data key holding the document.
	DocumentKey = "backend.yaml"
	// DocumentAPIVersion and DocumentKind identify the document.
	DocumentAPIVersion = "agent-platform.giantswarm.io/v1alpha1"
	DocumentKind       = "ModelBackend"
	// DocumentNamePrefix: the ConfigMap of a backend is named
	// DocumentNamePrefix + kind, so there is one document per kind.
	DocumentNamePrefix = "model-backend-"

	// SourceStatic marks a backend configured by --backends; SourcePerson
	// and SourceClusterManager are the accepted spec.source values of a
	// registered document.
	SourceStatic         = "static"
	SourcePerson         = "person"
	SourceClusterManager = "cluster-manager"

	// TargetLocal names the cluster model-manager runs on.
	TargetLocal = "local"
)

// ErrNoBackend: the process runs with no backend; the fix is in the message.
var ErrNoBackend = errors.New("no backend registered: register one with add_backend (kind ollama|lmstudio|lemonade|kserve) or configure --backends")

// Document is the ModelBackend document.
type Document struct {
	APIVersion string       `json:"apiVersion"`
	Kind       string       `json:"kind"`
	Metadata   DocumentMeta `json:"metadata"`
	Spec       DocumentSpec `json:"spec"`
}

// DocumentMeta is the document's metadata; Name equals spec.kind.
type DocumentMeta struct {
	Name string `json:"name"`
}

// DocumentSpec describes one backend.
type DocumentSpec struct {
	// Kind is the driver: ollama | lmstudio | lemonade | kserve.
	Kind Name `json:"kind"`
	// Source is who wrote the document: person | cluster-manager.
	Source string `json:"source"`
	// Endpoint is the host backend's base URL as reached by model-manager
	// (required for ollama, lmstudio, lemonade; not accepted for kserve).
	Endpoint string `json:"endpoint,omitempty"`
	// AgentEndpoint is the host backend as reached by agent pods (optional;
	// defaults to Endpoint).
	AgentEndpoint string `json:"agentEndpoint,omitempty"`
	// Credentials references a Secret holding a token for the backend
	// (optional). Accepted for kserve (the Hugging Face hub token, a Secret
	// in the serving namespace); the host drivers do not present a bearer
	// yet and refuse it.
	Credentials *CredentialsRef `json:"credentials,omitempty"`
	// KServe carries the kserve backend's target and discovery (required
	// for kserve, not accepted otherwise).
	KServe *KServeSpec `json:"kserve,omitempty"`
}

// CredentialsRef names a Secret and the key holding the token.
type CredentialsRef struct {
	SecretRef SecretKeyRef `json:"secretRef"`
}

// SecretKeyRef names a Secret and a key in it.
type SecretKeyRef struct {
	Name string `json:"name"`
	// Key defaults to "token".
	Key string `json:"key,omitempty"`
}

// KServeSpec is the kserve backend's part of the document.
type KServeSpec struct {
	// Target is the cluster the backend acts on.
	Target Target `json:"target"`
	// Discovery locates the model-serving discovery ConfigMap (kind
	// ModelServingConfig) on the target: the slice release renders it into
	// its own namespace while model-manager reads its own, so a document
	// names it. Name defaults to agent-platform-model-serving; Namespace
	// defaults to the serving namespace.
	Discovery DiscoveryRef `json:"discovery"`
	// GPUPool, when set, replaces the discovery ConfigMap's spec.gpuPool:
	// the pool taint model-manager tolerates and the pool label it selects
	// on everything it schedules onto the pool.
	GPUPool *GPUPool `json:"gpuPool,omitempty"`
	// GPUPools are the cluster's GPU pools by name — the value of their
	// nodes' giantswarm.io/machine-pool label — each with the sizes it
	// launches, written by cluster-manager while the cluster has two or more
	// pools and so no gpuPool pins every predictor (giantswarm/cluster-manager#89).
	// The fit check judges a model against the pool whose size hosts it and
	// the predictor is pinned to that pool (giantswarm/model-manager#152).
	GPUPools map[string]GPUPool `json:"gpuPools,omitempty"`
	// Router, when set, is the router shape of every LLMInferenceService
	// the backend composes; see Router.
	Router *Router `json:"router,omitempty"`
}

// Router is the router shape of the LLMInferenceServices the kserve backend
// composes. Scheduler asks KServe for the llm-d endpoint picker beside the
// route: the model's HTTPRoute then targets an InferencePool, which the
// models Gateway resolves only with the Gateway API Inference Extension
// (giantswarm/agent-platform#504). Off — the default — KServe routes the
// Gateway to the workload Service, the shape a single-replica predictor
// needs. A preset's spec.router.scheduler overrides it for that preset.
type Router struct {
	Scheduler bool `json:"scheduler"`
}

// GPUPool is the scheduling of the GPU node pool the kserve backend puts
// work on: the pool's taint, tolerated by the inventory scan pods, the
// download Jobs and the predictors model-manager composes, and the pool's
// label as their node selector. It has the shape of the discovery
// ConfigMap's spec.gpuPool, which the platform chart renders from its
// modelServing.gpuPool values; a registered document's block replaces it.
type GPUPool struct {
	Taint        *Taint            `json:"taint,omitempty"`
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
	// Instances are the sizes the pool launches — the node as the provider
	// lists it and what it leaves a predictor — in the shape cluster-manager's
	// create_node_pool answer lists under `sizes`. Known, the fit check
	// judges a model against them while the pool has no node (a Karpenter
	// pool at scale-to-zero) instead of answering unverified, and a load no
	// size of the pool could host is refused before a predictor is created
	// (giantswarm/model-manager#97). Unknown, nothing changes.
	Instances []InstanceShape `json:"instances,omitempty"`
}

// InstanceShape is one size of a GPU pool: the node as the provider lists
// it, and what a predictor may request on it.
type InstanceShape struct {
	// InstanceType is the provider's name of the size (g6.xlarge); Size the
	// size within its family (xlarge), what the pool's `sizes` names.
	// Defaults to the part of InstanceType after its first dot.
	InstanceType string `json:"instanceType"`
	Size         string `json:"size,omitempty"`
	// VCPU and MemoryGiB are the node's nominal shape; GPUs its accelerators
	// and GPUMemoryGiB the memory of one of them.
	VCPU         int `json:"vcpu"`
	MemoryGiB    int `json:"memoryGiB"`
	GPUs         int `json:"gpus"`
	GPUMemoryGiB int `json:"gpuMemoryGiB"`
	// UsableVCPU and UsableMemoryGiB are what a predictor may request on a
	// node of this size once the kubelet's reservations and the fleet's
	// daemonsets have theirs (a g6.xlarge: 3 vCPU / 11.9 GiB of 4 / 16).
	UsableVCPU      float64 `json:"usableVcpu"`
	UsableMemoryGiB float64 `json:"usableMemoryGiB"`
}

// SizeName is the size a person knows the shape by: Size, else the part of
// InstanceType after its first dot (g6.xlarge → xlarge), else InstanceType.
func (s InstanceShape) SizeName() string {
	if s.Size != "" {
		return s.Size
	}
	if _, size, ok := strings.Cut(s.InstanceType, "."); ok && size != "" {
		return size
	}
	return s.InstanceType
}

// Validate checks the shape: an instance type and positive numbers.
func (s InstanceShape) Validate() error {
	if strings.TrimSpace(s.InstanceType) == "" {
		return errors.New("instanceType: required")
	}
	for _, f := range []struct {
		name  string
		value float64
	}{
		{"vcpu", float64(s.VCPU)}, {"memoryGiB", float64(s.MemoryGiB)},
		{"gpus", float64(s.GPUs)}, {"gpuMemoryGiB", float64(s.GPUMemoryGiB)},
		{"usableVcpu", s.UsableVCPU}, {"usableMemoryGiB", s.UsableMemoryGiB},
	} {
		if f.value <= 0 {
			return fmt.Errorf("%s: must be positive, got %v", f.name, f.value)
		}
	}
	return nil
}

// Validate checks the pool block: the taint and every instance shape.
// Errors name the field relative to the block (taint.key: required,
// instances[1].vcpu: must be positive).
func (p *GPUPool) Validate() error {
	if p == nil {
		return nil
	}
	if err := p.Taint.Validate(); err != nil {
		return fmt.Errorf("taint.%w", err)
	}
	return ValidateInstances(p.Instances)
}

// ValidateInstances checks a list of shapes, naming the failing entry.
func ValidateInstances(shapes []InstanceShape) error {
	for i, s := range shapes {
		if err := s.Validate(); err != nil {
			return fmt.Errorf("instances[%d].%w", i, err)
		}
	}
	return nil
}

// Taint is a node taint as a toleration input: an empty Value tolerates
// every value of Key (operator Exists), an empty Effect every effect.
type Taint struct {
	Key    string `json:"key"`
	Value  string `json:"value,omitempty"`
	Effect string `json:"effect,omitempty"`
}

// Validate checks the taint: a key, and a Kubernetes taint effect when set.
func (t *Taint) Validate() error {
	if t == nil {
		return nil
	}
	if t.Key == "" {
		return errors.New("key: required")
	}
	switch t.Effect {
	case "", "NoSchedule", "PreferNoSchedule", "NoExecute":
		return nil
	}
	return fmt.Errorf("effect: must be NoSchedule, PreferNoSchedule or NoExecute, got %q", t.Effect)
}

// DiscoveryRef locates a ConfigMap.
type DiscoveryRef struct {
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name,omitempty"`
}

// Target is the cluster a kserve backend acts on. Cluster "local" is the
// cluster model-manager runs on and needs no apiServer/caBundle; any other
// cluster names its apiserver and CA — never credentials: every call carries
// the caller's token, which the target apiserver must trust.
type Target struct {
	Cluster      string `json:"cluster"`
	Organization string `json:"organization,omitempty"`
	// APIServer is the target apiserver URL (https://...); empty for local.
	APIServer string `json:"apiServer,omitempty"`
	// CABundle is the target apiserver's CA, PEM (base64 not needed in YAML).
	CABundle string `json:"caBundle,omitempty"`
	// ServingNamespace is the namespace on the target holding the
	// LLMInferenceServices, Jobs and the cache.
	ServingNamespace string `json:"servingNamespace"`
}

// Local reports whether the target is the cluster model-manager runs on.
func (t Target) Local() bool { return t.Cluster == "" || t.Cluster == TargetLocal }

// Identity is the additive `target` clients see: cluster and organization
// only, never the apiserver.
func (t Target) Identity() *Target {
	if t.Local() {
		return nil
	}
	return &Target{Cluster: t.Cluster, Organization: t.Organization}
}

// DocumentName is the ConfigMap name of kind's document.
func DocumentName(kind Name) string { return DocumentNamePrefix + string(kind) }

// KnownKinds are the drivers a document may name, sorted.
func KnownKinds() []Name {
	return []Name{NameKServe, NameLemonade, NameLMStudio, NameOllama}
}

func isKnownKind(kind Name) bool {
	for _, k := range KnownKinds() {
		if k == kind {
			return true
		}
	}
	return false
}

func kindList() string {
	parts := make([]string, 0, 4)
	for _, k := range KnownKinds() {
		parts = append(parts, string(k))
	}
	sort.Strings(parts)
	return strings.Join(parts, "|")
}

// NewDocument returns a valid document skeleton for spec, with the
// apiVersion, kind and name filled and the defaults applied.
func NewDocument(spec DocumentSpec) *Document {
	d := &Document{APIVersion: DocumentAPIVersion, Kind: DocumentKind, Metadata: DocumentMeta{Name: string(spec.Kind)}, Spec: spec}
	d.applyDefaults()
	return d
}

func (d *Document) applyDefaults() {
	if d.Spec.Source == "" {
		d.Spec.Source = SourcePerson
	}
	if d.Spec.Credentials != nil && d.Spec.Credentials.SecretRef.Key == "" {
		d.Spec.Credentials.SecretRef.Key = "token"
	}
	if d.Spec.KServe != nil {
		if d.Spec.KServe.Target.Cluster == "" {
			d.Spec.KServe.Target.Cluster = TargetLocal
		}
		if d.Spec.KServe.Discovery.Name == "" {
			d.Spec.KServe.Discovery.Name = "agent-platform-model-serving"
		}
		if d.Spec.KServe.Discovery.Namespace == "" {
			d.Spec.KServe.Discovery.Namespace = d.Spec.KServe.Target.ServingNamespace
		}
	}
}

// ParseDocument decodes and validates raw. Errors name the failing field
// (`spec.kind: required (...)`), so a report of an invalid document says
// what to fix.
func ParseDocument(raw []byte) (*Document, error) {
	var d Document
	if err := yaml.UnmarshalStrict(raw, &d); err != nil {
		return nil, fmt.Errorf("document: %w", err)
	}
	d.applyDefaults()
	if err := d.Validate(); err != nil {
		return nil, err
	}
	return &d, nil
}

// Validate is the read-time schema.
func (d *Document) Validate() error {
	s := &d.Spec
	switch {
	case d.APIVersion != DocumentAPIVersion:
		return fmt.Errorf("apiVersion: must be %s", DocumentAPIVersion)
	case d.Kind != DocumentKind:
		return fmt.Errorf("kind: must be %s", DocumentKind)
	case s.Kind == "":
		return fmt.Errorf("spec.kind: required (%s)", kindList())
	case !isKnownKind(s.Kind):
		return fmt.Errorf("spec.kind: unknown %q (%s)", s.Kind, kindList())
	case d.Metadata.Name != "" && d.Metadata.Name != string(s.Kind):
		return fmt.Errorf("metadata.name: must equal spec.kind (%s)", s.Kind)
	case s.Source != SourcePerson && s.Source != SourceClusterManager:
		return fmt.Errorf("spec.source: must be %s or %s (static backends come from --backends)", SourcePerson, SourceClusterManager)
	}
	if s.Credentials != nil && s.Credentials.SecretRef.Name == "" {
		return errors.New("spec.credentials.secretRef.name: required when credentials are set")
	}
	if s.Kind == NameKServe {
		return s.validateKServe()
	}
	return s.validateHost()
}

func (s *DocumentSpec) validateHost() error {
	if s.KServe != nil {
		return fmt.Errorf("spec.kserve: not accepted for %s", s.Kind)
	}
	if s.Credentials != nil {
		return fmt.Errorf("spec.credentials: not supported by %s yet (the driver presents no bearer)", s.Kind)
	}
	if s.Endpoint == "" {
		return fmt.Errorf("spec.endpoint: required for %s", s.Kind)
	}
	if err := checkURL(s.Endpoint); err != nil {
		return fmt.Errorf("spec.endpoint: %w", err)
	}
	if s.AgentEndpoint != "" {
		if err := checkURL(s.AgentEndpoint); err != nil {
			return fmt.Errorf("spec.agentEndpoint: %w", err)
		}
	}
	return nil
}

func (s *DocumentSpec) validateKServe() error {
	if s.Endpoint != "" || s.AgentEndpoint != "" {
		return errors.New("spec.endpoint: not accepted for kserve (the target names the cluster)")
	}
	if s.KServe == nil {
		return errors.New("spec.kserve: required for kserve")
	}
	t := s.KServe.Target
	if t.ServingNamespace == "" {
		return errors.New("spec.kserve.target.servingNamespace: required")
	}
	if err := s.KServe.GPUPool.Validate(); err != nil {
		return fmt.Errorf("spec.kserve.gpuPool.%w", err)
	}
	for name, pool := range s.KServe.GPUPools {
		if name == "" {
			return errors.New("spec.kserve.gpuPools: a pool without a name")
		}
		if err := ValidateInstances(pool.Instances); err != nil {
			return fmt.Errorf("spec.kserve.gpuPools.%s.%w", name, err)
		}
	}
	if t.Local() {
		if t.APIServer != "" || t.CABundle != "" {
			return errors.New("spec.kserve.target.apiServer: not accepted for the local cluster")
		}
		return nil
	}
	if t.APIServer == "" {
		return fmt.Errorf("spec.kserve.target.apiServer: required for cluster %q (only %s needs none)", t.Cluster, TargetLocal)
	}
	if err := checkURL(t.APIServer); err != nil {
		return fmt.Errorf("spec.kserve.target.apiServer: %w", err)
	}
	if t.CABundle == "" {
		return fmt.Errorf("spec.kserve.target.caBundle: required for cluster %q", t.Cluster)
	}
	return nil
}

func checkURL(s string) error {
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("must be an http(s) URL, got %q", s)
	}
	return nil
}

// Render returns the document as YAML.
func (d *Document) Render() ([]byte, error) {
	return yaml.Marshal(d)
}

// ConfigMap renders the document as the ConfigMap add_backend writes.
func (d *Document) ConfigMap(namespace string) (*corev1.ConfigMap, error) {
	raw, err := d.Render()
	if err != nil {
		return nil, err
	}
	return &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      DocumentName(d.Spec.Kind),
			Namespace: namespace,
			Labels: map[string]string{
				DocumentLabel:       "true",
				DocumentSourceLabel: d.Spec.Source,
			},
		},
		Data: map[string]string{DocumentKey: string(raw)},
	}, nil
}

// Options applies the document onto a copy of base: the driver block of the
// document's kind is replaced by what the document says, every other field
// (images, timeouts, Ollama's context length and think, the Kubernetes clients) keeps
// base's value.
func (d *Document) Options(base Options) Options {
	s := d.Spec
	switch s.Kind {
	case NameOllama:
		base.Ollama = OllamaOptions{Endpoint: s.Endpoint, AgentHost: s.AgentEndpoint, Timeout: base.Ollama.Timeout, ContextLength: base.Ollama.ContextLength, Think: base.Ollama.Think}
	case NameLMStudio:
		base.LMStudio = LMStudioOptions{Endpoint: s.Endpoint, AgentHost: s.AgentEndpoint, Timeout: base.LMStudio.Timeout, LoadTimeout: base.LMStudio.LoadTimeout}
	case NameLemonade:
		base.Lemonade = LemonadeOptions{Endpoint: s.Endpoint, AgentHost: s.AgentEndpoint, Timeout: base.Lemonade.Timeout, LoadTimeout: base.Lemonade.LoadTimeout}
	case NameKServe:
		k := s.KServe
		base.KServe.Target = k.Target
		base.KServe.Namespace = k.Target.ServingNamespace
		base.KServe.DiscoveryNamespace = k.Discovery.Namespace
		base.KServe.DiscoveryConfigMap = k.Discovery.Name
		if base.KServe.PresetNamespace == "" {
			base.KServe.PresetNamespace = k.Discovery.Namespace
		}
		if k.GPUPool != nil {
			base.KServe.GPUPool = *k.GPUPool
		}
		base.KServe.GPUPools = k.GPUPools
		if k.Router != nil {
			base.KServe.Router = *k.Router
		}
		if s.Credentials != nil {
			base.KServe.HFTokenSecret = s.Credentials.SecretRef.Name
			base.KServe.HFTokenSecretKey = s.Credentials.SecretRef.Key
		}
	}
	return base
}
