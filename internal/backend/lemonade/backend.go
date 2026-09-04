// Package lemonade implements the serving backend over a Lemonade Server —
// AMD's local model server: FastFlowLM on Ryzen AI NPUs, llama.cpp on GPUs
// and CPUs, ONNX Runtime GenAI and more, one OpenAI-compatible API in front.
// The driver is a thin proxy of Lemonade's management API: /api/v1/health for
// the loaded models, /api/v1/models, the streamed /api/v1/pull,
// /api/v1/load, /api/v1/unload and /api/v1/delete, plus /api/v1/system-info
// for the host node (nodes.go). Lemonade does downloads and memory management
// itself, so there are no Jobs, presets or fit checks here — those are kserve
// concerns.
package lemonade

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/giantswarm/model-manager/internal/backend"
)

const (
	// DefaultEndpoint is where a Lemonade Server listens by default.
	DefaultEndpoint = "http://127.0.0.1:13305"
	// AgentAPIPath is Lemonade's OpenAI-compatible base path, the one written
	// into kagent ModelConfigs after the agent host.
	AgentAPIPath = "/api/v1"
	// KeepAliveForever is the one keep-alive Lemonade has a meaning for: a
	// Load with it pins the model (Lemonade's `pinned`), exempting it from
	// slot eviction. Every other keep-alive is ignored — there is no timer.
	KeepAliveForever = "-1"

	// ProviderOpenAI is the kagent provider Lemonade's endpoint is wired as.
	ProviderOpenAI = "OpenAI"

	statusLoaded = "loaded"
	statusReady  = "ready"
)

// loading is how Lemonade manages memory: a model is loaded by the first
// completion that names it and stays until it is unloaded or until another
// model of its type needs the slot (`max_loaded_models`, one per type by
// default; least recently used goes first, pinned models never). There is no
// idle timer and no keep-alive.
var loading = backend.Loading{OnDemand: true, IdleEviction: false}

// Backend is the lemonade driver.
type Backend struct {
	client    *Client
	endpoint  string
	agentHost string
	now       func() time.Time
}

// Factory builds the driver from backend.Options.
func Factory(opts backend.Options) (backend.Backend, error) {
	return New(opts.Lemonade)
}

// New builds the driver. AgentHost defaults to Endpoint.
func New(opts backend.LemonadeOptions) (*Backend, error) {
	endpoint := strings.TrimRight(strings.TrimSpace(opts.Endpoint), "/")
	if endpoint == "" {
		return nil, fmt.Errorf("lemonade endpoint is required")
	}
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("lemonade endpoint %q must be an http(s) URL", opts.Endpoint)
	}
	agentHost := strings.TrimRight(strings.TrimSpace(opts.AgentHost), "/")
	if agentHost == "" {
		agentHost = endpoint
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	hc := &http.Client{Timeout: timeout, Transport: http.DefaultTransport.(*http.Transport).Clone()}
	return &Backend{client: NewClient(endpoint, hc, opts.LoadTimeout), endpoint: endpoint, agentHost: agentHost, now: time.Now}, nil
}

// NewWithClient builds the driver around an existing client (tests).
func NewWithClient(c *Client, endpoint, agentHost string) *Backend {
	if agentHost == "" {
		agentHost = endpoint
	}
	return &Backend{client: c, endpoint: endpoint, agentHost: agentHost, now: time.Now}
}

// Name implements backend.Backend.
func (b *Backend) Name() backend.Name { return backend.NameLemonade }

// Capabilities implements backend.Backend. Wire is decided by the service.
// NodeInventory is the proxied host as one node (ListNodes).
func (b *Backend) Capabilities() backend.Capabilities {
	return backend.Capabilities{
		Pull:          true,
		PullProgress:  true,
		Delete:        true,
		Load:          true,
		Unload:        true,
		LoadedModels:  true,
		NodeInventory: true,
	}
}

