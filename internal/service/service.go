// Package service orchestrates the backend driver, the job manager and the
// agent wiring behind one backend-agnostic API that both the REST and the MCP
// surfaces expose.
package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/giantswarm/model-manager/internal/backend"
	"github.com/giantswarm/model-manager/internal/jobs"
	"github.com/giantswarm/model-manager/internal/wiring"
)

// ErrWiringDisabled is returned for wire operations when no Kubernetes access
// was configured.
var ErrWiringDisabled = fmt.Errorf("%w: agent wiring is disabled (no Kubernetes access)", backend.ErrUnsupported)

// Config tunes the service.
type Config struct {
	// AutoWire creates a ModelConfig when a pull completes or a model is
	// loaded, unless the request opts out.
	AutoWire bool
	// DefaultKeepAlive is passed to Load when the request has none.
	DefaultKeepAlive string
}

// BackendResponse is the backend identity plus effective capabilities.
type BackendResponse struct {
	backend.Info
	Capabilities backend.Capabilities `json:"capabilities"`
	// Wiring describes where ModelConfigs are created, when wiring is enabled.
	Wiring *WiringInfo `json:"wiring,omitempty"`
}

// WiringInfo describes the agent-wiring target.
type WiringInfo struct {
	Namespace  string `json:"namespace"`
	APIVersion string `json:"apiVersion,omitempty"`
	AutoWire   bool   `json:"autoWire"`
}

// ModelView is a downloaded model enriched with loaded state and wiring.
type ModelView struct {
	backend.Model
	Loaded      bool                   `json:"loaded"`
	Running     *backend.LoadedModel   `json:"running,omitempty"`
	ModelConfig *wiring.ModelConfigRef `json:"modelConfig,omitempty"`
}

// Service is the orchestration layer.
type Service struct {
	backend backend.Backend
	jobs    *jobs.Manager
	wirer   wiring.Wirer
	wiring  *WiringInfo
	cfg     Config
	log     *slog.Logger
}

// New builds a Service. wirer may be nil (wiring disabled).
func New(b backend.Backend, jm *jobs.Manager, wirer wiring.Wirer, info *WiringInfo, cfg Config, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	if info != nil {
		info.AutoWire = cfg.AutoWire
	}
	return &Service{backend: b, jobs: jm, wirer: wirer, wiring: info, cfg: cfg, log: log}
}

// Capabilities returns the effective flags: the driver's plus Wire.
func (s *Service) Capabilities() backend.Capabilities {
	caps := s.backend.Capabilities()
	caps.Wire = s.wirer != nil
	return caps
}

// Backend reports identity, health and capabilities.
func (s *Service) Backend(ctx context.Context) BackendResponse {
	resp := BackendResponse{Info: s.backend.Info(ctx), Capabilities: s.Capabilities()}
	if s.wirer != nil {
		resp.Wiring = s.wiring
	}
	return resp
}

// Ready reports whether the backend answers.
func (s *Service) Ready(ctx context.Context) error {
	info := s.backend.Info(ctx)
	if !info.Healthy {
		return fmt.Errorf("backend %s not healthy: %s", info.Backend, info.Message)
	}
	return nil
}

// ListModels returns the downloaded models enriched with loaded state and
// ModelConfig references. Enrichment failures are logged, not fatal.
func (s *Service) ListModels(ctx context.Context) ([]ModelView, error) {
	models, err := s.backend.ListModels(ctx)
	if err != nil {
		return nil, err
	}
	loaded := s.loadedIndex(ctx)
	wired := s.wiredIndex(ctx)
	out := make([]ModelView, 0, len(models))
	for _, m := range models {
		out = append(out, s.view(m, loaded, wired))
	}
	return out, nil
}

// GetModel returns one enriched model.
func (s *Service) GetModel(ctx context.Context, name string) (*ModelView, error) {
	m, err := s.backend.GetModel(ctx, name)
	if err != nil {
		return nil, err
	}
	v := s.view(*m, s.loadedIndex(ctx), s.wiredIndex(ctx))
	return &v, nil
}

// ListLoaded returns the loaded/running models.
func (s *Service) ListLoaded(ctx context.Context) ([]backend.LoadedModel, error) {
	if !s.backend.Capabilities().LoadedModels {
		return nil, fmt.Errorf("%w: listing loaded models", backend.ErrUnsupported)
	}
	return s.backend.ListLoaded(ctx)
}

// Pull starts (or joins) an import job. wire nil means the AutoWire default.
func (s *Service) Pull(ctx context.Context, ref string, wire *bool) (jobs.Job, bool, error) {
	if !s.backend.Capabilities().Pull {
		return jobs.Job{}, false, fmt.Errorf("%w: pull", backend.ErrUnsupported)
	}
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return jobs.Job{}, false, fmt.Errorf("%w: model reference is required", backend.ErrInvalid)
	}
	doWire := s.cfg.AutoWire && s.wirer != nil
	if wire != nil {
		doWire = *wire
	}
	if doWire && s.wirer == nil {
		return jobs.Job{}, false, ErrWiringDisabled
	}
	job, created := s.jobs.Start(jobs.StartRequest{Type: jobs.TypePull, Model: ref, Wire: doWire},
		func(jobCtx context.Context, report func(backend.Progress)) (any, error) {
			if err := s.backend.Pull(jobCtx, backend.PullRequest{Ref: ref}, report); err != nil {
				return nil, err
			}
			if !doWire {
				return nil, nil
			}
			mcRef, err := s.wireModel(jobCtx, ref)
			if err != nil {
				return nil, fmt.Errorf("pulled %s but wiring failed: %w", ref, err)
			}
			return mcRef, nil
		})
	if created {
		s.log.Info("pull started", "model", ref, "job", job.ID, "wire", doWire)
	}
	return job, created, nil
}

