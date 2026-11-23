package cmd

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

func TestShutdownBudgetStartsOnceAndReservesTelemetry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		budget, err := newShutdownBudget(t.Context(), defaultShutdownTimeout)
		require.NoError(t, err)
		defer budget.Close()
		time.Sleep(time.Hour)
		started := time.Now()
		work := budget.WorkContext()
		deadline, ok := work.Deadline()
		require.True(t, ok)
		require.Equal(t, started.Add(defaultShutdownTimeout-telemetryShutdownReserve), deadline)
		process := budget.Context()
		time.Sleep(defaultShutdownTimeout - telemetryShutdownReserve)
		synctest.Wait()
		require.ErrorIs(t, work.Err(), context.DeadlineExceeded)
		require.NoError(t, process.Err())
		require.Same(t, work, budget.WorkContext())
		time.Sleep(telemetryShutdownReserve)
		synctest.Wait()
		require.ErrorIs(t, process.Err(), context.DeadlineExceeded)
	})
}

func TestShutdownBudgetIgnoresServingCancellation(t *testing.T) {
	serving, cancel := context.WithCancel(t.Context())
	cancel()
	budget, err := newShutdownBudget(serving, defaultShutdownTimeout)
	require.NoError(t, err)
	require.NoError(t, budget.Context().Err())
	budget.Close()
	require.ErrorIs(t, budget.Context().Err(), context.Canceled)
}

func TestShutdownBudgetRejectsInsufficientReserve(t *testing.T) {
	for _, duration := range []time.Duration{-time.Second, 0, telemetryShutdownReserve} {
		_, err := newShutdownBudget(t.Context(), duration)
		require.Error(t, err)
	}
}
