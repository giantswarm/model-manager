package kserve

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// A served model on the platform's LLM endpoint (the agent-platform chart's
// llmRouting, discovery spec.llmEndpoint): one AgentgatewayModel per Ready
// LLMInferenceService created from a preset, attached to the endpoint's route
// under the preset's name — the public name a client sends as `model` —, a
// Custom provider on the workload Service whose formats are the API
// interfaces the server registered (interfaces.go), so the gateway converts
// only what the model does not speak. A concrete AgentgatewayModel forwards
// the request's `model` unchanged, and vLLM serves the model under its
// Hugging Face id (the llm-d template's --served-model-name), so the object
// sets `model` to that id after any format conversion
// (policies.finalTransformations). The objects are the driver's own, written
// with its ServiceAccount: they follow the serving object's readiness, not a
// caller's request.

var agentgatewayModelGVR = schema.GroupVersionResource{Group: "agentgateway.dev", Version: "v1alpha1", Resource: "agentgatewaymodels"}

const (
	kindAgentgatewayModel = "AgentgatewayModel"
	// servingAnnotation names the LLMInferenceService an AgentgatewayModel
	// puts on the endpoint; specHashAnnotation the digest of the spec the
	// driver wrote, so a sync rewrites an object only when its spec changed.
	servingAnnotation  = "model-manager.giantswarm.io/llminferenceservice"
	specHashAnnotation = "model-manager.giantswarm.io/spec-hash"
	// gatewayResync is how often the objects are compared with the served
	// models when nothing the driver sees changed (an object deleted by hand
	// comes back within it).
	gatewayResync = 5 * time.Minute
)

// llmEndpoint is discovery's spec.llmEndpoint: the parent references a served
// model's AgentgatewayModel carries, the namespace the objects live in (the
// first parent's: a model attaches to routes of its own namespace), the
// listener's in-cluster URL and, when published, the endpoint's public one.
type llmEndpoint struct {
	ParentRefs []map[string]any
	Namespace  string
	Endpoint   string
	External   string
}

// clientURL is the URL a client of the endpoint is given: the public one
// when the installation publishes it, else the in-cluster listener.
func (ep *llmEndpoint) clientURL() string {
	if ep.External != "" {
		return ep.External
	}
	return ep.Endpoint
}

// newLLMEndpoint validates spec.llmEndpoint; a parent without a namespace is
// in the discovery ConfigMap's.
func newLLMEndpoint(parentRefs []map[string]any, endpoint, external, defaultNamespace string) (*llmEndpoint, error) {
	endpoint = strings.TrimRight(strings.TrimSpace(endpoint), "/")
	if len(parentRefs) == 0 || endpoint == "" {
		return nil, errors.New("enabled without parentRefs or without an endpoint")
	}
	ep := &llmEndpoint{Endpoint: endpoint, External: strings.TrimRight(strings.TrimSpace(external), "/")}
	for i, ref := range parentRefs {
		name, _ := ref["name"].(string)
		if name == "" {
			return nil, fmt.Errorf("parentRefs[%d] names no route", i)
		}
		ns, _ := ref["namespace"].(string)
		if ns == "" {
			ns = defaultNamespace
		}
		if i == 0 {
			ep.Namespace = ns
		} else if ns != ep.Namespace {
			return nil, fmt.Errorf("parentRefs[%d] is in namespace %s, not %s: an AgentgatewayModel attaches to routes of its own namespace", i, ns, ep.Namespace)
		}
		ep.ParentRefs = append(ep.ParentRefs, ref)
	}
	return ep, nil
}

// onEndpoint reports whether sv is eligible for the LLM endpoint at all: an
// object created from a preset, whose name is the public one.
func (sv served) onEndpointName() string {
	if !sv.manageable() {
		return ""
	}
	return sv.Preset
}

