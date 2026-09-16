package kserve

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Labels and annotations model-manager puts on the objects it creates, plus
// the KServe label that links predictor pods to their InferenceService.
const (
	ManagedByLabel  = "app.kubernetes.io/managed-by"
	ManagedByValue  = "model-manager"
	BackendLabel    = "model-manager.giantswarm.io/backend"
	ComponentLabel  = "model-manager.giantswarm.io/component"
	ModelAnnotation = "model-manager.giantswarm.io/model"

	isvcPodLabel = "serving.kserve.io/inferenceservice"

	statusReady    = "Ready"
	statusNotReady = "NotReady"
	statusPending  = "Pending"
)

var isvcGVR = schema.GroupVersionResource{Group: "serving.kserve.io", Version: "v1beta1", Resource: "inferenceservices"}

// served is the driver's view of one InferenceService.
type served struct {
	// Kind is ServingKindLLM or ServingKindClassic.
	Kind      string
	Name      string
	Namespace string
	Model     string
	Preset    string
	Managed   bool
	ManagedBy string
	// PresetLabelled is true when the object carries the preset label (set
	// by model-manager and by the portal's serve flow); a preset inferred from
	// the name alone does not make an InferenceService manageable.
	PresetLabelled bool
	StorageURI     string
	Runtime        string
	GPUs           int64
	Ready          bool
	Status         string
	Message        string
	URL            string
	Node           string
	Created        time.Time
	Deleting       bool
}

// manageable reports whether model-manager may operate on the
// InferenceService: the ones it created and the ones the portal's serve flow
// created from a preset (label agent-platform.giantswarm.io/preset). Anything
// else in the serving namespace is inventory only.
func (sv served) manageable() bool {
	return sv.Managed || sv.PresetLabelled
}

// servedName is the model name vLLM answers under: the object's name for a
// classic InferenceService (the ClusterServingRuntime passes
// --served-model-name {{.Name}}), spec.model.name for an LLMInferenceService
// (the well-known template passes the spec's model name).
func (sv served) servedName() string {
	if sv.Kind == ServingKindLLM {
		return sv.Model
	}
	return sv.Name
}

// predictorURL is the in-cluster URL KServe gives a raw-deployment predictor
// (Service <name>-predictor, port 80).
func predictorURL(name, namespace string) string {
	return fmt.Sprintf("http://%s-predictor.%s.svc.cluster.local", name, namespace)
}

// normalizePredictorURL fixes the scheme of the address KServe publishes for a
// predictor. In raw-deployment mode the controller writes status.address.url
// with the ingress urlScheme — https wherever the external route is
// TLS-terminated — although the predictor Service itself speaks plain HTTP on
// port 80. A cluster-local host without an explicit port therefore always gets
// the http scheme; external hosts and explicit ports are kept as published.
// Trailing slashes are dropped.
func normalizePredictorURL(raw string) string {
	raw = strings.TrimRight(raw, "/")
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	if u.Scheme == "https" && u.Port() == "" && isClusterLocalHost(u.Hostname()) {
		u.Scheme = "http"
		return strings.TrimRight(u.String(), "/")
	}
	return raw
}

// isClusterLocalHost reports whether a host is a Kubernetes Service DNS name
// (<svc>.<ns>.svc or <svc>.<ns>.svc.<cluster domain>).
func isClusterLocalHost(host string) bool {
	return strings.HasSuffix(host, ".svc") || strings.Contains(host, ".svc.")
}

