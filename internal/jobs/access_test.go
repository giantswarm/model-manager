package jobs

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/model-manager/internal/backend"
	"github.com/giantswarm/model-manager/internal/identity"
)

func authenticatedCaller(t *testing.T, subject string) context.Context {
	t.Helper()
	return identity.ContextWith(t.Context(), &identity.Identity{Issuer: "https://issuer.example.test", Subject: subject, ClientID: "client", Email: "display@example.test", Scopes: []string{"mcp:access"}})
}

func managedJobs(t *testing.T) *Manager {
	t.Helper()
	manager := NewManager()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), time.Second)
		defer cancel()
		require.NoError(t, manager.Shutdown(ctx))
	})
	return manager
}

func blockingOperation(ctx context.Context, _ func(backend.Progress)) (any, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestTaskIdentityIsPrivateAndStableAcrossProfileChanges(t *testing.T) {
	manager := managedJobs(t)
	alice := authenticatedCaller(t, "alice")
	job, created, err := manager.Start(StartRequest{Type: TypePull, Backend: "backend", Model: "model", Context: alice}, blockingOperation)
	require.NoError(t, err)
	require.True(t, created)
	for _, test := range []struct {
		name   string
		change func(*identity.Identity)
	}{
		{"different subject", func(c *identity.Identity) { c.Subject = "bob" }},
		{"different issuer", func(c *identity.Identity) { c.Issuer = "https://other-connector.example.test" }},
		{"different client", func(c *identity.Identity) { c.ClientID = "other-client" }},
		{"delegated actor", func(c *identity.Identity) { c.ActorIssuer = "https://agents.example.test"; c.ActorSubject = "agent" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			original, _ := identity.FromContext(alice)
			caller := *original
			test.change(&caller)
			ctx := identity.ContextWith(t.Context(), &caller)
			_, err := manager.GetFor(ctx, job.ID)
			require.ErrorIs(t, err, ErrNotFound)
			_, err = manager.CancelFor(ctx, job.ID)
			require.ErrorIs(t, err, ErrNotFound)
			visible, err := manager.ListFor(ctx, "")
			require.NoError(t, err)
			require.Empty(t, visible)
			other, newTask, err := manager.Start(StartRequest{Type: TypePull, Backend: "backend", Model: "model", Context: ctx}, blockingOperation)
			require.NoError(t, err)
			require.True(t, newTask)
			require.NotEqual(t, job.ID, other.ID)
		})
	}
	original, _ := identity.FromContext(alice)
	changed := *original
	changed.Email = "changed@example.test"
	changed.Groups = []string{"changed-profile"}
	refreshed := identity.ContextWith(t.Context(), &changed)
	visible, err := manager.GetFor(refreshed, job.ID)
	require.NoError(t, err)
	require.Equal(t, job.ID, visible.ID)
	duplicate, newTask, err := manager.Start(StartRequest{Type: TypePull, Backend: "backend", Model: "model", Context: refreshed}, blockingOperation)
	require.NoError(t, err)
	require.False(t, newTask)
	require.Equal(t, job.ID, duplicate.ID)
	_, err = manager.CancelFor(refreshed, job.ID)
	require.NoError(t, err)
	completed, err := manager.Await(t.Context(), job.ID)
	require.NoError(t, err)
	require.Equal(t, PhaseCancelled, completed.Phase)
}

func TestTaskDeduplicationIncludesEffectiveAuthorityAndParameters(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*StartRequest)
	}{
		{"node", func(r *StartRequest) { r.Node = "other-node" }},
		{"preset", func(r *StartRequest) { r.Preset = "other-preset" }},
		{"wiring", func(r *StartRequest) { r.Wire = true }},
		{"resource", func(r *StartRequest) { r.Resource = "other-resource" }},
		{"backend instance", func(r *StartRequest) { r.Scope = "other-target" }},
		{"parameters", func(r *StartRequest) { r.Parameters = `{"placement":"split"}` }},
		{"new credential", func(r *StartRequest) { r.Context = identity.ContextWithToken(r.Context, "fresh-credential") }},
		{"different grant", func(r *StartRequest) {
			caller, _ := identity.FromContext(r.Context)
			changed := *caller
			changed.Scopes = []string{"other-grant"}
			r.Context = identity.ContextWith(r.Context, &changed)
		}},
		{"resource UID", func(r *StartRequest) {
			r.Object = &backend.JobObject{Namespace: "tenant", Name: "job", UID: "replacement"}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager := managedJobs(t)
			request := StartRequest{Type: TypePull, Backend: "backend", Model: "model", Scope: "target", Context: authenticatedCaller(t, "alice")}
			first, _, err := manager.Start(request, blockingOperation)
			require.NoError(t, err)
			test.change(&request)
			second, created, err := manager.Start(request, blockingOperation)
			require.NoError(t, err)
			require.True(t, created)
			require.NotEqual(t, first.ID, second.ID)
		})
	}
}