// Info implements backend.Backend. AgentEndpoint is the OpenAI-compatible
// base URL agents dial (the one written into ModelConfigs), Endpoint the
// server as model-manager dials it.
func (b *Backend) Info(ctx context.Context) backend.Info {
	info := backend.Info{Backend: backend.NameLemonade, Endpoint: b.endpoint, AgentEndpoint: b.agentBaseURL(), Loading: loading}
	h, err := b.client.Health(ctx)
	if err != nil {
		info.Message = err.Error()
		return info
	}
	info.Version = h.Version
	if h.Status != "" && h.Status != "ok" {
		info.Message = "lemonade reports status " + h.Status
		return info
	}
	info.Healthy = true
	return info
}

// ListModels implements backend.Backend (downloaded models, /api/v1/models).
func (b *Backend) ListModels(ctx context.Context) ([]backend.Model, error) {
	models, err := b.client.Models(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]backend.Model, 0, len(models))
	for _, m := range models {
		if !m.downloaded() {
			continue
		}
		out = append(out, toModel(m))
	}
	return out, nil
}

// GetModel implements backend.Backend. Lemonade ids are case-sensitive; a
// reference that matches exactly one id case-insensitively is accepted.
func (b *Backend) GetModel(ctx context.Context, name string) (*backend.Model, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("%w: empty model name", backend.ErrInvalid)
	}
	models, err := b.client.Models(ctx)
	if err != nil {
		return nil, err
	}
	var folded []apiModel
	for _, m := range models {
		if !m.downloaded() {
			continue
		}
		if m.ID == name {
			out := toModel(m)
			return &out, nil
		}
		if strings.EqualFold(m.ID, name) {
			folded = append(folded, m)
		}
	}
	if len(folded) == 1 {
		out := toModel(folded[0])
		return &out, nil
	}
	return nil, fmt.Errorf("%w: %s", backend.ErrNotFound, name)
}

// ListLoaded implements backend.Backend (all_models_loaded of /api/v1/health).
// Lemonade reports no per-model memory; SizeBytes is the catalog size.
func (b *Backend) ListLoaded(ctx context.Context) ([]backend.LoadedModel, error) {
	h, err := b.client.Health(ctx)
	if err != nil {
		return nil, err
	}
	catalog, err := b.client.Models(ctx)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]apiModel, len(catalog))
	for _, m := range catalog {
		byID[m.ID] = m
	}
	node := hostOf(b.agentHost)
	out := make([]backend.LoadedModel, 0, len(h.AllModelsLoaded))
	for _, l := range h.AllModelsLoaded {
		lm := backend.LoadedModel{
			Name:          l.ModelName,
			SizeBytes:     gbToBytes(byID[l.ModelName].SizeGB),
			ContextLength: l.RecipeOptions.CtxSize,
			Status:        loadedStatus(l),
			Node:          node,
			Device:        l.Device,
			Pinned:        l.Pinned,
		}
		if lm.ContextLength == 0 {
			lm.ContextLength = byID[l.ModelName].ContextLength
		}
		out = append(out, lm)
	}
	return out, nil
}

// Pull implements backend.Backend: a streamed install of a catalog model.
// Lemonade reports per-file progress and, when it knows it, the size of the
// whole download; BytesCompleted/BytesTotal describe the whole download.
func (b *Backend) Pull(ctx context.Context, req backend.PullRequest, progress func(backend.Progress)) error {
	ref := strings.TrimSpace(req.Ref)
	if ref == "" {
		return fmt.Errorf("%w: empty model reference", backend.ErrInvalid)
	}
	var total, completed int64
	err := b.client.Pull(ctx, ref, func(ev pullEvent) {
		if progress == nil {
			return
		}
		p := backend.Progress{}
		if ev.Event == eventComplete {
			p.Status = "success"
			if total > 0 {
				completed = total
			}
		} else {
			if ev.TotalDownloadSize > 0 {
				total = ev.TotalDownloadSize
			} else if ev.TotalFiles <= 1 && ev.BytesTotal > 0 {
				// A single file is the whole download.
				total = ev.BytesTotal
			}
			done := ev.CumulativeBytesDownloaded
			if done == 0 {
				done = ev.CompletedFilesBytes + ev.BytesDownloaded
			}
			if done > completed {
				completed = done
			}
			p.Status = describe(ev)
		}
		p.BytesTotal, p.BytesCompleted = total, completed
		progress(p)
	})
	return mapErr(err, ref)
}

