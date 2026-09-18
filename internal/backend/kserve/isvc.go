package kserve

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/giantswarm/model-manager/internal/backend"
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
	// statusTerminating is what a deleting object answers; the phase says
	// the same and the list shows the object until it is gone.
	statusTerminating = "Terminating"
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
	// Reason names why Status is not Ready: the Ready condition's reason, a
	// failed load's, or the predictor pod's (Unschedulable, ImagePullBackOff).
	Reason   string
	Message  string
	URL      string
	Node     string
	Created  time.Time
	Deleting bool
	// ReadyAt is when the Ready condition last turned True; Failed says the
	// object recorded a failed load (modelStatus.lastFailureInfo).
	ReadyAt time.Time
	Failed  bool
	// Phase and Steps are where the serve is (phases.go).
	Phase string
	Steps []backend.Step
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

// routed reports whether the address KServe published for the model is its
// route on the models Gateway rather than its in-cluster Service: a host that
// is no Service DNS name. The Gateway admits a person's token and nothing
// else, so agents reach a routed model with the caller's own token
// (apiKeyPassthrough); the keyless in-cluster Service is reached with
// kagent's placeholder key.
func (sv served) routed() bool {
	u, err := url.Parse(sv.URL)
	return err == nil && u.Host != "" && !isClusterLocalHost(u.Hostname())
}

// expectedURL is the address the object gets before KServe has published
// one: its route on the models Gateway — <gateway>/<namespace>/<name>, the
// path KServe renders for every LLMInferenceService attached to it — when
// discovery names the Gateway, else the kind's in-cluster Service. Known at
// compose time, so the ModelConfig a load wires points where the model will
// answer and carries the token shape the Gateway demands
// (giantswarm/model-manager#115).
func (sv served) expectedURL(gateway string) string {
	if gateway != "" && sv.Kind == ServingKindLLM {
		return gateway + "/" + sv.Namespace + "/" + sv.Name
	}
	return sv.defaultURL()
}

// agentEndpoint is how kagent reaches the served model: its OpenAI-compatible
// API at sv.URL, the caller's token forwarded when that is the route on the
// models Gateway (the Gateway's JWT policy admits nothing else), kagent's
// placeholder key when it is the keyless in-cluster Service. The ModelConfig
// is named after the object — the rule the portal's serve flow applies.
func (sv served) agentEndpoint() backend.AgentEndpoint {
	routed := sv.routed()
	return backend.AgentEndpoint{Provider: "OpenAI", BaseURL: sv.URL + "/v1", Model: sv.servedName(), APIKeyPassthrough: routed, PlaceholderAPIKey: !routed, Name: sv.Name}
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
	pods := b.predictorPods(ctx, s)
	out := make([]served, 0, len(items))
	for i := range items {
		sv := parseServed(&items[i], idx, s)
		sv.applyPod(pods[sv.Name])
		var total int64
		if p, ok := idx.byName[sv.Preset]; ok {
			total = p.weightsBytes()
		}
		b.weightsBytes(ctx, &sv, total)
		out = append(out, sv)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	b.rememberServed(out)
	// While they exist, remember which repository each one fills its cache
	// directory from (index.go).
	b.recordServed(ctx, out)
	return out, nil
}

// predictorPod is what the driver reads off the predictor (workload) pod of
// an InferenceService or LLMInferenceService.
type predictorPod struct {
	Node        string
	Terminating bool
	// Pending is true while the pod waits for a node or for its containers'
	// images; Reason and Message are then the scheduler's or the kubelet's.
	Pending bool
	Reason  string
	Message string
	// Phase and Steps are where the serve is by the pod's account
	// (servePhase); Found says a pod was read at all; facts is what the
	// phase was computed from, for applyPod to finish it with the object.
	Found bool
	Phase string
	Steps []backend.Step
	facts podFacts
}

// outranks reports whether p describes its object better than other: a pod
// that stays over one that terminates, a scheduled one over a pending one.
func (p predictorPod) outranks(other predictorPod) bool {
	if p.Terminating != other.Terminating {
		return !p.Terminating
	}
	return p.Node != "" && other.Node == ""
}

// predictorPods maps the name of an InferenceService or LLMInferenceService
// to its predictor (workload) pod.
func (b *Backend) predictorPods(ctx context.Context, s settings) map[string]predictorPod {
	pods := map[string]*corev1.Pod{}
	b.podsByName(ctx, s.Namespace, isvcPodLabel, isvcPodLabel, pods)
	b.podsByName(ctx, s.Namespace, llmisvcPodSelector, llmisvcPodLabel, pods)
	out := make(map[string]predictorPod, len(pods))
	if len(pods) == 0 {
		return out
	}
	// The phases need the pods' Events and nodes — a crashed runtime's log,
	// and for a pod without a node the NodeClaim Karpenter nominated and
	// its refusals: the nodes once, the rest per pod concurrently, each read
	// bounded (phases.go, launch.go).
	gpus := b.nodeGPUs(ctx, s.GPUResourceName)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for name, p := range pods {
		wg.Add(1)
		go func(name string, p *corev1.Pod) {
			defer wg.Done()
			facts := podFacts{Pod: p, Events: b.podEvents(ctx, p), GPUResource: s.GPUResourceName, Now: time.Now(), ScaleUpTimeout: b.opts.ScaleUpTimeout}
			if n, ok := gpus[p.Spec.NodeName]; ok {
				facts.NodeKnown, facts.NodeGPUs = true, n
			}
			if cs := runtimeStatus(p); cs != nil && crashed(cs) {
				facts.Crash = b.crashLog(ctx, p, cs)
			}
			if p.Spec.NodeName == "" && !conditionIs(p, corev1.PodScheduled, corev1.ConditionTrue) {
				facts.Launch = b.launchFacts(ctx, facts.Events)
			}
			mu.Lock()
			out[name] = predictorPodOf(p, facts)
			mu.Unlock()
		}(name, p)
	}
	wg.Wait()
	return out
}

// podsByName adds every pod matching selector to out, keyed by the pod's
// nameLabel value; of several pods for one name (a rollout, a replacement)
// the one that outranks the others stays.
func (b *Backend) podsByName(ctx context.Context, namespace, selector, nameLabel string, out map[string]*corev1.Pod) {
	pods, err := b.k8s(ctx).CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		b.log.Warn("listing predictor pods failed", "namespace", namespace, "selector", selector, "error", err)
		return
	}
	for i := range pods.Items {
		name := pods.Items[i].Labels[nameLabel]
		if name == "" {
			continue
		}
		cur := &pods.Items[i]
		if prev, ok := out[name]; ok && podRank(prev).outranks(podRank(cur)) {
			continue
		}
		out[name] = cur
	}
}

