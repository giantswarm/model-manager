package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/model-manager/internal/backend"
)

func waitDone(t *testing.T, m *Manager, id string) Job {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		j, err := m.Get(id)
		require.NoError(t, err)
		if j.Done() {
			return j
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("job %s did not finish", id)
	return Job{}
}

func TestStartRunsAndReportsProgress(t *testing.T) {
	m := NewManager()
	job, created := m.Start(StartRequest{Type: TypePull, Model: "smollm2:135m", Wire: true}, func(_ context.Context, report func(backend.Progress)) (any, error) {
		report(backend.Progress{Status: "pulling manifest"})
		report(backend.Progress{Status: "pulling", BytesCompleted: 50, BytesTotal: 200})
		report(backend.Progress{Status: "pulling", BytesCompleted: 150, BytesTotal: 200})
		return map[string]string{"name": "smollm2-135m"}, nil
	})
	require.True(t, created)
	assert.Equal(t, PhasePending, job.Phase)
	assert.True(t, job.Wire)

	done := waitDone(t, m, job.ID)
	assert.Equal(t, PhaseSucceeded, done.Phase)
	assert.Equal(t, int64(200), done.BytesCompleted)
	assert.Equal(t, int64(200), done.BytesTotal)
	assert.Equal(t, 100.0, done.Percent)
	assert.Equal(t, map[string]string{"name": "smollm2-135m"}, done.Result)
	assert.NotNil(t, done.StartedAt)
	assert.NotNil(t, done.FinishedAt)
}

func TestStartEchoesPlacementAndProgressRefinesIt(t *testing.T) {
	m := NewManager()
	// The request named a preset but left the node open: the job echoes the
	// preset at once and takes the node the backend reports once it picked one.
	job, created := m.Start(StartRequest{Type: TypePull, Model: "org/tiny", Preset: "tiny"}, func(_ context.Context, report func(backend.Progress)) (any, error) {
		report(backend.Progress{Status: "download job created", Node: "gpu1", Preset: "tiny"})
		report(backend.Progress{Status: "downloading", BytesCompleted: 5, BytesTotal: 10})
		return nil, nil
	})
	require.True(t, created)
	assert.Equal(t, "tiny", job.Preset)
	assert.Empty(t, job.Node, "not known before the backend ran its fit check")
	done := waitDone(t, m, job.ID)
	assert.Equal(t, "gpu1", done.Node)
	assert.Equal(t, "tiny", done.Preset, "a sample without placement leaves the values alone")

	// A named node is echoed in the initial snapshot already.
	pinned, _ := m.Start(StartRequest{Type: TypePull, Model: "org/other", Node: "gpu2"}, func(context.Context, func(backend.Progress)) (any, error) { return nil, nil })
	assert.Equal(t, "gpu2", pinned.Node)
	assert.Empty(t, pinned.Preset)

	// Backends without placement never set either; the JSON omits both.
	bare, _ := m.Start(StartRequest{Type: TypePull, Model: "smollm2:135m"}, func(context.Context, func(backend.Progress)) (any, error) { return nil, nil })
	raw, err := json.Marshal(waitDone(t, m, bare.ID))
	require.NoError(t, err)
	assert.NotContains(t, string(raw), `"node"`)
	assert.NotContains(t, string(raw), `"preset"`)
}

func TestStartJoinsActiveJobForSameModel(t *testing.T) {
	m := NewManager()
	release := make(chan struct{})
	first, created := m.Start(StartRequest{Type: TypePull, Model: "a:1"}, func(ctx context.Context, _ func(backend.Progress)) (any, error) {
		<-release
		return nil, nil
	})
	require.True(t, created)
	second, created := m.Start(StartRequest{Type: TypePull, Model: "a:1"}, func(context.Context, func(backend.Progress)) (any, error) { return nil, nil })
	assert.False(t, created)
	assert.Equal(t, first.ID, second.ID)
	other, created := m.Start(StartRequest{Type: TypePull, Model: "b:1"}, func(context.Context, func(backend.Progress)) (any, error) { return nil, nil })
	assert.True(t, created)
	assert.NotEqual(t, first.ID, other.ID)
	close(release)
	m.Wait()
	assert.Len(t, m.List(), 2)
}

func TestFailureAndCancel(t *testing.T) {
	m := NewManager()
	failed, _ := m.Start(StartRequest{Type: TypePull, Model: "bad:1"}, func(context.Context, func(backend.Progress)) (any, error) {
		return nil, errors.New("manifest not found")
	})
	j := waitDone(t, m, failed.ID)
	assert.Equal(t, PhaseFailed, j.Phase)
	assert.Equal(t, "manifest not found", j.Error)

	started := make(chan struct{})
	var once sync.Once
	running, _ := m.Start(StartRequest{Type: TypePull, Model: "slow:1"}, func(ctx context.Context, report func(backend.Progress)) (any, error) {
		once.Do(func() { close(started) })
		<-ctx.Done()
		return nil, ctx.Err()
	})
	<-started
	j, err := m.Cancel(running.ID)
	require.NoError(t, err)
	j = waitDone(t, m, j.ID)
	assert.Equal(t, PhaseCancelled, j.Phase)

	_, err = m.Cancel("missing")
	require.ErrorIs(t, err, ErrNotFound)
	_, err = m.Get("missing")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestListOrderAndPrune(t *testing.T) {
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	m := NewManager(WithClock(clock), WithRetention(time.Hour), WithMaxJobs(1))
	a, _ := m.Start(StartRequest{Type: TypePull, Model: "a:1"}, func(context.Context, func(backend.Progress)) (any, error) { return nil, nil })
	waitDone(t, m, a.ID)
	now = now.Add(time.Minute)
	b, _ := m.Start(StartRequest{Type: TypePull, Model: "b:1"}, func(context.Context, func(backend.Progress)) (any, error) { return nil, nil })
	waitDone(t, m, b.ID)

	list := m.List()
	require.Len(t, list, 1, "maxJobs keeps only the newest finished job")
	assert.Equal(t, b.ID, list[0].ID)

	now = now.Add(2 * time.Hour)
	assert.Empty(t, m.List(), "retention window drops finished jobs")
}