// listServed lists the InferenceServices of the serving namespace with the
// node their predictor runs on.
func (b *Backend) listServed(ctx context.Context) ([]served, error) {
	s := b.cfg.settings(ctx)
	var items []unstructured.Unstructured
	for _, kind := range s.kinds() {
		list, err := b.dynamic(ctx).Resource(gvrFor(kind)).Namespace(s.Namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("list %ss in %s: %w", kind, s.Namespace, err)
		}
		items = append(items, list.Items...)
	}
	presets, _, err := b.presets(ctx)
	if err != nil {
		b.log.Warn("listing presets failed; serving objects are shown without preset details", "error", err)
	}
	idx := indexPresets(presets)
	nodes := b.predictorNodes(ctx, s.Namespace)
	out := make([]served, 0, len(items))
	for i := range items {
		sv := parseServed(&items[i], idx, s.GPUResourceName)
		sv.Node = nodes[sv.Name]
		out = append(out, sv)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	b.rememberServed(out)
	// While they exist, remember which repository each one fills its cache
	// directory from (index.go).
	b.recordServed(ctx, out)
	return out, nil
}

// predictorNodes maps the name of an InferenceService or LLMInferenceService
// to the node its predictor (workload) pod runs on.
func (b *Backend) predictorNodes(ctx context.Context, namespace string) map[string]string {
	out := map[string]string{}
	b.podNodes(ctx, namespace, isvcPodLabel, isvcPodLabel, out)
	b.podNodes(ctx, namespace, llmisvcPodSelector, llmisvcPodLabel, out)
	return out
}

// podNodes adds the node of every pod matching selector to out, keyed by the
// pod's nameLabel value.
func (b *Backend) podNodes(ctx context.Context, namespace, selector, nameLabel string, out map[string]string) {
	pods, err := b.k8s(ctx).CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		b.log.Warn("listing predictor pods failed", "namespace", namespace, "selector", selector, "error", err)
		return
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		name := p.Labels[nameLabel]
		if name == "" || p.Spec.NodeName == "" {
			continue
		}
		// Prefer a running pod over a terminating one.
		if prev, ok := out[name]; ok && prev != "" && p.DeletionTimestamp != nil {
			continue
		}
		out[name] = p.Spec.NodeName
	}
}

// parseServed reads the fields the driver needs from an InferenceService or
// an LLMInferenceService.
func parseServed(obj *unstructured.Unstructured, idx presetIndex, gpuResource string) served {
	sv := served{
		Kind:           ServingKindClassic,
		Name:           obj.GetName(),
		Namespace:      obj.GetNamespace(),
		Managed:        obj.GetLabels()[ManagedByLabel] == ManagedByValue,
		ManagedBy:      obj.GetLabels()[ManagedByLabel],
		Preset:         obj.GetLabels()[PresetLabel],
		PresetLabelled: obj.GetLabels()[PresetLabel] != "",
		Created:        obj.GetCreationTimestamp().Time,
		Deleting:       obj.GetDeletionTimestamp() != nil,
	}
	if obj.GetKind() == ServingKindLLM {
		sv.Kind = ServingKindLLM
		sv.StorageURI, _, _ = unstructured.NestedString(obj.Object, "spec", "model", "uri")
		if main := mainContainer(obj); main != nil {
			sv.GPUs = gpusOf(main["resources"], gpuResource)
		}
	} else {
		sv.StorageURI, _, _ = unstructured.NestedString(obj.Object, "spec", "predictor", "model", "storageUri")
		sv.Runtime, _, _ = unstructured.NestedString(obj.Object, "spec", "predictor", "model", "runtime")
		model, _, _ := unstructured.NestedMap(obj.Object, "spec", "predictor", "model")
		sv.GPUs = gpusOf(model["resources"], gpuResource)
	}

	// Model id: the annotation, the LLMInferenceService's model name, the
	// preset, then the hf:// storage URI.
	sv.Model = obj.GetAnnotations()[ModelAnnotation]
	if sv.Model == "" && sv.Kind == ServingKindLLM {
		sv.Model, _, _ = unstructured.NestedString(obj.Object, "spec", "model", "name")
	}
	if p, ok := idx.byName[sv.Preset]; ok && sv.Preset != "" {
		if sv.Model == "" {
			sv.Model = p.Spec.Model.ID
		}
	} else if p, ok := idx.byName[sv.Name]; ok && sv.Preset == "" && sv.Model == "" {
		sv.Preset = p.name()
		sv.Model = p.Spec.Model.ID
	}
	if sv.Model == "" && strings.HasPrefix(sv.StorageURI, "hf://") {
		sv.Model, _ = splitRevision(sv.StorageURI)
	}
	if sv.Model == "" {
		sv.Model = sv.Name
	}

	sv.Status, sv.Message, sv.Ready = servedStatus(obj)
	sv.URL = normalizePredictorURL(servedURL(obj, sv))
	return sv
}