// composeGatewayModel builds sv's AgentgatewayModel.
func composeGatewayModel(sv served, ep *llmEndpoint) *unstructured.Unstructured {
	formats := make([]any, 0, len(sv.API.Interfaces))
	for _, i := range sv.API.Interfaces {
		formats = append(formats, map[string]any{"type": i.Type})
	}
	parents := make([]any, 0, len(ep.ParentRefs))
	for _, ref := range ep.ParentRefs {
		parents = append(parents, ref)
	}
	spec := map[string]any{
		"parentRefs": parents,
		"provider":   "Custom",
		// The upstream path is the baseURL's path plus the format's suffix:
		// /v1 makes /v1/chat/completions, /v1/messages, ...
		"baseURL": workloadURL(sv.Name, sv.Namespace) + "/v1",
		"custom":  map[string]any{"formats": formats},
		"policies": map[string]any{"finalTransformations": []any{
			map[string]any{"field": "model", "expression": strconv.Quote(sv.Model)},
		}},
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": agentgatewayModelGVR.GroupVersion().String(),
		"kind":       kindAgentgatewayModel,
		"metadata": map[string]any{
			"name":      sv.Preset,
			"namespace": ep.Namespace,
			"labels": map[string]any{
				ManagedByLabel: ManagedByValue,
				BackendLabel:   "kserve",
				PresetLabel:    sv.Preset,
			},
			"annotations": map[string]any{
				ModelAnnotation:    sv.Model,
				servingAnnotation:  sv.Namespace + "/" + sv.Name,
				specHashAnnotation: specHash(spec),
			},
		},
		"spec": spec,
	}}
}

// specHash is a short digest of a spec (encoding/json sorts map keys).
func specHash(spec map[string]any) string {
	raw, _ := json.Marshal(spec)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:8])
}

// gatewayModelSelector selects the driver's AgentgatewayModels.
var gatewayModelSelector = ManagedByLabel + "=" + ManagedByValue + "," + BackendLabel + "=kserve"

// syncGatewayModels puts the served models on the LLM endpoint: an
// AgentgatewayModel for every Ready model created from a preset whose server
// registered at least one interface, kept while its serving object exists
// (a model restarting keeps its object), deleted when that object is gone.
// Each served entry learns whether it is on the endpoint, or why not. A pass
// whose inputs match the last successful one within gatewayResync writes
// nothing.
func (b *Backend) syncGatewayModels(ctx context.Context, s settings, list []served) {
	ep := s.LLMEndpoint
	if ep == nil {
		return
	}
	desired := map[string]*unstructured.Unstructured{}
	present := map[string]bool{}
	for i := range list {
		sv := &list[i]
		name := sv.onEndpointName()
		switch {
		case sv.Deleting:
			continue
		case name == "":
			sv.EndpointReason = "not on the LLM endpoint: the serving object was not created from a serving preset, whose name would be the public one"
			continue
		}
		present[name] = true
		switch {
		case !sv.Ready:
			sv.EndpointReason = "not on the LLM endpoint until the model is Ready"
		case !sv.API.read():
			sv.EndpointReason = "not on the LLM endpoint until its server is read"
		case len(sv.API.Interfaces) == 0:
			sv.EndpointReason = "not on the LLM endpoint: the server reports no API interfaces (" + sv.API.Reason + ")"
		default:
			desired[name] = composeGatewayModel(*sv, ep)
		}
	}
	fingerprint := gatewayFingerprint(ep, desired, present)

	b.gwMu.Lock()
	fresh := fingerprint == b.gwFingerprint && time.Since(b.gwSyncedAt) < gatewayResync
	onEndpoint := b.gwOnEndpoint
	b.gwMu.Unlock()
	if !fresh {
		onEndpoint = b.applyGatewayModels(ctx, ep, desired, present)
		b.gwMu.Lock()
		b.gwOnEndpoint, b.gwSyncedAt = onEndpoint, time.Now()
		b.gwFingerprint = ""
		if len(onEndpoint.failed) == 0 {
			b.gwFingerprint = fingerprint
		}
		b.gwMu.Unlock()
	}
	for i := range list {
		sv := &list[i]
		name := sv.onEndpointName()
		if name == "" || sv.Deleting {
			continue
		}
		if reason, failed := onEndpoint.failed[name]; failed {
			sv.EndpointReason = reason
			continue
		}
		if onEndpoint.names[name] {
			sv.OnEndpoint, sv.EndpointReason = true, ""
		}
	}
}

