// Package lmstudio implements the serving backend over an LM Studio server —
// the desktop model runner (llama.cpp on GPU and CPU, MLX on Apple silicon)
// with one OpenAI-compatible API in front. The driver is a thin proxy of LM
// Studio's own API under /api/v1: the local library (/models), the download
// that backs a pull with the job that tracks it, and /models/load and
// /models/unload. LM Studio does downloads and memory management itself, so
// there are no Jobs, presets or fit checks here — those are kserve concerns.
//
// Two LM Studio traits shape the driver. It exposes no delete (removing a
// model is `lms rm` on the host, which a pod cannot run), so Delete is not
// among its capabilities. And it answers HTTP 200 with an {"error":…}
// document for every path outside /api/v1 — including Ollama's /api/version,
// /api/tags and /api/show — so nothing here may read a status code as proof
// that it reached an LM Studio; the shape of the answer is the evidence.
package lmstudio

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/giantswarm/model-manager/internal/backend"
)

const (
	// DefaultEndpoint is where an LM Studio server listens by default.
	DefaultEndpoint = "http://127.0.0.1:1234"
	// AgentAPIPath is LM Studio's OpenAI-compatible base path, the one
	// written into kagent ModelConfigs after the agent host.
	AgentAPIPath = "/v1"

	// ProviderOpenAI is the kagent provider LM Studio's endpoint is wired as.
	ProviderOpenAI = "OpenAI"

	// defaultPollInterval is how often a running download is asked for
	// progress.
	defaultPollInterval = 2 * time.Second
	// maxUnknownStatuses is how many consecutive polls may report a status
	// that is neither `downloading` nor an end pullDone knows before the
	// pull gives up (see the loop in Pull).
	maxUnknownStatuses = 30
)

// loading is how LM Studio manages memory, as it applies to the loads THIS
// driver performs. POST /api/v1/models/load is an explicit load: LM Studio
// treats it as manual, so it has no idle TTL, Auto-Evict leaves it alone
// ("non-JIT loaded models are not affected"), and it stays resident until
// something unloads it. The endpoint accepts no ttl either, which is why
// LoadRequest.KeepAlive has nothing to map onto.
//
// OnDemand is true for the other path: with just-in-time loading on (its
// default) the first completion naming a downloaded model loads it, which is
// what an agent turn does. Note the asymmetry — a JIT-loaded model DOES get
// LM Studio's default idle TTL (60 minutes) and is subject to Auto-Evict, so
// a model an agent warmed up can be gone later, while one loaded through
// this driver will not be. IdleEviction describes the driver's own loads.
var loading = backend.Loading{OnDemand: true, IdleEviction: false}

// Backend is the lmstudio driver.
type Backend struct {
	client    *Client
	endpoint  string
	agentHost string
	// pollInterval paces the progress reads of a running download; tests
	// shorten it.
	pollInterval time.Duration
}

// Factory builds the driver from backend.Options.
func Factory(opts backend.Options) (backend.Backend, error) {
	return New(opts.LMStudio)
}

// New builds the driver. AgentHost defaults to Endpoint.
func New(opts backend.LMStudioOptions) (*Backend, error) {
	endpoint := strings.TrimRight(strings.TrimSpace(opts.Endpoint), "/")
	if endpoint == "" {
		return nil, fmt.Errorf("lmstudio endpoint is required")
	}
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("lmstudio endpoint %q must be an http(s) URL", opts.Endpoint)
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
	return &Backend{client: NewClient(endpoint, hc, opts.LoadTimeout), endpoint: endpoint, agentHost: agentHost,
		pollInterval: defaultPollInterval}, nil
}

// NewWithClient builds the driver around an existing client (tests).
func NewWithClient(c *Client, endpoint, agentHost string) *Backend {
	if agentHost == "" {
		agentHost = endpoint
	}
	return &Backend{client: c, endpoint: endpoint, agentHost: agentHost, pollInterval: defaultPollInterval}
}

// Name implements backend.Backend.
func (b *Backend) Name() backend.Name { return backend.NameLMStudio }

// Capabilities implements backend.Backend. Wire is decided by the service.
// Delete is absent: LM Studio removes a model only through its `lms` CLI on
// the host, so the service answers 501 for it rather than the driver
// pretending. NodeInventory too: LM Studio reports no host hardware.
func (b *Backend) Capabilities() backend.Capabilities {
	return backend.Capabilities{
		Pull:         true,
		PullProgress: true,
		Load:         true,
		Unload:       true,
		LoadedModels: true,
	}
}