// servedURL is the address KServe published for the object — status.address
// (classic), status.url, the first of status.addresses (llmisvc) — else the
// kind's default.
func servedURL(obj *unstructured.Unstructured, sv served) string {
	if u, _, _ := unstructured.NestedString(obj.Object, "status", "address", "url"); u != "" {
		return u
	}
	if u, _, _ := unstructured.NestedString(obj.Object, "status", "url"); u != "" {
		return u
	}
	if addrs, _, _ := unstructured.NestedSlice(obj.Object, "status", "addresses"); len(addrs) > 0 {
		if first, ok := addrs[0].(map[string]any); ok {
			if u, _ := first["url"].(string); u != "" {
				return u
			}
		}
	}
	return sv.defaultURL()
}

// defaultURL is the in-cluster Service the kind gets: the workload Service
// of an LLMInferenceService, the predictor Service of an InferenceService.
func (sv served) defaultURL() string {
	if sv.Kind == ServingKindLLM {
		return workloadURL(sv.Name, sv.Namespace)
	}
	return predictorURL(sv.Name, sv.Namespace)
}

// gpusOf reads the accelerator count from a container's resources: requests,
// else limits.
func gpusOf(resources any, gpuResource string) int64 {
	res, _ := resources.(map[string]any)
	if req, ok := res["requests"].(map[string]any); ok {
		if n := quantityValue(req[gpuResource]); n > 0 {
			return n
		}
	}
	if lim, ok := res["limits"].(map[string]any); ok {
		return quantityValue(lim[gpuResource])
	}
	return 0
}

// servedStatus maps the KServe conditions / modelStatus to Ready, NotReady
// (with a message) or Pending.
func servedStatus(obj *unstructured.Unstructured) (status, message string, ready bool) {
	conds, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	var readyCond map[string]any
	for _, c := range conds {
		cm, ok := c.(map[string]any)
		if ok && cm["type"] == "Ready" {
			readyCond = cm
		}
	}
	if readyCond != nil && readyCond["status"] == "True" {
		return statusReady, "", true
	}
	if failure, ok, _ := unstructured.NestedMap(obj.Object, "status", "modelStatus", "lastFailureInfo"); ok && len(failure) > 0 {
		msg, _ := failure["message"].(string)
		reason, _ := failure["reason"].(string)
		return statusNotReady, strings.TrimSpace(reason + " " + msg), false
	}
	if readyCond != nil {
		msg, _ := readyCond["message"].(string)
		reason, _ := readyCond["reason"].(string)
		if readyCond["status"] == "False" {
			return statusNotReady, strings.TrimSpace(reason + " " + msg), false
		}
		return statusPending, strings.TrimSpace(reason + " " + msg), false
	}
	if ts, _, _ := unstructured.NestedString(obj.Object, "status", "modelStatus", "transitionStatus"); ts != "" && ts != "UpToDate" && ts != "InProgress" {
		return statusNotReady, ts, false
	}
	return statusPending, "", false
}

func quantityValue(v any) int64 {
	switch q := v.(type) {
	case string:
		n, err := strconv.ParseInt(q, 10, 64)
		if err != nil {
			return 0
		}
		return n
	case int64:
		return q
	case float64:
		return int64(q)
	case int:
		return int64(q)
	}
	return 0
}

// compose builds the serving object for a preset in the configured kind.
func (b *Backend) compose(p *servingPreset, s settings, node string) *unstructured.Unstructured {
	if s.ServingKind == ServingKindLLM {
		return b.composeLLM(p, s, node)
	}
	return b.composeClassic(p, s, node)
}

