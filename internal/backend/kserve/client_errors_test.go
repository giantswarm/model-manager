package kserve

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

func TestClientSelectionErrorsNeverUseConfiguredClients(t *testing.T) {
	for _, failure := range []string{"construction error", "missing clients"} {
		t.Run(failure, func(t *testing.T) {
			fixture := newFixture(t)
			selectionError := errors.New("caller client construction failed")
			fixture.b.opts.ClientsFor = func(context.Context) (kubernetes.Interface, dynamic.Interface, error) {
				if failure == "missing clients" {
					return nil, nil, nil
				}
				return nil, nil, selectionError
			}
			fixture.b.cfg.opts.ClientsFor = fixture.b.opts.ClientsFor
			fixture.cs.ClearActions()
			fixture.dyn.ClearActions()
			tests := []struct {
				name string
				call func(context.Context) error
			}{
				{"presets", func(ctx context.Context) error { _, _, err := fixture.b.presets(ctx); return err }},
				{"served models", func(ctx context.Context) error { _, err := fixture.b.listServed(ctx); return err }},
				{"get serving", func(ctx context.Context) error { _, err := fixture.b.getServing(ctx, "default", "model"); return err }},
				{"delete serving", func(ctx context.Context) error { return fixture.b.deleteServing(ctx, "default", "model") }},
				{"delete job", func(ctx context.Context) error { return fixture.b.deleteJob(ctx, "default", "job") }},
				{"discovery", func(ctx context.Context) error { _, err := fixture.b.cfg.discover(ctx); return err }},
				{"control plane", func(ctx context.Context) error { _, _, err := fixture.b.cfg.controlPlane(ctx); return err }},
			}
			for _, test := range tests {
				t.Run(test.name, func(t *testing.T) {
					err := test.call(t.Context())
					if failure == "missing clients" {
						require.Error(t, err)
					} else {
						require.ErrorIs(t, err, selectionError)
					}
				})
			}
			require.Empty(t, fixture.cs.Actions())
			require.Empty(t, fixture.dyn.Actions())
		})
	}
}