// Info implements backend.Backend. LM Studio serves no version or health
// document, so the inventory answering with a models array is what "healthy"
// means and Version stays empty.
func (b *Backend) Info(ctx context.Context) backend.Info {
	info := backend.Info{Backend: backend.NameLMStudio, Endpoint: b.endpoint, AgentEndpoint: b.agentBaseURL(), Loading: loading}
	if _, err := b.client.Models(ctx); err != nil {
		info.Message = err.Error()
		return info
	}
	info.Healthy = true
	return info
}

// ListModels implements backend.Backend. LM Studio's inventory is the local
// library, so every entry is downloaded; embedding models are listed like any
// other and say so through their capabilities, as on lemonade.
func (b *Backend) ListModels(ctx context.Context) ([]backend.Model, error) {
	models, err := b.client.Models(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]backend.Model, 0, len(models))
	for _, m := range models {
		out = append(out, toModel(m))
	}
	return out, nil
}

// GetModel implements backend.Backend. LM Studio has no per-model endpoint,
// so this reads the library. Keys are case-sensitive; a reference matching
// exactly one key case-insensitively is accepted, as on lemonade.
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
		if m.Key == name {
			out := toModel(m)
			return &out, nil
		}
		if strings.EqualFold(m.Key, name) {
			folded = append(folded, m)
		}
	}
	if len(folded) == 1 {
		out := toModel(folded[0])
		return &out, nil
	}
	return nil, fmt.Errorf("%w: %s", backend.ErrNotFound, name)
}

// ListLoaded implements backend.Backend: the library entries that have a
// resident instance. LM Studio reports no per-instance memory, so SizeBytes
// is the model's size on disk.
func (b *Backend) ListLoaded(ctx context.Context) ([]backend.LoadedModel, error) {
	models, err := b.client.Models(ctx)
	if err != nil {
		return nil, err
	}
	node := hostOf(b.agentHost)
	var out []backend.LoadedModel
	for _, m := range models {
		for _, inst := range m.LoadedInstances {
			lm := backend.LoadedModel{
				Name:          m.Key,
				SizeBytes:     m.SizeBytes,
				ContextLength: inst.Config.ContextLength,
				Status:        statusLoaded,
				Node:          node,
			}
			if lm.ContextLength == 0 {
				lm.ContextLength = m.MaxContextLength
			}
			out = append(out, lm)
		}
	}
	return out, nil
}

// Pull implements backend.Backend: LM Studio downloads a model from its hub
// as a job, so the pull starts one and follows it to its end. A model that is
// already downloaded is reported complete straight away.
func (b *Backend) Pull(ctx context.Context, req backend.PullRequest, progress func(backend.Progress)) error {
	ref := strings.TrimSpace(req.Ref)
	if ref == "" {
		return fmt.Errorf("%w: empty model reference", backend.ErrInvalid)
	}
	job, err := b.client.StartDownload(ctx, ref, "")
	if err != nil {
		return mapErr(err, ref)
	}
	report := func(j *downloadJob, status string) {
		if progress == nil {
			return
		}
		progress(backend.Progress{Status: status, BytesCompleted: int64(j.DownloadedBytes), BytesTotal: int64(j.TotalSizeBytes)})
	}
	if done, err := pullDone(job, ref); done || err != nil {
		if err != nil {
			return err
		}
		return b.finishPull(ctx, job, ref, report)
	}
	if job.JobID == "" {
		return fmt.Errorf("lmstudio: pull %s: the download answered no job id", ref)
	}
	report(job, "downloading")
	ticker := time.NewTicker(b.pollInterval)
	defer ticker.Stop()
	// A status the driver does not know reads as "still going", so bound it:
	// the job context outlives the request (jobs.Manager keeps it), and a
	// pending job blocks every later pull of the same model, so an
	// unrecognised terminal state must not poll forever.
	unknown := 0
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
		job, err = b.client.DownloadStatus(ctx, job.JobID)
		if err != nil {
			return mapErr(err, ref)
		}
		done, err := pullDone(job, ref)
		if err != nil {
			return err
		}
		if done {
			return b.finishPull(ctx, job, ref, report)
		}
		if job.Status != statusDownloading {
			unknown++
			if unknown > maxUnknownStatuses {
				hint := "giving up rather than polling forever"
				if job.Status == statusPaused {
					hint = "resume or cancel it in LM Studio"
				}
				return fmt.Errorf("lmstudio: pull %s: the download has reported %q for %s, which is neither progress nor an end — %s",
					ref, job.Status, time.Duration(unknown)*b.pollInterval, hint)
			}
		} else {
			unknown = 0
		}
		report(job, describe(job))
	}
}