// composeClassic builds the InferenceService for a preset following the
// modelServing contract's composition recipe (agent-platform-connectivity,
// templates/model-serving/): predictor.model from the preset, defaults from
// discovery, scheduling merged, chat template mounted, spec.predictor extras
// copied on top verbatim.
func (b *Backend) composeClassic(p *servingPreset, s settings, node string) *unstructured.Unstructured {
	model := map[string]any{
		"modelFormat": map[string]any{"name": p.Spec.Model.Format},
		"storageUri":  p.Spec.Model.StorageURI,
	}
	runtime := p.Spec.Runtime
	if runtime == "" {
		runtime = s.Runtime
	}
	if runtime != "" {
		model["runtime"] = runtime
	}
	if len(p.Spec.Args) > 0 {
		model["args"] = toAnySlice(p.Spec.Args)
	}
	if len(p.Spec.Env) > 0 {
		model["env"] = mapsToAny(p.Spec.Env)
	}
	if res := p.resources(s); len(res) > 0 {
		model["resources"] = res
	}

	predictor := map[string]any{}
	if mount, volume := p.chatTemplateMount(); mount != nil {
		model["volumeMounts"] = []any{mount}
		predictor["volumes"] = []any{volume}
	}
	predictor["model"] = model
	if ns := p.nodeSelector(s, node); len(ns) > 0 {
		predictor["nodeSelector"] = ns
	}
	if tols := p.tolerations(s); len(tols) > 0 {
		predictor["tolerations"] = tols
	}
	if s.RuntimeClassName != "" {
		predictor["runtimeClassName"] = s.RuntimeClassName
	}
	if s.DeploymentStrategyType != "" {
		predictor["deploymentStrategy"] = map[string]any{"type": s.DeploymentStrategyType}
	}
	if s.TimeoutSeconds > 0 {
		predictor["timeout"] = s.TimeoutSeconds
	}
	for k, v := range p.Spec.Predictor {
		predictor[k] = v
	}

	obj := newServingObject(ServingKindClassic, p, s.Namespace)
	obj.Object["spec"] = map[string]any{"predictor": predictor}
	return obj
}

// newServingObject is the metadata both kinds share: the preset's name, the
// labels that make the object model-manager's and link it to its preset, the
// model annotation.
func newServingObject(kind string, p *servingPreset, namespace string) *unstructured.Unstructured {
	gvr := gvrFor(kind)
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": gvr.Group + "/" + gvr.Version,
		"kind":       kind,
		"metadata": map[string]any{
			"name":      p.name(),
			"namespace": namespace,
			"labels": map[string]any{
				ManagedByLabel:                          ManagedByValue,
				BackendLabel:                            "kserve",
				PresetLabel:                             p.name(),
				"app.kubernetes.io/name":                p.name(),
				"app.kubernetes.io/component":           "inference",
				"app.kubernetes.io/part-of":             "agent-platform",
				"model-manager.giantswarm.io/model-dir": p.name(),
			},
			"annotations": map[string]any{
				ModelAnnotation: p.Spec.Model.ID,
			},
		},
	}}
}

// resources is the preset's requests/limits with the accelerator count
// added under the discovery's resource name; empty when there is nothing.
func (p *servingPreset) resources(s settings) map[string]any {
	requests := copyResourceMap(p.Spec.Resources.Requests)
	limits := copyResourceMap(p.Spec.Resources.Limits)
	if gpus := p.gpus(); gpus > 0 && s.GPUResourceName != "" {
		requests[s.GPUResourceName] = strconv.FormatInt(gpus, 10)
		limits[s.GPUResourceName] = strconv.FormatInt(gpus, 10)
	}
	resources := map[string]any{}
	if len(requests) > 0 {
		resources["requests"] = requests
	}
	if len(limits) > 0 {
		resources["limits"] = limits
	}
	return resources
}

