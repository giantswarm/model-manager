package kserve

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	"github.com/giantswarm/model-manager/internal/backend"
)

// Labels of the modelServing contract (the agent-platform-connectivity chart
// of giantswarm/agent-platform, templates/model-serving/).
const (
	PresetLabel       = "agent-platform.giantswarm.io/preset"
	PresetSourceLabel = "agent-platform.giantswarm.io/preset-source"
)

// servingPreset is a published ServingPreset document (schema:
// files/model-serving/serving-preset.schema.json in agent-platform-connectivity).
type servingPreset struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec presetSpec `json:"spec"`
	// source is the preset-source label (shipped|values); not part of the doc.
	source string
}

type presetSpec struct {
	DisplayName string `json:"displayName"`
	Description string `json:"description"`
	Model       struct {
		ID            string   `json:"id"`
		StorageURI    string   `json:"storageUri"`
		Format        string   `json:"format"`
		ContextLength int64    `json:"contextLength"`
		Capabilities  []string `json:"capabilities"`
		License       string   `json:"license"`
	} `json:"model"`
	Runtime      string           `json:"runtime"`
	Args         []string         `json:"args"`
	Env          []map[string]any `json:"env"`
	ChatTemplate *struct {
		ConfigMap string `json:"configMap"`
		Key       string `json:"key"`
		MountPath string `json:"mountPath"`
	} `json:"chatTemplate"`
	Resources struct {
		GPUs     *int64         `json:"gpus"`
		Requests map[string]any `json:"requests"`
		Limits   map[string]any `json:"limits"`
	} `json:"resources"`
	Requirements struct {
		WeightsGiB  float64  `json:"weightsGiB"`
		OverheadGiB *float64 `json:"overheadGiB"`
	} `json:"requirements"`
	Scheduling struct {
		NodeSelector map[string]string `json:"nodeSelector"`
		Tolerations  []map[string]any  `json:"tolerations"`
	} `json:"scheduling"`
	// Predictor holds extra InferenceService predictor fields copied on top
	// verbatim (classic kind); Template the LLMInferenceService's
	// spec.template extras, with containers merged by name so a preset can
	// override template.containers[main].image without repeating the rest.
	// BaseRefs names custom LLMInferenceServiceConfigs; absent for every
	// shipped preset — KServe picks its well-known configs from the spec's
	// shape and appends them itself.
	Predictor map[string]any   `json:"predictor"`
	Template  map[string]any   `json:"template"`
	BaseRefs  []map[string]any `json:"baseRefs"`
	// Router decides the LLMInferenceService's router shape for this preset
	// alone: Scheduler set composes (true) or leaves out (false) the llm-d
	// endpoint picker whatever the backend's default; nil follows it.
	Router *presetRouter `json:"router"`
}

// presetRouter is a preset's spec.router.
type presetRouter struct {
	Scheduler *bool `json:"scheduler"`
}

func (p *servingPreset) name() string { return p.Metadata.Name }

func (p *servingPreset) gpus() int64 {
	if p.Spec.Resources.GPUs == nil {
		return 1
	}
	return *p.Spec.Resources.GPUs
}

// cpu reports whether the preset requests no accelerator (resources.gpus: 0):
// CPU work, served on the cluster's CPU capacity and judged against a node's
// allocatable memory — never against the GPU pool, whose taint and label it
// does not carry (settings.forPreset, nodes). A nil preset is a bare model
// reference, which the shipped presets serve on GPUs.
func (p *servingPreset) cpu() bool { return p != nil && p.gpus() == 0 }

// routerScheduler reports whether the preset's LLMInferenceService gets the
// llm-d endpoint picker: the preset's spec.router.scheduler when set, else
// the backend's default.
func (p *servingPreset) routerScheduler(s settings) bool {
	if p.Spec.Router != nil && p.Spec.Router.Scheduler != nil {
		return *p.Spec.Router.Scheduler
	}
	return s.RouterScheduler
}

// storesInCache reports whether the cache claim has a say in where the preset
// is served because its weights live there: every scheme the KServe
// storage-initializer downloads (hf://, which a pre-warm download fills ahead
// of time; with the redirect policy on, the rest write into the claim mounted
// at the model path as well) and pvc://, which reads a claim. Not oci://:
// KServe's modelcar is an OCI image containerd pulls onto the node's own disk
// — no storage-initializer, no claim mount — so the claim's pin and its nodes
// have no say over the placement and there is nothing to download into it
// (giantswarm/model-manager#123). A nil preset is a bare Hugging Face model
// reference and stores in the cache.
func (p *servingPreset) storesInCache() bool {
	return p == nil || !strings.HasPrefix(p.Spec.Model.StorageURI, "oci://")
}

func (p *servingPreset) weightsBytes() int64 {
	return gibToBytes(p.Spec.Requirements.WeightsGiB)
}

func (p *servingPreset) overheadBytes(defaultGiB float64) int64 {
	if p.Spec.Requirements.OverheadGiB == nil {
		return gibToBytes(defaultGiB)
	}
	return gibToBytes(*p.Spec.Requirements.OverheadGiB)
}

func gibToBytes(g float64) int64 {
	if g <= 0 {
		return 0
	}
	return int64(math.Round(g * float64(gib)))
}

