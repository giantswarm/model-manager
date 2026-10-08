package kserve

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/giantswarm/model-manager/internal/backend"
)

// Catalog reads the serving presets the platform's connectivity chart
// publishes where model-manager runs — the preset ConfigMaps of the preset
// namespace the chart values or the discovery document name — without a
// registered backend, and answers what a preset declares. A person sizing a
// GPU pool learns what the preset needs before the pool, and with it the
// backend, exists (giantswarm/model-manager#274). A registered kserve backend
// reads the same catalog on its serving target; the catalog reads the local
// cluster alone.
type Catalog struct {
	opts backend.KServeOptions
	cfg  *config
	log  *slog.Logger
}

// NewCatalog builds the catalog over the local cluster's clients.
func NewCatalog(opts backend.KServeOptions) (*Catalog, error) {
	if opts.Clientset == nil {
		return nil, fmt.Errorf("the preset catalog needs Kubernetes access")
	}
	applyDefaults(&opts)
	log := slog.Default().With("component", "preset-catalog")
	return &Catalog{opts: opts, cfg: newConfig(opts, log), log: log}, nil
}

// ListPresets implements backend.PresetLister from the catalog.
func (c *Catalog) ListPresets(ctx context.Context) ([]backend.Preset, error) {
	s, presets, err := c.read(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]backend.Preset, 0, len(presets))
	for _, p := range presets {
		out = append(out, p.view(c.opts.DefaultOverheadGiB, s.TemplateImage))
	}
	return out, nil
}

// FitCheck implements backend.FitChecker without a backend: the preset's
// declaration as the numbers, Verdict unverified and Fits false — nothing
// judged the model against a node or a pool size, and a load has nothing to
// compose onto. The preset is resolved the way a backend resolves it: by
// name, or the single preset serving the model. A model no preset serves is
// ErrNoBackend (its weights are the hub's to size, which a backend asks), as
// is an empty catalog, naming where the chart publishes it; an unknown preset
// is ErrNotFound naming the published ones.
func (c *Catalog) FitCheck(ctx context.Context, req backend.FitRequest) (*backend.FitResult, error) {
	s, presets, err := c.read(ctx)
	if err != nil {
		return nil, err
	}
	if len(presets) == 0 {
		return nil, fmt.Errorf("%w: no serving preset is published in namespace %s (ConfigMaps labelled %s)", backend.ErrNoBackend, s.PresetNamespace, s.PresetSelector)
	}
	idx := indexPresets(presets)
	model := strings.TrimSpace(req.Model)
	p, err := idx.resolve(model, strings.TrimSpace(req.Preset))
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, fmt.Errorf("%w: no serving preset serves %s, and without a backend only a preset's declaration is answered (presets: %s)", backend.ErrNoBackend, model, strings.Join(idx.names(presets), ", "))
	}
	return c.declared(p, idx), nil
}

// read lists the published presets with the first client that answers: the
// caller's, then the configured one, as the discovery document is read.
func (c *Catalog) read(ctx context.Context) (settings, []*servingPreset, error) {
	s := c.cfg.settings(ctx)
	var errs []error
	for _, cs := range c.cfg.clientsets(ctx) {
		presets, warnings, err := listPresets(ctx, cs, s)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, w := range warnings {
			c.log.Warn("skipping unusable preset", "detail", w)
		}
		return s, presets, nil
	}
	return s, nil, errors.Join(errs...)
}

// declared is the unverified answer for p: what it declares, in the fields a
// judged answer carries them in, and its own shape as the devices (no node
// derived one).
func (c *Catalog) declared(p *servingPreset, idx presetIndex) *backend.FitResult {
	weights := p.weightsBytes()
	overhead := p.overheadBytes(c.opts.DefaultOverheadGiB)
	res := &backend.FitResult{
		Model:                     p.Spec.Model.ID,
		Verdict:                   backend.VerdictUnverified,
		Retryable:                 true,
		Preset:                    p.name(),
		Presets:                   idx.names(idx.forModel(p.Spec.Model.ID)),
		WeightsBytes:              weights,
		WeightsSource:             weightsSourcePreset,
		DeclaredWeightsBytes:      weights,
		OverheadBytes:             overhead,
		RequiredBytes:             weights + overhead,
		ComputeCapabilityRequired: p.Spec.Requirements.MinComputeCapability,
		DevicesPerPod:             p.gpus(),
		TensorParallel:            p.gpus(),
		GPUMemoryUtilization:      p.utilization(),
		CacheSource:               backend.CacheSourceUnknown,
	}
	flags := vllmFlagValues(p.Spec.Args)
	if v, ok := flags[flagTensorParallelSize]; ok {
		if n, err := humanReadableInt(v); err == nil && n > 0 {
			res.TensorParallel = n
		}
	}
	if v, ok := flags[flagMaxModelLen]; ok {
		if n, err := humanReadableInt(v); err == nil && n > 0 {
			res.MaxModelLen = n
		}
	}
	res.Reason = declarationOf(p, res)
	return res
}

// declarationOf words what the preset declares.
func declarationOf(p *servingPreset, res *backend.FitResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "the fit of preset %s is unverified until a backend is registered: it declares ", p.name())
	switch gpus := p.gpus(); gpus {
	case 0:
		b.WriteString("no GPU (CPU)")
	case 1:
		b.WriteString("1 GPU")
	default:
		fmt.Fprintf(&b, "%d GPUs", gpus)
	}
	fmt.Fprintf(&b, ", %s of weights and %s of overhead (%s required)", humanBytes(res.WeightsBytes), humanBytes(res.OverheadBytes), humanBytes(res.RequiredBytes))
	if cc := res.ComputeCapabilityRequired; cc != "" {
		fmt.Fprintf(&b, ", compute capability %s or newer", cc)
	}
	if res.MaxModelLen > 0 {
		fmt.Fprintf(&b, ", %s %d", flagMaxModelLen, res.MaxModelLen)
	}
	return b.String()
}