// finishPull closes a completed download: it resolves the reference against
// the library and reports the whole size done.
//
// The resolve is not a formality. LM Studio does not promise that the `key` a
// download lands under is the reference that was asked for — the download
// answer carries no key, there is no resolve call, and keys come from
// model.yaml (a canonical publisher/model) while Hugging Face artifacts are
// keyed per weight set and may carry an @<quant> suffix. The service wires
// the ModelConfig with the reference the CALLER gave, so a divergence would
// leave a ModelConfig whose model LM Studio's /v1 API does not serve: the
// agent's first turn would fail with a model-not-found, long after the pull
// reported success. Failing here instead names it at pull time.
//
// The size comes from the library too: LM Studio sends no counters for a
// model that was already there (already_downloaded), and a re-pull reporting
// 0 B / 0 B reads like a broken pull wherever the numbers are rendered.
func (b *Backend) finishPull(ctx context.Context, job *downloadJob, ref string, report func(*downloadJob, string)) error {
	m, err := b.GetModel(ctx, ref)
	if err != nil {
		if errors.Is(err, backend.ErrNotFound) {
			return fmt.Errorf("lmstudio: pulled %s, but the library has no model under that name: LM Studio keyed it differently (it keys Hugging Face artifacts per weight set, sometimes with an @<quant> suffix). Pull the key `GET /api/v1/models` reports, so the wired ModelConfig names a model the server serves: %w", ref, err)
		}
		return err
	}
	if job.TotalSizeBytes <= 0 && m.SizeBytes > 0 {
		job.TotalSizeBytes = float64(m.SizeBytes)
	}
	job.DownloadedBytes = job.TotalSizeBytes
	report(job, "success")
	return nil
}

// pullDone reads a download job's state: done once the model is on disk,
// an error when the download failed or stalled in a state that will not
// finish on its own.
func pullDone(job *downloadJob, ref string) (bool, error) {
	switch job.Status {
	case statusCompleted, statusAlreadyDownloaded:
		return true, nil
	case statusFailed:
		msg := job.Error
		if msg == "" {
			msg = "download failed"
		}
		return false, &APIError{Status: http.StatusOK, Code: statusFailed, Message: msg}
	default:
		// Including `paused`: not an end, and not necessarily permanent — a
		// download may sit there while it is queued. The caller's bound on
		// non-progress statuses is what stops a pull that stays paused.
		return false, nil
	}
}

// describe words a progress event.
func describe(job *downloadJob) string {
	if job.BytesPerSecond > 0 {
		return fmt.Sprintf("downloading (%s/s)", humanBytes(int64(job.BytesPerSecond)))
	}
	return "downloading"
}

// Delete implements backend.Backend. LM Studio serves no delete: a model is
// removed with `lms rm` on the host, which model-manager cannot reach. The
// missing Delete capability means the service refuses the call before it gets
// here; the sentinel keeps the contract honest for any direct caller.
func (b *Backend) Delete(ctx context.Context, name string) error {
	return fmt.Errorf("%w: delete on %s (LM Studio removes models only through its `lms rm` CLI on the host)",
		backend.ErrUnsupported, backend.NameLMStudio)
}

// Load implements backend.Backend. Loading a model that is already resident
// is a no-op, as on ollama and lemonade: LM Studio loads an *instance*, and
// one model can hold several, so a second load would pin another copy of a
// multi-GB model rather than answer "already loaded" — and load_model is
// annotated idempotent for MCP clients, which retry.
func (b *Backend) Load(ctx context.Context, req backend.LoadRequest) error {
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return fmt.Errorf("%w: empty model name", backend.ErrInvalid)
	}
	loaded, err := b.instancesOf(ctx, name)
	if err != nil {
		return err
	}
	if len(loaded) > 0 {
		return nil
	}
	if _, err := b.client.Load(ctx, name); err != nil {
		return mapErr(err, name)
	}
	return nil
}

// instancesOf are the resident instance ids of a model, nil when it holds
// none; found is false when the library does not know the model at all.
func (b *Backend) instancesOf(ctx context.Context, name string) ([]string, error) {
	models, err := b.client.Models(ctx)
	if err != nil {
		return nil, err
	}
	for _, m := range models {
		if !strings.EqualFold(m.Key, name) {
			continue
		}
		ids := make([]string, 0, len(m.LoadedInstances))
		for _, inst := range m.LoadedInstances {
			id := inst.ID
			if id == "" {
				id = m.Key
			}
			ids = append(ids, id)
		}
		return ids, nil
	}
	return nil, nil
}

