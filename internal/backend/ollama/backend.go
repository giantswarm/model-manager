// Package ollama implements the serving backend over a host Ollama API: a thin
// proxy of /api/tags, /api/ps, /api/pull, /api/delete and keep_alive-driven
// load/unload. Ollama does downloads and memory management itself, so there
// are no Jobs, presets or fit-checks here — those are kserve concerns. The
// node inventory is the proxied host itself (nodes.go).
package ollama

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
	// DefaultKeepAlive is Ollama's own default idle timeout.
	DefaultKeepAlive = "5m"
	latestTag        = ":latest"
)

// Backend is the ollama driver.
type Backend struct {
	client    *Client
	endpoint  string
	agentHost string
	// meminfoPath is the /proc/meminfo the host node's budget is read from.
	meminfoPath string
}

// Factory builds the driver from backend.Options.
func Factory(opts backend.Options) (backend.Backend, error) {
	return New(opts.Ollama)
}

// New builds the driver. AgentHost defaults to Endpoint.
func New(opts backend.OllamaOptions) (*Backend, error) {
	endpoint := strings.TrimRight(strings.TrimSpace(opts.Endpoint), "/")
	if endpoint == "" {
		return nil, fmt.Errorf("ollama endpoint is required")
	}
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("ollama endpoint %q must be an http(s) URL", opts.Endpoint)
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
	return &Backend{client: NewClient(endpoint, hc), endpoint: endpoint, agentHost: agentHost, meminfoPath: procMeminfo}, nil
}

// NewWithClient builds the driver around an existing client (tests).
func NewWithClient(c *Client, endpoint, agentHost string) *Backend {
	if agentHost == "" {
		agentHost = endpoint
	}
	return &Backend{client: c, endpoint: endpoint, agentHost: agentHost, meminfoPath: procMeminfo}
}

// Name implements backend.Backend.
func (b *Backend) Name() backend.Name { return backend.NameOllama }

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

// Info implements backend.Backend. AgentEndpoint is the host agents dial (the
// one written into ModelConfigs), Endpoint the one model-manager dials.
func (b *Backend) Info(ctx context.Context) backend.Info {
	info := backend.Info{Backend: backend.NameOllama, Endpoint: b.endpoint, AgentEndpoint: b.agentHost}
	v, err := b.client.Version(ctx)
	if err != nil {
		info.Message = err.Error()
		return info
	}
	info.Version = v
	info.Healthy = true
	return info
}

// ListModels implements backend.Backend (downloaded models, /api/tags).
func (b *Backend) ListModels(ctx context.Context) ([]backend.Model, error) {
	tags, err := b.client.Tags(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]backend.Model, 0, len(tags))
	for _, t := range tags {
		out = append(out, toModel(t))
	}
	return out, nil
}

// GetModel implements backend.Backend. "name" and "name:latest" are the same
// model, as in Ollama itself.
func (b *Backend) GetModel(ctx context.Context, name string) (*backend.Model, error) {
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("%w: empty model name", backend.ErrInvalid)
	}
	tags, err := b.client.Tags(ctx)
	if err != nil {
		return nil, err
	}
	for _, t := range tags {
		if sameRef(t.Name, name) {
			m := toModel(t)
			return &m, nil
		}
	}
	return nil, fmt.Errorf("%w: %s", backend.ErrNotFound, name)
}

// ListLoaded implements backend.Backend (/api/ps).
func (b *Backend) ListLoaded(ctx context.Context) ([]backend.LoadedModel, error) {
	ps, err := b.client.PS(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]backend.LoadedModel, 0, len(ps))
	for _, p := range ps {
		lm := backend.LoadedModel{
			Name:          p.Name,
			Digest:        p.Digest,
			SizeBytes:     p.Size,
			VRAMBytes:     p.SizeVRAM,
			ContextLength: p.ContextLength,
			Status:        "loaded",
		}
		if p.ExpiresAt != nil && !p.ExpiresAt.IsZero() {
			t := *p.ExpiresAt
			lm.ExpiresAt = &t
		}
		out = append(out, lm)
	}
	return out, nil
}