// describe words a progress event.
func describe(ev pullEvent) string {
	if ev.File != "" && ev.TotalFiles > 0 {
		return fmt.Sprintf("downloading %s (%d/%d)", ev.File, ev.FileIndex, ev.TotalFiles)
	}
	if ev.File != "" {
		return "downloading " + ev.File
	}
	return "downloading"
}

// Delete implements backend.Backend.
func (b *Backend) Delete(ctx context.Context, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("%w: empty model name", backend.ErrInvalid)
	}
	return mapErr(b.client.Delete(ctx, name), name)
}

// Load implements backend.Backend. KeepAliveForever pins the model against
// slot eviction; Lemonade has no timer, so every other keep-alive is ignored.
func (b *Backend) Load(ctx context.Context, req backend.LoadRequest) error {
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return fmt.Errorf("%w: empty model name", backend.ErrInvalid)
	}
	pinned := strings.TrimSpace(req.KeepAlive) == KeepAliveForever
	return mapErr(b.client.Load(ctx, name, pinned), name)
}

// Unload implements backend.Backend. Unloading a model that is not loaded
// is a no-op, as on ollama.
func (b *Backend) Unload(ctx context.Context, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("%w: empty model name", backend.ErrInvalid)
	}
	err := b.client.Unload(ctx, name)
	if isNotLoaded(err) {
		return nil
	}
	return mapErr(err, name)
}

// AgentEndpoint implements backend.Backend: kagent's OpenAI provider pointed
// at Lemonade's OpenAI-compatible API as agent pods reach it, with the
// placeholder API key the kagent runtime insists on.
func (b *Backend) AgentEndpoint(model string) backend.AgentEndpoint {
	return backend.AgentEndpoint{Provider: ProviderOpenAI, BaseURL: b.agentBaseURL(), Model: model, PlaceholderAPIKey: true}
}

// agentBaseURL is the agent host plus AgentAPIPath (once).
func (b *Backend) agentBaseURL() string {
	host := strings.TrimRight(b.agentHost, "/")
	for _, suffix := range []string{AgentAPIPath, "/v1"} {
		if strings.HasSuffix(host, suffix) {
			return host
		}
	}
	return host + AgentAPIPath
}

func toModel(m apiModel) backend.Model {
	return backend.Model{
		Name:          m.ID,
		SizeBytes:     gbToBytes(m.SizeGB),
		Format:        formatOf(m.Checkpoint),
		Quantization:  quantizationOf(m.Checkpoint),
		ContextLength: m.ContextLength,
		Capabilities:  capabilitiesOf(m.Labels, m.Recipe),
		Runtime:       m.Recipe,
	}
}

// loadedStatus is statusLoaded once Lemonade's backend process for the model
// answers, else Lemonade's own state (e.g. loading).
func loadedStatus(l loadedModel) string {
	if l.Status == statusReady || l.BackendHealth == statusReady || l.Status == "" {
		return statusLoaded
	}
	return strings.ToLower(l.Status)
}

// gbToBytes converts Lemonade's decimal-GB size to bytes.
func gbToBytes(gb float64) int64 {
	if gb <= 0 || math.IsNaN(gb) || math.IsInf(gb, 0) {
		return 0
	}
	return int64(math.Round(gb * 1e9))
}

// formatOf derives the weights format from a checkpoint reference such as
// unsloth/Qwen3-0.6B-GGUF:Q4_0 or stabilityai/sd-turbo:sd_turbo.safetensors.
func formatOf(checkpoint string) string {
	c := strings.ToLower(checkpoint)
	switch {
	case strings.Contains(c, "gguf"):
		return "gguf"
	case strings.Contains(c, ".safetensors"):
		return "safetensors"
	case strings.Contains(c, ".onnx") || strings.Contains(c, "-onnx"):
		return "onnx"
	}
	return ""
}