// Unload implements backend.Backend. LM Studio evicts a resident instance,
// not a model, so the model's instances are looked up first; a model that is
// not loaded is a no-op, as on ollama and lemonade.
func (b *Backend) Unload(ctx context.Context, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("%w: empty model name", backend.ErrInvalid)
	}
	ids, err := b.instancesOf(ctx, name)
	if err != nil {
		return err
	}
	// A model with no resident instance — or one the library does not know
	// at all — is already in the state unload asks for.
	for _, id := range ids {
		if err := b.client.Unload(ctx, id); err != nil && !isNotLoaded(err) {
			return mapErr(err, name)
		}
	}
	return nil
}

// AgentEndpoint implements backend.Backend: kagent's OpenAI provider pointed
// at LM Studio's OpenAI-compatible API as agent pods reach it, with the
// placeholder API key the kagent runtime insists on (LM Studio needs none).
func (b *Backend) AgentEndpoint(model string) backend.AgentEndpoint {
	return backend.AgentEndpoint{Provider: ProviderOpenAI, BaseURL: b.agentBaseURL(), Model: model, PlaceholderAPIKey: true}
}

// agentBaseURL is the agent host plus AgentAPIPath (once).
func (b *Backend) agentBaseURL() string {
	host := strings.TrimRight(b.agentHost, "/")
	if strings.HasSuffix(host, AgentAPIPath) {
		return host
	}
	return host + AgentAPIPath
}

// statusLoaded is the state ListLoaded reports; LM Studio lists an instance
// only once it is resident.
const statusLoaded = "loaded"

func toModel(m apiModel) backend.Model {
	downloaded := true
	out := backend.Model{
		Name:          m.Key,
		SizeBytes:     m.SizeBytes,
		Format:        m.Format,
		Family:        m.Architecture,
		ParameterSize: m.ParamsString,
		ContextLength: m.MaxContextLength,
		Capabilities:  capabilitiesOf(m),
		Downloaded:    &downloaded,
	}
	if m.Quantization != nil {
		out.Quantization = m.Quantization.Name
	}
	return out
}

// capabilitiesOf maps LM Studio's capability object onto the vocabulary the
// other backends report (completion, tools, vision, thinking, embedding).
// An embedding model carries no capability object at all.
func capabilitiesOf(m apiModel) []string {
	if isEmbedding(m.Type) {
		return []string{"embedding"}
	}
	// Everything else is a chat model: llm, vlm, an absent type, and
	// whatever a later LM Studio adds. The capability object below says what
	// it can do beyond completions — a vlm reaches here so its vision and
	// tool-use flags are reported, instead of being filed as an embedding.
	// completion first, as ollama lists it.
	out := []string{"completion"}
	if m.Capabilities == nil {
		return out
	}
	if m.Capabilities.TrainedForToolUse {
		out = append(out, "tools")
	}
	if m.Capabilities.Vision {
		out = append(out, "vision")
	}
	if len(m.Capabilities.Reasoning) > 0 && string(m.Capabilities.Reasoning) != "null" {
		out = append(out, "thinking")
	}
	return out
}

// isEmbedding reports whether LM Studio's model type is the one type that
// does not serve completions. Both spellings: /api/v1 says embedding,
// /api/v0 said embeddings.
func isEmbedding(t string) bool {
	return t == typeEmbedding || t == typeEmbeddings
}

// hostOf is the hostname of a base URL, without scheme or port — the node
// name the proxied host is reported under.
func hostOf(base string) string {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return strings.TrimSpace(base)
	}
	return u.Hostname()
}

// humanBytes words a byte count for a progress line.
func humanBytes(n int64) string {
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	value := float64(n)
	for _, suffix := range []string{"kB", "MB", "GB", "TB"} {
		value /= unit
		if value < unit {
			return fmt.Sprintf("%.1f %s", value, suffix)
		}
	}
	return fmt.Sprintf("%.1f PB", value/unit)
}

// mapErr turns LM Studio's answers into the backend's sentinel errors.
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
		apiErr.Code == "model_not_found", apiErr.Code == "job_not_found",
		strings.Contains(msg, "not found"):
		return fmt.Errorf("%w: %s: %s", backend.ErrNotFound, name, apiErr.Message)
	case apiErr.Status == http.StatusBadRequest, apiErr.Status == http.StatusUnprocessableEntity,
		apiErr.Code == "invalid_request", apiErr.Code == "invalid_arguments":
		return fmt.Errorf("%w: %s: %s", backend.ErrInvalid, name, apiErr.Message)
	}
	return err
}

// isNotLoaded reports LM Studio's answer to unloading an instance that is not
// resident ("Model with instance identifier '…' is not loaded.", HTTP 404).
func isNotLoaded(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && strings.Contains(strings.ToLower(apiErr.Message), "not loaded")
}