// Load loads/serves a model and, with AutoWire, ensures its ModelConfig.
func (s *Service) Load(ctx context.Context, name, keepAlive string) (*ModelView, error) {
	if !s.backend.Capabilities().Load {
		return nil, fmt.Errorf("%w: load", backend.ErrUnsupported)
	}
	m, err := s.backend.GetModel(ctx, name)
	if err != nil {
		return nil, err
	}
	if keepAlive == "" {
		keepAlive = s.cfg.DefaultKeepAlive
	}
	if err := s.backend.Load(ctx, backend.LoadRequest{Name: m.Name, KeepAlive: keepAlive}); err != nil {
		return nil, err
	}
	s.log.Info("model loaded", "model", m.Name, "keepAlive", keepAlive)
	if s.cfg.AutoWire && s.wirer != nil {
		if _, err := s.wireModel(ctx, m.Name); err != nil {
			s.log.Warn("auto-wire after load failed", "model", m.Name, "error", err)
		}
	}
	return s.GetModel(ctx, m.Name)
}

// Unload evicts a model.
func (s *Service) Unload(ctx context.Context, name string) error {
	if !s.backend.Capabilities().Unload {
		return fmt.Errorf("%w: unload", backend.ErrUnsupported)
	}
	m, err := s.backend.GetModel(ctx, name)
	if err != nil {
		return err
	}
	if err := s.backend.Unload(ctx, m.Name); err != nil {
		return err
	}
	s.log.Info("model unloaded", "model", m.Name)
	return nil
}

// Delete removes a downloaded model, unwiring it first when requested.
func (s *Service) Delete(ctx context.Context, name string, unwire bool) error {
	if !s.backend.Capabilities().Delete {
		return fmt.Errorf("%w: delete", backend.ErrUnsupported)
	}
	m, err := s.backend.GetModel(ctx, name)
	if err != nil {
		return err
	}
	if unwire && s.wirer != nil {
		if err := s.wirer.Remove(ctx, m.Name); err != nil {
			return fmt.Errorf("unwire %s: %w", m.Name, err)
		}
	}
	if err := s.backend.Delete(ctx, m.Name); err != nil {
		return err
	}
	s.log.Info("model deleted", "model", m.Name, "unwired", unwire && s.wirer != nil)
	return nil
}

// Wire creates the ModelConfig for an existing model.
func (s *Service) Wire(ctx context.Context, name string) (*wiring.ModelConfigRef, error) {
	if s.wirer == nil {
		return nil, ErrWiringDisabled
	}
	m, err := s.backend.GetModel(ctx, name)
	if err != nil {
		return nil, err
	}
	return s.wireModel(ctx, m.Name)
}

// Unwire removes the ModelConfig for a model (which need not exist anymore).
func (s *Service) Unwire(ctx context.Context, name string) error {
	if s.wirer == nil {
		return ErrWiringDisabled
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("%w: model name is required", backend.ErrInvalid)
	}
	if m, err := s.backend.GetModel(ctx, name); err == nil {
		name = m.Name
	}
	if err := s.wirer.Remove(ctx, name); err != nil {
		return err
	}
	s.log.Info("model unwired", "model", name)
	return nil
}

// Jobs lists jobs, newest first.
func (s *Service) Jobs() []jobs.Job { return s.jobs.List() }

// Job returns one job.
func (s *Service) Job(id string) (jobs.Job, error) { return s.jobs.Get(id) }

// CancelJob cancels a running job.
func (s *Service) CancelJob(id string) (jobs.Job, error) { return s.jobs.Cancel(id) }

func (s *Service) wireModel(ctx context.Context, model string) (*wiring.ModelConfigRef, error) {
	// Resolve the canonical name so "smollm2:135m" and "smollm2:135m" pulled
	// as "smollm2" end up in one ModelConfig.
	if m, err := s.backend.GetModel(ctx, model); err == nil {
		model = m.Name
	}
	ref, err := s.wirer.Ensure(ctx, model, s.backend.AgentEndpoint(model))
	if err != nil {
		return nil, err
	}
	s.log.Info("model wired", "model", model, "modelConfig", ref.Namespace+"/"+ref.Name)
	return ref, nil
}

func (s *Service) loadedIndex(ctx context.Context) map[string]backend.LoadedModel {
	idx := map[string]backend.LoadedModel{}
	if !s.backend.Capabilities().LoadedModels {
		return idx
	}
	loaded, err := s.backend.ListLoaded(ctx)
	if err != nil {
		s.log.Warn("listing loaded models failed", "error", err)
		return idx
	}
	for _, l := range loaded {
		idx[l.Name] = l
	}
	return idx
}

func (s *Service) wiredIndex(ctx context.Context) map[string]wiring.ModelConfigRef {
	if s.wirer == nil {
		return nil
	}
	wired, err := s.wirer.List(ctx)
	if err != nil {
		s.log.Warn("listing ModelConfigs failed", "error", err)
		return nil
	}
	return wired
}

func (s *Service) view(m backend.Model, loaded map[string]backend.LoadedModel, wired map[string]wiring.ModelConfigRef) ModelView {
	v := ModelView{Model: m}
	if l, ok := loaded[m.Name]; ok {
		v.Loaded = true
		lc := l
		v.Running = &lc
	}
	if mc, ok := wired[m.Name]; ok {
		mcc := mc
		v.ModelConfig = &mcc
	}
	return v
}

// IsNotFound reports whether err is a not-found from the backend or jobs.
func IsNotFound(err error) bool {
	return errors.Is(err, backend.ErrNotFound) || errors.Is(err, jobs.ErrNotFound)
}