// podRank is the part of a pod that decides which of several stays.
func podRank(p *corev1.Pod) predictorPod {
	return predictorPod{Node: p.Spec.NodeName, Terminating: p.DeletionTimestamp != nil}
}

// predictorPodOf reads a pod for its object: the node, whether it goes,
// why it is Pending — and, from the facts around it, where the serve is.
// The phase is completed against the object in applyPod (routing, ready).
// Karpenter's account of the node a pod without one waits for — the claim
// it is launching, its refusal, the instance registering — says more than
// the scheduler's Unschedulable and is the pod's reason then.
func predictorPodOf(p *corev1.Pod, facts podFacts) predictorPod {
	pp := podRank(p)
	pp.Found = true
	if p.Status.Phase == corev1.PodPending {
		pp.Pending = true
		pp.Reason, pp.Message = podPendingReason(p)
	}
	pp.Phase, pp.Steps = servePhase(served{}, facts)
	if pp.Pending && p.Spec.NodeName == "" {
		if s := karpenterStep(pp.Steps); s != nil {
			pp.Reason, pp.Message = s.Reason, s.Message
		}
	}
	pp.facts = facts
	return pp
}

// karpenterStep is the step under way or failed when its reason is
// Karpenter's account of the node (launch.go); nil otherwise.
func karpenterStep(steps []backend.Step) *backend.Step {
	for i := range steps {
		s := &steps[i]
		if s.State != backend.StepInProgress && s.State != backend.StepFailed {
			continue
		}
		switch s.Reason {
		case reasonCapacityUnavailable, reasonNodeLaunching, reasonNodeStarting:
			return s
		}
		return nil
	}
	return nil
}

// podPendingReason is why a pod is Pending: the first container waiting with
// a reason (ImagePullBackOff, CreateContainerConfigError, ContainerCreating —
// not PodInitializing, which only says an init container runs), else the
// scheduler's (Unschedulable, with the nodes it looked at), else the pod's
// own status reason.
func podPendingReason(p *corev1.Pod) (reason, message string) {
	for _, list := range [][]corev1.ContainerStatus{p.Status.InitContainerStatuses, p.Status.ContainerStatuses} {
		for _, cs := range list {
			if w := cs.State.Waiting; w != nil && w.Reason != "" && w.Reason != "PodInitializing" {
				return w.Reason, w.Message
			}
		}
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse {
			return c.Reason, c.Message
		}
	}
	return p.Status.Reason, p.Status.Message
}