// view converts the preset to its API form.
func (p *servingPreset) view(defaultOverheadGiB float64) backend.Preset {
	out := backend.Preset{
		Name:          p.name(),
		DisplayName:   p.Spec.DisplayName,
		Description:   p.Spec.Description,
		Source:        p.source,
		Model:         p.Spec.Model.ID,
		StorageURI:    p.Spec.Model.StorageURI,
		Format:        p.Spec.Model.Format,
		Runtime:       p.Spec.Runtime,
		ContextLength: p.Spec.Model.ContextLength,
		Capabilities:  p.Spec.Model.Capabilities,
		License:       p.Spec.Model.License,
		GPUs:          p.gpus(),
		WeightsBytes:  p.weightsBytes(),
		OverheadBytes: p.overheadBytes(defaultOverheadGiB),
		Args:          p.Spec.Args,
		NodeSelector:  p.Spec.Scheduling.NodeSelector,
	}
	out.RequiredBytes = out.WeightsBytes + out.OverheadBytes
	if p.Spec.ChatTemplate != nil {
		out.ChatTemplate = p.Spec.ChatTemplate.ConfigMap
	}
	return out
}

// parsePreset decodes one preset.yaml document.
func parsePreset(raw []byte, source string) (*servingPreset, error) {
	var p servingPreset
	if err := yaml.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("parse preset: %w", err)
	}
	if p.Kind != "" && p.Kind != presetKind {
		return nil, fmt.Errorf("kind %q, want %s", p.Kind, presetKind)
	}
	if p.APIVersion != "" && p.APIVersion != agentPlatformAPIVersion {
		return nil, fmt.Errorf("apiVersion %q, want %s", p.APIVersion, agentPlatformAPIVersion)
	}
	if p.Metadata.Name == "" {
		return nil, fmt.Errorf("metadata.name is required")
	}
	if p.Spec.Model.ID == "" {
		return nil, fmt.Errorf("spec.model.id is required")
	}
	if p.Spec.Model.StorageURI == "" {
		p.Spec.Model.StorageURI = "hf://" + p.Spec.Model.ID
	}
	if p.Spec.Model.Format == "" {
		p.Spec.Model.Format = "vLLM"
	}
	p.source = source
	return &p, nil
}

// presets lists the published presets from the preset namespace, sorted by
// name. Unparseable ConfigMaps are skipped and reported through the returned
// warnings so one bad preset does not hide the rest.
func (b *Backend) presets(ctx context.Context) ([]*servingPreset, []string, error) {
	s := b.cfg.settings(ctx)
	list, err := b.k8s(ctx).CoreV1().ConfigMaps(s.PresetNamespace).List(ctx, metav1.ListOptions{LabelSelector: s.PresetSelector})
	if err != nil {
		return nil, nil, fmt.Errorf("list presets in %s (%s): %w", s.PresetNamespace, s.PresetSelector, err)
	}
	out := make([]*servingPreset, 0, len(list.Items))
	var warnings []string
	for i := range list.Items {
		cm := &list.Items[i]
		raw, ok := cm.Data[presetConfigKey]
		if !ok {
			warnings = append(warnings, fmt.Sprintf("%s: no %s key", cm.Name, presetConfigKey))
			continue
		}
		p, err := parsePreset([]byte(raw), cm.Labels[PresetSourceLabel])
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("%s: %v", cm.Name, err))
			continue
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name() < out[j].name() })
	b.mu.Lock()
	b.presetCache = out
	b.mu.Unlock()
	return out, warnings, nil
}

// presetIndex answers the two lookups the driver needs: by preset name and by
// model id.
type presetIndex struct {
	byName  map[string]*servingPreset
	byModel map[string][]*servingPreset
	all     []*servingPreset
}

func indexPresets(list []*servingPreset) presetIndex {
	idx := presetIndex{byName: map[string]*servingPreset{}, byModel: map[string][]*servingPreset{}, all: list}
	for _, p := range list {
		idx.byName[p.name()] = p
		key := strings.ToLower(p.Spec.Model.ID)
		idx.byModel[key] = append(idx.byModel[key], p)
	}
	return idx
}

func (idx presetIndex) forModel(model string) []*servingPreset {
	return idx.byModel[strings.ToLower(strings.TrimSpace(model))]
}

func (idx presetIndex) names(list []*servingPreset) []string {
	out := make([]string, 0, len(list))
	for _, p := range list {
		out = append(out, p.name())
	}
	return out
}

// resolve picks the preset for a request: an explicit preset must exist and
// (when a model is given) serve that model; without one, the single preset
// serving the model is used.
func (idx presetIndex) resolve(model, preset string) (*servingPreset, error) {
	if preset != "" {
		p, ok := idx.byName[preset]
		if !ok {
			return nil, fmt.Errorf("%w: preset %q not found (available: %s)", backend.ErrNotFound, preset, strings.Join(idx.names(idx.all), ", "))
		}
		if model != "" && !strings.EqualFold(p.Spec.Model.ID, model) && model != p.name() {
			return nil, fmt.Errorf("%w: preset %q serves %s, not %s", backend.ErrInvalid, preset, p.Spec.Model.ID, model)
		}
		return p, nil
	}
	if p, ok := idx.byName[model]; ok {
		return p, nil
	}
	matches := idx.forModel(model)
	switch len(matches) {
	case 0:
		return nil, nil
	case 1:
		return matches[0], nil
	default:
		return nil, fmt.Errorf("%w: %d presets serve %s (%s); pass preset to choose one", backend.ErrInvalid, len(matches), model, strings.Join(idx.names(matches), ", "))
	}
}