// quantizationRe matches GGUF variant names: Q4_K_M, Q8_0, IQ4_XS, F16, BF16,
// UD-Q4_K_XL.
var quantizationRe = regexp.MustCompile(`(?i)^(i?q\d[a-z0-9_]*|f16|f32|bf16|ud-[a-z0-9_]+)$`)

// quantizationOf is the variant of a GGUF checkpoint (org/repo:variant),
// empty when the reference carries none that reads as a quantization.
func quantizationOf(checkpoint string) string {
	colon := strings.LastIndex(checkpoint, ":")
	if colon < 0 || !strings.Contains(checkpoint[:colon], "/") {
		return ""
	}
	variant := strings.TrimSuffix(checkpoint[colon+1:], ".gguf")
	if quantizationRe.MatchString(variant) {
		return variant
	}
	return ""
}

// Lemonade labels onto the capability vocabulary the other backends use.
var labelCapabilities = map[string]string{
	"chat":         "completion",
	"tool-calling": "tools",
	"reasoning":    "thinking",
	"embeddings":   "embedding",
	"embedding":    "embedding",
	"classifier":   "classification",
}

// droppedLabels are marketing / launch hints, not model features.
var droppedLabels = map[string]bool{"hot": true, "experimental": true, "mtp": true}

// deploymentLabels name a model's endpoint; a text model carries chat, or
// none at all when its recipe defaults to chat.
var deploymentLabels = map[string]bool{
	"chat": true, "transcription": true, "embeddings": true, "embedding": true, "reranking": true,
	"image": true, "tts": true, "audio-generation": true, "classification": true, "classifier": true, "3d": true,
}

// chatRecipes default to the chat deployment when the model has no
// deployment label.
var chatRecipes = map[string]bool{"llamacpp": true, "flm": true, "vllm": true, "ryzenai-llm": true, "cloud": true, "ds4": true}

// capabilitiesOf maps Lemonade's labels (chat, tool-calling, reasoning,
// vision, embeddings, ...) onto the capabilities the other backends report
// (completion, tools, thinking, vision, embedding, ...); unknown labels pass
// through. completion comes first when present.
func capabilitiesOf(labels []string, recipe string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(c string) {
		if c == "" || seen[c] {
			return
		}
		seen[c] = true
		out = append(out, c)
	}
	deployed := false
	for _, l := range labels {
		l = strings.ToLower(strings.TrimSpace(l))
		if deploymentLabels[l] {
			deployed = true
		}
	}
	if !deployed && chatRecipes[recipe] {
		add("completion")
	}
	for _, l := range labels {
		l = strings.ToLower(strings.TrimSpace(l))
		if droppedLabels[l] {
			continue
		}
		if c, ok := labelCapabilities[l]; ok {
			add(c)
			continue
		}
		add(l)
	}
	// completion first, as ollama lists it.
	for i, c := range out {
		if c == "completion" && i > 0 {
			out = append([]string{c}, append(out[:i:i], out[i+1:]...)...)
			break
		}
	}
	return out
}

// mapErr turns Lemonade's answers into the backend's sentinel errors.
func mapErr(err error, name string) error {
	if err == nil {
		return nil
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return err
	}
	msg := strings.ToLower(apiErr.Message)
	switch {
	case apiErr.Status == http.StatusNotFound,
		apiErr.Code == "model_not_found", apiErr.Code == "unknown_model", apiErr.Code == "not_found",
		strings.Contains(msg, "not found"):
		return fmt.Errorf("%w: %s: %s", backend.ErrNotFound, name, apiErr.Message)
	case apiErr.Status == http.StatusBadRequest, apiErr.Status == http.StatusUnprocessableEntity:
		return fmt.Errorf("%w: %s: %s", backend.ErrInvalid, name, apiErr.Message)
	}
	return err
}

// isNotLoaded reports Lemonade's answer to unloading a model that is not
// loaded ("Model not loaded: <name>", HTTP 404).
func isNotLoaded(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && strings.Contains(strings.ToLower(apiErr.Message), "not loaded")
}