// gatewayState is what the last write left on the endpoint: the public names
// whose object exists, and the ones whose write failed with why.
type gatewayState struct {
	names  map[string]bool
	failed map[string]string
}

// applyGatewayModels creates, updates and deletes the driver's
// AgentgatewayModels so they are desired, plus the existing ones whose
// serving object is still present.
func (b *Backend) applyGatewayModels(ctx context.Context, ep *llmEndpoint, desired map[string]*unstructured.Unstructured, present map[string]bool) gatewayState {
	state := gatewayState{names: map[string]bool{}, failed: map[string]string{}}
	res := b.dyn.Resource(agentgatewayModelGVR).Namespace(ep.Namespace)
	existing, err := res.List(ctx, metav1.ListOptions{LabelSelector: gatewayModelSelector})
	if err != nil {
		reason := fmt.Sprintf("not on the LLM endpoint: listing the AgentgatewayModels in %s failed: %v", ep.Namespace, err)
		for name := range present {
			state.failed[name] = reason
		}
		b.log.Warn("listing the LLM endpoint's AgentgatewayModels failed", "namespace", ep.Namespace, "error", err)
		return state
	}
	have := map[string]*unstructured.Unstructured{}
	for i := range existing.Items {
		have[existing.Items[i].GetName()] = &existing.Items[i]
	}
	for name, obj := range desired {
		cur, ok := have[name]
		switch {
		case !ok:
			_, err = res.Create(ctx, obj, metav1.CreateOptions{FieldManager: ManagedByValue})
		case cur.GetAnnotations()[specHashAnnotation] != obj.GetAnnotations()[specHashAnnotation]:
			obj.SetResourceVersion(cur.GetResourceVersion())
			_, err = res.Update(ctx, obj, metav1.UpdateOptions{FieldManager: ManagedByValue})
		default:
			err = nil
		}
		if err != nil {
			state.failed[name] = fmt.Sprintf("not on the LLM endpoint: writing %s %s/%s failed: %v", kindAgentgatewayModel, ep.Namespace, name, err)
			b.log.Warn("writing a served model's AgentgatewayModel failed", "name", name, "namespace", ep.Namespace, "error", err)
			continue
		}
		state.names[name] = true
		if !ok {
			b.log.Info("served model put on the LLM endpoint", "name", name, "namespace", ep.Namespace, "model", obj.GetAnnotations()[ModelAnnotation])
		}
	}
	for name := range have {
		if desired[name] != nil {
			continue
		}
		if present[name] {
			state.names[name] = true // restarting: the object stays
			continue
		}
		if err := b.deleteGatewayModel(ctx, ep, name); err != nil {
			b.log.Warn("deleting an AgentgatewayModel whose serving object is gone failed", "name", name, "namespace", ep.Namespace, "error", err)
		}
	}
	return state
}

// deleteGatewayModel removes the driver's AgentgatewayModel of a public name;
// one that is not there is no error.
func (b *Backend) deleteGatewayModel(ctx context.Context, ep *llmEndpoint, name string) error {
	err := b.dyn.Resource(agentgatewayModelGVR).Namespace(ep.Namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete %s %s/%s: %w", kindAgentgatewayModel, ep.Namespace, name, err)
	}
	if err == nil {
		b.log.Info("served model taken off the LLM endpoint", "name", name, "namespace", ep.Namespace)
	}
	b.gwMu.Lock()
	b.gwFingerprint = ""
	b.gwMu.Unlock()
	return nil
}

// gatewayFingerprint digests what a sync would write: the endpoint, the
// desired objects' specs and the public names still served.
func gatewayFingerprint(ep *llmEndpoint, desired map[string]*unstructured.Unstructured, present map[string]bool) string {
	names := make([]string, 0, len(present))
	for name := range present {
		names = append(names, name)
	}
	sort.Strings(names)
	var b strings.Builder
	fmt.Fprintf(&b, "%s|%s|", ep.Namespace, ep.Endpoint)
	for _, name := range names {
		hash := ""
		if obj := desired[name]; obj != nil {
			hash = obj.GetAnnotations()[specHashAnnotation]
		}
		fmt.Fprintf(&b, "%s=%s;", name, hash)
	}
	return b.String()
}