// Pull implements backend.Backend. Ollama streams per-layer progress; the
// driver sums the layers so BytesCompleted/BytesTotal describe the whole pull.
func (b *Backend) Pull(ctx context.Context, req backend.PullRequest, progress func(backend.Progress)) error {
	ref := strings.TrimSpace(req.Ref)
	if ref == "" {
		return fmt.Errorf("%w: empty model reference", backend.ErrInvalid)
	}
	type layer struct{ total, completed int64 }
	layers := map[string]*layer{}
	order := []string{}
	err := b.client.Pull(ctx, ref, func(ev pullEvent) {
		if ev.Digest != "" {
			l, ok := layers[ev.Digest]
			if !ok {
				l = &layer{}
				layers[ev.Digest] = l
				order = append(order, ev.Digest)
			}
			if ev.Total > 0 {
				l.total = ev.Total
			}
			if ev.Completed > l.completed {
				l.completed = ev.Completed
			}
		}
		if progress == nil {
			return
		}
		p := backend.Progress{Status: ev.Status}
		for _, d := range order {
			p.BytesTotal += layers[d].total
			p.BytesCompleted += layers[d].completed
		}
		if ev.Status == "success" {
			p.BytesCompleted = p.BytesTotal
		}
		progress(p)
	})
	if err != nil {
		return mapErr(err, ref)
	}
	return nil
}

// Delete implements backend.Backend.
func (b *Backend) Delete(ctx context.Context, name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("%w: empty model name", backend.ErrInvalid)
	}
	return mapErr(b.client.Delete(ctx, name), name)
}

// Load implements backend.Backend: a generate call with a positive keep_alive
// loads the model and keeps it resident for that long after the last request.
func (b *Backend) Load(ctx context.Context, req backend.LoadRequest) error {
	if strings.TrimSpace(req.Name) == "" {
		return fmt.Errorf("%w: empty model name", backend.ErrInvalid)
	}
	keepAlive := strings.TrimSpace(req.KeepAlive)
	if keepAlive == "" {
		keepAlive = DefaultKeepAlive
	}
	if keepAlive == "0" || keepAlive == "0s" {
		return fmt.Errorf("%w: keepAlive must be positive to load (use unload)", backend.ErrInvalid)
	}
	return mapErr(b.client.SetKeepAlive(ctx, req.Name, keepAlive), req.Name)
}

// Unload implements backend.Backend: keep_alive 0 evicts the model.
func (b *Backend) Unload(ctx context.Context, name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("%w: empty model name", backend.ErrInvalid)
	}
	return mapErr(b.client.SetKeepAlive(ctx, name, 0), name)
}

// AgentEndpoint implements backend.Backend: kagent's native keyless Ollama
// provider pointed at the host as agent pods reach it.
func (b *Backend) AgentEndpoint(model string) backend.AgentEndpoint {
	return backend.AgentEndpoint{Provider: "Ollama", Host: b.agentHost, Model: model}
}

func toModel(t apiModel) backend.Model {
	return backend.Model{
		Name:          t.Name,
		Digest:        t.Digest,
		SizeBytes:     t.Size,
		ModifiedAt:    t.ModifiedAt,
		Format:        t.Details.Format,
		Family:        t.Details.Family,
		ParameterSize: t.Details.ParameterSize,
		Quantization:  t.Details.QuantizationLevel,
		ContextLength: t.Details.ContextLength,
		Capabilities:  t.Capabilities,
	}
}

// sameRef reports whether two Ollama references name the same model,
// treating a missing tag as ":latest".
func sameRef(a, b string) bool {
	return canonical(a) == canonical(b)
}

func canonical(ref string) string {
	ref = strings.TrimSpace(ref)
	// The tag is after the last ':' that follows the last '/' (hf.co/org/repo:Q4).
	slash := strings.LastIndex(ref, "/")
	if strings.LastIndex(ref, ":") <= slash {
		ref += latestTag
	}
	return ref
}

func mapErr(err error, name string) error {
	if err == nil {
		return nil
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		msg := strings.ToLower(apiErr.Message)
		if apiErr.Status == http.StatusNotFound || strings.Contains(msg, "not found") || strings.Contains(msg, "file does not exist") {
			return fmt.Errorf("%w: %s: %s", backend.ErrNotFound, name, apiErr.Message)
		}
	}
	return err
}