// nodeSelector merges the discovery's serving selector, the GPU pool's
// label, the preset's selector and the node pin, each overriding the one
// before; empty when there is nothing.
func (p *servingPreset) nodeSelector(s settings, node string) map[string]any {
	out := map[string]any{}
	for k, v := range s.NodeSelector {
		out[k] = v
	}
	for k, v := range s.GPUPool.NodeSelector {
		out[k] = v
	}
	for k, v := range p.Spec.Scheduling.NodeSelector {
		out[k] = v
	}
	if node != "" {
		out[labelHostname] = node
	}
	return out
}

// chatTemplateMount is the volume mount and volume for the preset's chat
// template ConfigMap; nil, nil without one.
func (p *servingPreset) chatTemplateMount() (mount, volume map[string]any) {
	ct := p.Spec.ChatTemplate
	if ct == nil || ct.ConfigMap == "" {
		return nil, nil
	}
	mountPath := ct.MountPath
	if mountPath == "" {
		mountPath = "/mnt/chat-template"
	}
	return map[string]any{"name": "chat-template", "mountPath": mountPath, "readOnly": true},
		map[string]any{"name": "chat-template", "configMap": map[string]any{"name": ct.ConfigMap}}
}

// getServing fetches the serving object of the given kind; nil when there
// is none.
func (b *Backend) getServing(ctx context.Context, kind, namespace, name string) (*unstructured.Unstructured, error) {
	obj, err := b.dynamic(ctx).Resource(gvrFor(kind)).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get %s %s/%s: %w", kind, namespace, name, err)
	}
	return obj, nil
}

// findServing looks a name up in every kind the cluster serves, the
// configured kind first.
func (b *Backend) findServing(ctx context.Context, s settings, name string) (*unstructured.Unstructured, error) {
	for _, kind := range s.kinds() {
		obj, err := b.getServing(ctx, kind, s.Namespace, name)
		if err != nil || obj != nil {
			return obj, err
		}
	}
	return nil, nil
}

func (b *Backend) createServing(ctx context.Context, obj *unstructured.Unstructured) error {
	if _, err := b.dynamic(ctx).Resource(gvrFor(obj.GetKind())).Namespace(obj.GetNamespace()).Create(ctx, obj, metav1.CreateOptions{FieldManager: ManagedByValue}); err != nil {
		return fmt.Errorf("create %s %s/%s: %w", obj.GetKind(), obj.GetNamespace(), obj.GetName(), err)
	}
	return nil
}

func (b *Backend) deleteServing(ctx context.Context, kind, namespace, name string) error {
	propagation := metav1.DeletePropagationForeground
	err := b.dynamic(ctx).Resource(gvrFor(kind)).Namespace(namespace).Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &propagation})
	if err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("delete %s %s/%s: %w", kind, namespace, name, err)
	}
	return nil
}

// kinds lists the serving kinds to read, the configured one first: the
// classic InferenceService always, the LLMInferenceService where its API is
// served.
func (s settings) kinds() []string {
	if !s.LLMServed && s.ServingKind != ServingKindLLM {
		return []string{ServingKindClassic}
	}
	if s.ServingKind == ServingKindLLM {
		return []string{ServingKindLLM, ServingKindClassic}
	}
	return []string{ServingKindClassic, ServingKindLLM}
}

func toAnySlice(in []string) []any {
	out := make([]any, 0, len(in))
	for _, s := range in {
		out = append(out, s)
	}
	return out
}

func mapsToAny(in []map[string]any) []any {
	out := make([]any, 0, len(in))
	for _, m := range in {
		out = append(out, m)
	}
	return out
}

// copyResourceMap copies a requests/limits map, stringifying numbers so the
// API server's quantity parser accepts them.
func copyResourceMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in)+1)
	for k, v := range in {
		switch n := v.(type) {
		case float64:
			if n == float64(int64(n)) {
				out[k] = strconv.FormatInt(int64(n), 10)
			} else {
				out[k] = strconv.FormatFloat(n, 'f', -1, 64)
			}
		case int64:
			out[k] = strconv.FormatInt(n, 10)
		case int:
			out[k] = strconv.Itoa(n)
		default:
			out[k] = v
		}
	}
	return out
}