func TestTaskAccessRechecksCurrentAuthorizationOnEveryPath(t *testing.T) {
	manager := managedJobs(t)
	var allowed atomic.Bool
	allowed.Store(true)
	var checks atomic.Int32
	request := StartRequest{Type: TypePull, Model: "model", Context: authenticatedCaller(t, "alice"), Authorize: func(context.Context, Job) error {
		checks.Add(1)
		if !allowed.Load() {
			return backend.ErrForbidden
		}
		return nil
	}}
	job, _, err := manager.Start(request, blockingOperation)
	require.NoError(t, err)
	_, created, err := manager.Start(request, blockingOperation)
	require.NoError(t, err)
	require.False(t, created)
	require.EqualValues(t, 3, checks.Load())
	before := checks.Load()
	_, err = manager.GetFor(authenticatedCaller(t, "bob"), job.ID)
	require.ErrorIs(t, err, ErrNotFound)
	require.Equal(t, before, checks.Load())
	allowed.Store(false)
	_, err = manager.GetFor(request.Context, job.ID)
	require.ErrorIs(t, err, backend.ErrForbidden)
	_, err = manager.CancelFor(request.Context, job.ID)
	require.ErrorIs(t, err, backend.ErrForbidden)
	visible, err := manager.ListFor(request.Context, "")
	require.NoError(t, err)
	require.Empty(t, visible)
	_, _, err = manager.Start(request, blockingOperation)
	require.ErrorIs(t, err, backend.ErrForbidden)
	current, err := manager.Get(job.ID)
	require.NoError(t, err)
	require.False(t, current.Done())
}

func TestTaskIdentityFailsClosedAndApplicationRecoveryIsNotUserOwnership(t *testing.T) {
	manager := managedJobs(t)
	for _, caller := range []*identity.Identity{
		{}, {Issuer: "https://issuer.example.test", Subject: "alice"},
		{Issuer: "https://issuer.example.test", ClientID: "client"},
		{Subject: "alice", ClientID: "client"},
		{Issuer: "https://issuer.example.test", Subject: "alice", ClientID: "client", ActorSubject: "agent"},
	} {
		_, _, err := manager.Start(StartRequest{Context: identity.ContextWith(t.Context(), caller)}, blockingOperation)
		require.ErrorIs(t, err, ErrIdentityRequired)
	}
	_, _, err := manager.Start(StartRequest{Context: t.Context()}, blockingOperation)
	require.ErrorIs(t, err, ErrIdentityRequired)
	_, _, err = manager.Start(StartRequest{Context: identity.ContextWithToken(t.Context(), "credential")}, blockingOperation)
	require.ErrorIs(t, err, ErrIdentityRequired)
	recovered, _, err := manager.Start(StartRequest{ApplicationOwned: true, Type: TypePull, Model: "model"}, blockingOperation)
	require.NoError(t, err)
	_, err = manager.GetFor(authenticatedCaller(t, "alice"), recovered.ID)
	require.ErrorIs(t, err, ErrNotFound)
	_, err = manager.GetFor(t.Context(), recovered.ID)
	require.ErrorIs(t, err, ErrIdentityRequired)
	local := NewManager(WithAnonymousAccess(true))
	job, _, err := local.Start(StartRequest{}, func(context.Context, func(backend.Progress)) (any, error) { return nil, nil })
	require.NoError(t, err)
	_, err = local.Await(t.Context(), job.ID)
	require.NoError(t, err)
	_, err = local.GetFor(t.Context(), job.ID)
	require.NoError(t, err)
}

func TestTaskSnapshotsCannotChangeAuthorizationResource(t *testing.T) {
	manager := managedJobs(t)
	object := &backend.JobObject{Namespace: "tenant", Name: "job", UID: "original"}
	request := StartRequest{Type: TypePull, Model: "model", Object: object, Context: authenticatedCaller(t, "alice")}
	job, _, err := manager.Start(request, blockingOperation)
	require.NoError(t, err)
	object.UID = "modified-input"
	job.Object.Namespace = "other-namespace"
	visible, err := manager.GetFor(request.Context, job.ID)
	require.NoError(t, err)
	require.Equal(t, "tenant", visible.Object.Namespace)
	require.Equal(t, "original", visible.Object.UID)
	visible.Object.UID = "modified-output"
	again, err := manager.GetFor(request.Context, job.ID)
	require.NoError(t, err)
	require.Equal(t, "original", again.Object.UID)
}

func TestDeduplicatedTaskChecksItsDiscoveredResource(t *testing.T) {
	manager := managedJobs(t)
	attached := make(chan struct{})
	request := StartRequest{Type: TypePull, Model: "model", Context: authenticatedCaller(t, "alice"), Authorize: func(_ context.Context, job Job) error {
		if job.Object != nil {
			return backend.ErrForbidden
		}
		return nil
	}}
	job, _, err := manager.Start(request, func(ctx context.Context, report func(backend.Progress)) (any, error) {
		report(backend.Progress{Object: &backend.JobObject{Namespace: "tenant", Name: "download", UID: "original"}})
		close(attached)
		return blockingOperation(ctx, report)
	})
	require.NoError(t, err)
	select {
	case <-attached:
	case <-time.After(time.Second):
		t.Fatal("worker did not attach its resource")
	}
	_, _, err = manager.Start(request, blockingOperation)
	require.ErrorIs(t, err, backend.ErrForbidden)
	current, err := manager.Get(job.ID)
	require.NoError(t, err)
	require.False(t, current.Done())
}

func TestTaskListChecksOnlyTheSelectedBackend(t *testing.T) {
	manager := managedJobs(t)
	caller := authenticatedCaller(t, "alice")
	var unavailable atomic.Bool
	for _, name := range []backend.Name{"selected", "other"} {
		_, _, err := manager.Start(StartRequest{Type: TypePull, Backend: name, Model: "model", Context: caller, Authorize: func(context.Context, Job) error {
			if name == "other" && unavailable.Load() {
				return backend.ErrUnavailable
			}
			return nil
		}}, blockingOperation)
		require.NoError(t, err)
	}
	unavailable.Store(true)
	visible, err := manager.ListFor(caller, "selected")
	require.NoError(t, err)
	require.Len(t, visible, 1)
	require.Equal(t, backend.Name("selected"), visible[0].Backend)
	_, err = manager.ListFor(caller, "")
	require.ErrorIs(t, err, backend.ErrUnavailable)
}
