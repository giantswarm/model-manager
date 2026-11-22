package service

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/model-manager/internal/backend"
	"github.com/giantswarm/model-manager/internal/jobs"
)

type scopedBackend struct {
	backend.Backend
	scope string
}

func (b scopedBackend) Name() backend.Name              { return backend.NameKServe }
func (b scopedBackend) JobScope(context.Context) string { return b.scope }

func TestApplicationCoordinationCannotCancelAnotherBackendInstance(t *testing.T) {
	manager := jobs.NewManager()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), time.Second)
		defer cancel()
		require.NoError(t, manager.Shutdown(ctx))
	})
	service := &Service{jobs: manager, log: slog.Default()}
	old, current := scopedBackend{scope: "old-target"}, scopedBackend{scope: "current-target"}
	start := func(driver scopedBackend) jobs.Job {
		t.Helper()
		job, _, err := manager.Start(jobs.StartRequest{Type: jobs.TypeLoad, Backend: driver.Name(), Model: "model", Scope: driver.JobScope(t.Context()), ApplicationOwned: true}, func(ctx context.Context, _ func(backend.Progress)) (any, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		})
		require.NoError(t, err)
		return job
	}
	previous := start(old)
	require.False(t, service.hasActiveJob(t.Context(), jobs.TypeLoad, current, "model"))
	active := start(current)
	require.True(t, service.hasActiveJob(t.Context(), jobs.TypeLoad, current, "model"))
	service.endLoadJob(t.Context(), current, "model")
	finished, err := manager.Get(active.ID)
	require.NoError(t, err)
	require.Equal(t, jobs.PhaseCancelled, finished.Phase)
	untouched, err := manager.Get(previous.ID)
	require.NoError(t, err)
	require.False(t, untouched.Done())
}
