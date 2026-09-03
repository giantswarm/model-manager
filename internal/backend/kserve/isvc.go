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
	list, err := b.dynamic(ctx).Resource(isvcGVR).Namespace(s.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list InferenceServices in %s: %w", s.Namespace, err)
	}
	presets, _, err := b.presets(ctx)
	if err != nil {
		b.log.Warn("listing presets failed; InferenceServices are shown without preset details", "error", err)
	}
	idx := indexPresets(presets)
	nodes := b.predictorNodes(ctx, s.Namespace)
	out := make([]served, 0, len(list.Items))
	for i := range list.Items {
		sv := parseServed(&list.Items[i], idx, s.GPUResourceName)
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

// predictorNodes maps InferenceService name to the node of its predictor pod.
func (b *Backend) predictorNodes(ctx context.Context, namespace string) map[string]string {
	out := map[string]string{}
	pods, err := b.k8s(ctx).CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: isvcPodLabel})
	if err != nil {
		b.log.Warn("listing predictor pods failed", "namespace", namespace, "error", err)
		return out
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		name := p.Labels[isvcPodLabel]
		if name == "" || p.Spec.NodeName == "" {
			continue
		}
		// Prefer a running pod over a terminating one.
		if prev, ok := out[name]; ok && prev != "" && p.DeletionTimestamp != nil {
			continue
		}
		out[name] = p.Spec.NodeName
	}
	return out
}

// parseServed reads the fields the driver needs from an InferenceService.
func parseServed(obj *unstructured.Unstructured, idx presetIndex, gpuResource string) served {
	sv := served{
		Name:           obj.GetName(),
		Namespace:      obj.GetNamespace(),
		Managed:        obj.GetLabels()[ManagedByLabel] == ManagedByValue,
		ManagedBy:      obj.GetLabels()[ManagedByLabel],
		Preset:         obj.GetLabels()[PresetLabel],
		PresetLabelled: obj.GetLabels()[PresetLabel] != "",
		Created:        obj.GetCreationTimestamp().Time,
		Deleting:       obj.GetDeletionTimestamp() != nil,
	}
	sv.StorageURI, _, _ = unstructured.NestedString(obj.Object, "spec", "predictor", "model", "storageUri")
	sv.Runtime, _, _ = unstructured.NestedString(obj.Object, "spec", "predictor", "model", "runtime")
	if req, ok, _ := unstructured.NestedMap(obj.Object, "spec", "predictor", "model", "resources", "requests"); ok {
		sv.GPUs = quantityValue(req[gpuResource])
	}
	if lim, ok, _ := unstructured.NestedMap(obj.Object, "spec", "predictor", "model", "resources", "limits"); ok && sv.GPUs == 0 {
		sv.GPUs = quantityValue(lim[gpuResource])
	}

	// Model id: the annotation, the preset, then the hf:// storage URI.
	sv.Model = obj.GetAnnotations()[ModelAnnotation]
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
	if u, _, _ := unstructured.NestedString(obj.Object, "status", "address", "url"); u != "" {
		sv.URL = u
	} else if u, _, _ := unstructured.NestedString(obj.Object, "status", "url"); u != "" {
		sv.URL = u
	} else {
		sv.URL = predictorURL(sv.Name, sv.Namespace)
	}
	sv.URL = normalizePredictorURL(sv.URL)
	return sv
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

// compose builds the InferenceService for a preset following the modelServing
// contract's composition recipe (agent-platform-standalone README, "Model
// serving"): predictor.model from the preset, defaults from discovery,
// scheduling merged, chat template mounted, spec.predictor extras copied on
// top verbatim.
func (b *Backend) compose(p *servingPreset, s settings, node string) *unstructured.Unstructured {
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
	if len(resources) > 0 {
		model["resources"] = resources
	}

	predictor := map[string]any{}
	if ct := p.Spec.ChatTemplate; ct != nil && ct.ConfigMap != "" {
		mountPath := ct.MountPath
		if mountPath == "" {
			mountPath = "/mnt/chat-template"
		}
		model["volumeMounts"] = []any{map[string]any{"name": "chat-template", "mountPath": mountPath, "readOnly": true}}
		predictor["volumes"] = []any{map[string]any{"name": "chat-template", "configMap": map[string]any{"name": ct.ConfigMap}}}
	}
	predictor["model"] = model

	nodeSelector := map[string]any{}
	for k, v := range s.NodeSelector {
		nodeSelector[k] = v
	}
	for k, v := range p.Spec.Scheduling.NodeSelector {
		nodeSelector[k] = v
	}
	if node != "" {
		nodeSelector[labelHostname] = node
	}
	if len(nodeSelector) > 0 {
		predictor["nodeSelector"] = nodeSelector
	}
	if len(p.Spec.Scheduling.Tolerations) > 0 {
		predictor["tolerations"] = mapsToAny(p.Spec.Scheduling.Tolerations)
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

	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": isvcGVR.Group + "/" + isvcGVR.Version,
		"kind":       "InferenceService",
		"metadata": map[string]any{
			"name":      p.name(),
			"namespace": s.Namespace,
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
		"spec": map[string]any{"predictor": predictor},
	}}
	return obj
}

func (b *Backend) getISVC(ctx context.Context, namespace, name string) (*unstructured.Unstructured, error) {
	obj, err := b.dynamic(ctx).Resource(isvcGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get InferenceService %s/%s: %w", namespace, name, err)
	}
	return obj, nil
}

func (b *Backend) createISVC(ctx context.Context, obj *unstructured.Unstructured) error {
	if _, err := b.dynamic(ctx).Resource(isvcGVR).Namespace(obj.GetNamespace()).Create(ctx, obj, metav1.CreateOptions{FieldManager: ManagedByValue}); err != nil {
		return fmt.Errorf("create InferenceService %s/%s: %w", obj.GetNamespace(), obj.GetName(), err)
	}
	return nil
}

func (b *Backend) deleteISVC(ctx context.Context, namespace, name string) error {
	propagation := metav1.DeletePropagationForeground
	err := b.dynamic(ctx).Resource(isvcGVR).Namespace(namespace).Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &propagation})
	if err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("delete InferenceService %s/%s: %w", namespace, name, err)
	}
	return nil
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
