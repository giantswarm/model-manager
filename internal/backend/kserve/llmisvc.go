package kserve

import (
	"fmt"
	"strconv"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// The LLMInferenceService API (KServe's llm-d control plane, the only kind
// the driver composes), the LLMInferenceServiceConfigs its controller
// composes from, and what the controller puts on the objects it derives.
var (
	llmisvcGVR       = schema.GroupVersionResource{Group: "serving.kserve.io", Version: "v1alpha2", Resource: "llminferenceservices"}
	llmisvcConfigGVR = schema.GroupVersionResource{Group: "serving.kserve.io", Version: "v1alpha2", Resource: "llminferenceserviceconfigs"}
)

const (
	// Labels the controller puts on the workload pods of an
	// LLMInferenceService: part-of names the kind, name the object.
	llmisvcPodSelector = "app.kubernetes.io/part-of=llminferenceservice"
	llmisvcPodLabel    = "app.kubernetes.io/name"
	// llmisvcMainContainer is the container the well-known template
	// (kserve-config-llm-template) names; the preset's args, env and
	// resources go there.
	llmisvcMainContainer = "main"
	// llmisvcWorkloadPort is the port the template's vLLM listens on.
	llmisvcWorkloadPort = 8000
)

// workloadURL is the in-cluster URL of an LLMInferenceService's workload
// Service (<name>-kserve-workload-svc): where the model answers until KServe
// publishes the routed address in status, and for good where discovery names
// no models Gateway.
func workloadURL(name, namespace string) string {
	return fmt.Sprintf("http://%s-kserve-workload-svc.%s.svc.cluster.local:%d", name, namespace, llmisvcWorkloadPort)
}

// composeLLM builds the LLMInferenceService for a preset by spec shape:
// spec.model.uri and .name from the preset, one replica, router.route so
// KServe renders the HTTPRoute on the configured ingress gateway with the
// workload Service as its backend — router.scheduler beside it only when the
// preset or the backend asks for the llm-d endpoint picker, whose
// InferencePool the gateway resolves only with the Inference Extension —, the
// preset's args, env (with the modelcar environment for a model image) and
// resources on the template's main container,
// scheduling as
// nodeSelector/tolerations, the chat template mounted, and
// template.runtimeClassName from discovery when set. No baseRefs — KServe's
// controller chooses its well-known LLMInferenceServiceConfigs from the
// spec's shape and appends them itself; a preset naming a custom config is
// the only baseRefs case. No image — the container runs the one the
// well-known template names unless the preset's template overrides
// containers[main].image.
func (b *Backend) composeLLM(p *servingPreset, s settings, node string) *unstructured.Unstructured {
	main := map[string]any{"name": llmisvcMainContainer}
	if len(p.Spec.Args) > 0 {
		main["args"] = toAnySlice(p.Spec.Args)
	}
	if env := p.env(); len(env) > 0 {
		main["env"] = mapsToAny(env)
	}
	if res := p.resources(s); len(res) > 0 {
		main["resources"] = res
	}
	template := map[string]any{}
	if mount, volume := p.chatTemplateMount(); mount != nil {
		main["volumeMounts"] = []any{mount}
		template["volumes"] = []any{volume}
	}
	template["containers"] = []any{main}
	if ns := p.nodeSelector(s, node); len(ns) > 0 {
		template["nodeSelector"] = ns
	}
	if tols := p.tolerations(s); len(tols) > 0 {
		template["tolerations"] = tols
	}
	if s.RuntimeClassName != "" {
		template["runtimeClassName"] = s.RuntimeClassName
	}
	mergeTemplate(template, p.Spec.Template)

	router := map[string]any{"route": map[string]any{}}
	if p.routerScheduler(s) {
		router["scheduler"] = map[string]any{}
	}
	spec := map[string]any{
		"model":    map[string]any{"uri": p.Spec.Model.StorageURI, "name": p.Spec.Model.ID},
		"replicas": int64(1),
		"router":   router,
		"template": template,
	}
	if len(p.Spec.BaseRefs) > 0 {
		spec["baseRefs"] = mapsToAny(p.Spec.BaseRefs)
	}
	obj := newServingObject(p, s.Namespace)
	obj.Object["spec"] = spec
	return obj
}

// modelcarEnv is the environment of a predictor served from a model image
// (giantswarm/model-manager#146). KServe's modelcar path runs the runtime as
// its modelcar uid on a read-only image, a uid the runtime image's passwd does
// not know: vLLM's import dies in torch's inductor cache-dir lookup
// (getpass.getuser → getpwuid: KeyError) unless the user name comes from the
// environment, and the home, the Hub cache and the compile caches must be
// writable, so they live under /tmp.
var modelcarEnv = [][2]string{
	{"HOME", "/tmp"},
	{"HF_HOME", "/tmp/hf"},
	{"VLLM_CACHE_ROOT", "/tmp/vllm-cache"},
	{"TORCHINDUCTOR_CACHE_DIR", "/tmp/torchinductor"},
	{"USER", "vllm"},
	{"LOGNAME", "vllm"},
}

// env is the main container's environment: the preset's own, and for a
// preset served from a model image the modelcar environment after it, for
// every name the preset does not set itself.
func (p *servingPreset) env() []map[string]any {
	if !p.fromModelImage() {
		return p.Spec.Env
	}
	out := append([]map[string]any{}, p.Spec.Env...)
	set := make(map[string]bool, len(p.Spec.Env))
	for _, e := range p.Spec.Env {
		if name, ok := e["name"].(string); ok {
			set[name] = true
		}
	}
	for _, e := range modelcarEnv {
		if !set[e[0]] {
			out = append(out, map[string]any{"name": e[0], "value": e[1]})
		}
	}
	return out
}

// newServingObject is the LLMInferenceService's metadata: the preset's name,
// the labels that make the object model-manager's and link it to its preset,
// the model annotation.
func newServingObject(p *servingPreset, namespace string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": llmisvcGVR.GroupVersion().String(),
		"kind":       kindLLMInferenceService,
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

// mergeTemplate copies the preset's spec.template extras on top of the
// composed template: containers are merged by name, so a preset overrides
// containers[main].image without repeating args, env or resources; every
// other field is copied verbatim.
func mergeTemplate(dst, extra map[string]any) {
	for k, v := range extra {
		if k != "containers" {
			dst[k] = v
			continue
		}
		list, _ := v.([]any)
		containers, _ := dst["containers"].([]any)
		for _, c := range list {
			if cm, ok := c.(map[string]any); ok {
				containers = mergeContainer(containers, cm)
			}
		}
		dst["containers"] = containers
	}
}

func mergeContainer(containers []any, extra map[string]any) []any {
	for _, c := range containers {
		cm, ok := c.(map[string]any)
		if ok && cm["name"] == extra["name"] {
			for k, v := range extra {
				cm[k] = v
			}
			return containers
		}
	}
	return append(containers, extra)
}

// mainContainer returns the main container of an LLMInferenceService's
// template, nil when there is none.
func mainContainer(obj *unstructured.Unstructured) map[string]any {
	containers, _, _ := unstructured.NestedSlice(obj.Object, "spec", "template", "containers")
	for _, c := range containers {
		if cm, ok := c.(map[string]any); ok && cm["name"] == llmisvcMainContainer {
			return cm
		}
	}
	return nil
}
