package kserve

import (
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// The LLMInferenceService API (KServe's llm-d control plane) and what its
// controller puts on the objects it derives from one.
var llmisvcGVR = schema.GroupVersionResource{Group: "serving.kserve.io", Version: "v1alpha2", Resource: "llminferenceservices"}

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
// Service (<name>-kserve-workload-svc), the fallback until KServe publishes
// the routed address in status.
func workloadURL(name, namespace string) string {
	return fmt.Sprintf("http://%s-kserve-workload-svc.%s.svc.cluster.local:%d", name, namespace, llmisvcWorkloadPort)
}

// gvrFor maps a serving kind to its API resource.
func gvrFor(kind string) schema.GroupVersionResource {
	if kind == ServingKindLLM {
		return llmisvcGVR
	}
	return isvcGVR
}

// composeLLM builds the LLMInferenceService for a preset by spec shape:
// spec.model.uri and .name from the preset, one replica, router.route and
// router.scheduler so KServe renders the HTTPRoute on the configured ingress
// gateway and the scheduler creates the InferencePool, the preset's args, env
// and resources on the template's main container, scheduling as
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
	if len(p.Spec.Env) > 0 {
		main["env"] = mapsToAny(p.Spec.Env)
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

	spec := map[string]any{
		"model":    map[string]any{"uri": p.Spec.Model.StorageURI, "name": p.Spec.Model.ID},
		"replicas": int64(1),
		"router":   map[string]any{"route": map[string]any{}, "scheduler": map[string]any{}},
		"template": template,
	}
	if len(p.Spec.BaseRefs) > 0 {
		spec["baseRefs"] = mapsToAny(p.Spec.BaseRefs)
	}
	obj := newServingObject(ServingKindLLM, p, s.Namespace)
	obj.Object["spec"] = spec
	return obj
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