// applyPod adds what the predictor pod tells: the node it runs on and, while
// the object is not Ready, a Pending pod's own state — the scheduler's reason
// (Unschedulable: no node with a free GPU, a pool still scaling from zero) or
// the kubelet's (ImagePullBackOff) — which says more than the object's Ready
// condition does. A serving object whose pod waits is Pending, not NotReady.
// A Running pod whose step failed (a runtime the kubelet backed off from
// restarting) is NotReady with that step's reason.
func (sv *served) applyPod(p predictorPod) {
	sv.Node = p.Node
	facts := p.facts
	if !p.Found {
		facts = podFacts{Now: time.Now()}
	}
	sv.Phase, sv.Steps = servePhase(*sv, facts)
	if sv.Ready || sv.Deleting {
		return
	}
	if !p.Pending {
		if s := failedStep(sv.Steps); s != nil && !sv.Failed {
			sv.Reason, sv.Message = s.Reason, strings.TrimSpace(s.Reason+" "+s.Message)
		}
		return
	}
	sv.Status = statusPending
	if p.Reason != "" || p.Message != "" {
		sv.Reason, sv.Message = p.Reason, strings.TrimSpace(p.Reason+" "+p.Message)
		return
	}
	// The pod says nothing (an init container running, a container being
	// created): the step under way does.
	for _, s := range sv.Steps {
		if s.State == backend.StepInProgress || s.State == backend.StepFailed {
			sv.Reason, sv.Message = s.Reason, strings.TrimSpace(s.Reason+" "+s.Message)
			return
		}
	}
}

// failedStep is the step that failed; nil when none did.
func failedStep(steps []backend.Step) *backend.Step {
	for i := range steps {
		if steps[i].State == backend.StepFailed {
			return &steps[i]
		}
	}
	return nil
}

// parseServed reads the fields the driver needs from an InferenceService or
// an LLMInferenceService; s names the GPU resource the accelerator count is
// read under and the models Gateway an unpublished address is expected on.
func parseServed(obj *unstructured.Unstructured, idx presetIndex, s settings) served {
	gpuResource := s.GPUResourceName
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

	sv.Status, sv.Reason, sv.Message, sv.Ready = servedStatus(obj)
	sv.ReadyAt = readyTransition(obj)
	failure, ok, _ := unstructured.NestedMap(obj.Object, "status", "modelStatus", "lastFailureInfo")
	sv.Failed = ok && len(failure) > 0
	sv.URL = normalizePredictorURL(servedURL(obj, sv, s.GatewayEndpoint))
	return sv
}

// readyTransition is the Ready condition's lastTransitionTime; zero without
// one.
func readyTransition(obj *unstructured.Unstructured) time.Time {
	conds, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	for _, c := range conds {
		cm, ok := c.(map[string]any)
		if !ok || cm["type"] != "Ready" {
			continue
		}
		if raw, _ := cm["lastTransitionTime"].(string); raw != "" {
			if t, err := time.Parse(time.RFC3339, raw); err == nil {
				return t
			}
		}
	}
	return time.Time{}
}

// servedURL is the address KServe published for the object — status.address
// (classic), status.url, the first of status.addresses (llmisvc) — else the
// address it is expected on (expectedURL).
func servedURL(obj *unstructured.Unstructured, sv served, gateway string) string {
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
	return sv.expectedURL(gateway)
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

// servedStatus maps the KServe conditions / modelStatus to Ready, NotReady or
// Pending, with the reason and the message (the reason and the condition's
// text together) behind a state that is not Ready.
func servedStatus(obj *unstructured.Unstructured) (status, reason, message string, ready bool) {
	conds, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	var readyCond map[string]any
	for _, c := range conds {
		cm, ok := c.(map[string]any)
		if ok && cm["type"] == "Ready" {
			readyCond = cm
		}
	}
	if readyCond != nil && readyCond["status"] == "True" {
		return statusReady, "", "", true
	}
	if failure, ok, _ := unstructured.NestedMap(obj.Object, "status", "modelStatus", "lastFailureInfo"); ok && len(failure) > 0 {
		reason, message = conditionText(failure)
		return statusNotReady, reason, message, false
	}
	if readyCond != nil {
		reason, message = conditionText(readyCond)
		if readyCond["status"] == "False" {
			return statusNotReady, reason, message, false
		}
		return statusPending, reason, message, false
	}
	if ts, _, _ := unstructured.NestedString(obj.Object, "status", "modelStatus", "transitionStatus"); ts != "" && ts != "UpToDate" && ts != "InProgress" {
		return statusNotReady, ts, ts, false
	}
	return statusPending, "", "", false
}

// conditionText reads a condition's (or a failure's) reason, and reason and
// message as one line.
func conditionText(c map[string]any) (reason, text string) {
	reason, _ = c["reason"].(string)
	msg, _ := c["message"].(string)
	return reason, strings.TrimSpace(reason + " " + msg)
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
