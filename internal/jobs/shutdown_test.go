package jobs

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/model-manager/internal/backend"
	"github.com/giantswarm/model-manager/internal/identity"
)

func TestJobsHaveApplicationLifetime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		lifetime, cancel := context.WithCancel(t.Context())
		defer cancel()
		manager := NewManager(WithLifetime(lifetime))
		caller := identity.ContextWithToken(identity.ContextWith(t.Context(), &identity.Identity{Issuer: "https://issuer.example.test", Subject: "user", ClientID: "cli"}), "expired-caller-token")
		entered := make(chan struct{})
		job, created, err := manager.Start(StartRequest{Type: TypePull, Model: "model", Context: caller}, func(ctx context.Context, _ func(backend.Progress)) (any, error) {
			require.Equal(t, "user", identity.Caller(ctx))
			token, ok := identity.TokenFromContext(ctx)
			require.True(t, ok)
			require.Equal(t, "expired-caller-token", token)
			close(entered)
			<-ctx.Done()
			return nil, nil
		})
		require.NoError(t, err)
		require.True(t, created)
		<-entered
		cancel()
		require.NoError(t, manager.Shutdown(t.Context()))
		finished, err := manager.Get(job.ID)
		require.NoError(t, err)
		require.Equal(t, PhaseCancelled, finished.Phase)
		_, _, err = manager.Start(StartRequest{Model: "later", Context: caller}, func(context.Context, func(backend.Progress)) (any, error) {
			t.Error("new operation started during shutdown")
			return nil, nil
		})
		require.ErrorIs(t, err, ErrShuttingDown)
		require.NoError(t, manager.Shutdown(t.Context()))
	})
}

func TestShutdownDoesNotWaitForeverForUncooperativeJob(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager := NewManager(WithAnonymousAccess(true))
		entered, release := make(chan struct{}), make(chan struct{})
		job, _, err := manager.Start(StartRequest{Model: "stuck"}, func(context.Context, func(backend.Progress)) (any, error) { close(entered); <-release; return nil, nil })
		require.NoError(t, err)
		<-entered
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		require.ErrorIs(t, manager.Shutdown(ctx), context.DeadlineExceeded)
		close(release)
		require.Equal(t, PhaseCancelled, waitDone(t, manager, job.ID).Phase)
	})
}

func TestStartRejectsExpiredApplicationLifetime(t *testing.T) {
	lifetime, cancel := context.WithCancel(t.Context())
	cancel()
	manager := NewManager(WithAnonymousAccess(true), WithLifetime(lifetime))
	_, created, err := manager.Start(StartRequest{Model: "late"}, func(context.Context, func(backend.Progress)) (any, error) {
		t.Error("operation started after application stopped")
		return nil, nil
	})
	require.ErrorIs(t, err, ErrShuttingDown)
	require.False(t, created)
	require.Empty(t, manager.List())
}
