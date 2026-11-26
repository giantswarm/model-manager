package jobs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/giantswarm/model-manager/internal/backend"
	"github.com/giantswarm/model-manager/internal/requester"
)

type principal struct{ Issuer, Subject, ActorIssuer, ActorSubject, ClientID string }
type operationKey struct {
	Owner                                                   principal
	Type                                                    Type
	Backend                                                 backend.Name
	Model, Node, Preset, Resource, Parameters, Grant, Scope string
	Wire                                                    bool
	Object                                                  backend.JobObject
}

func callerPrincipal(ctx context.Context) (principal, error) {
	caller, ok := requester.FromContext(ctx)
	if !ok {
		if _, hasToken := requester.IDTokenFromContext(ctx); hasToken {
			return principal{}, ErrIdentityRequired
		}
		return principal{}, nil
	}
	if caller.Issuer == "" || caller.ID == "" || caller.ClientID == "" || (caller.ActorIssuer == "") != (caller.ActorSubject == "") {
		return principal{}, ErrIdentityRequired
	}
	return principal{Issuer: caller.Issuer, Subject: caller.ID, ActorIssuer: caller.ActorIssuer, ActorSubject: caller.ActorSubject, ClientID: caller.ClientID}, nil
}

func grantKey(ctx context.Context) string {
	if _, ok := requester.FromContext(ctx); !ok {
		return ""
	}
	scopes := slices.Clone(requester.Scopes(ctx))
	slices.Sort(scopes)
	scopes = slices.Compact(scopes)
	credential, _ := requester.IDTokenFromContext(ctx)
	credentialDigest := sha256.Sum256([]byte(credential))
	encoded, _ := json.Marshal(struct {
		Scopes     []string
		Credential [32]byte
	}{Scopes: scopes, Credential: credentialDigest})
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

// CheckCaller requires a complete task identity or explicit anonymous mode.
func (m *Manager) CheckCaller(ctx context.Context) error {
	caller, err := callerPrincipal(ctx)
	if err != nil {
		return err
	}
	if caller == (principal{}) && !m.allowAnonymous {
		return ErrIdentityRequired
	}
	return ctx.Err()
}

// GetFor returns a caller-owned task after checking current backend access.
func (m *Manager) GetFor(ctx context.Context, id string) (Job, error) {
	caller, err := callerPrincipal(ctx)
	if err != nil {
		return Job{}, err
	}
	if caller == (principal{}) && !m.allowAnonymous {
		return Job{}, ErrIdentityRequired
	}
	m.mu.Lock()
	entry, ok := m.jobs[id]
	if !ok || entry.owner != caller {
		m.mu.Unlock()
		return Job{}, ErrNotFound
	}
	job, authorize := snapshot(entry.job), entry.authorize
	m.mu.Unlock()
	if authorize != nil {
		if err := authorize(ctx, job); err != nil {
			return Job{}, err
		}
	}
	return job, nil
}

// ListFor omits tasks the caller does not own or cannot currently access.
func (m *Manager) ListFor(ctx context.Context, backendName backend.Name) ([]Job, error) {
	caller, err := callerPrincipal(ctx)
	if err != nil {
		return nil, err
	}
	if caller == (principal{}) && !m.allowAnonymous {
		return nil, ErrIdentityRequired
	}
	all := m.List()
	out := make([]Job, 0, len(all))
	for _, job := range all {
		if backendName != "" && job.Backend != backendName {
			continue
		}
		visible, err := m.GetFor(ctx, job.ID)
		if err == nil {
			out = append(out, visible)
		} else if !hiddenFromList(err) {
			return nil, err
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return out, nil
}

// hiddenFromList reports access outcomes that remove one task from a list
// without failing it: another owner, revoked access, or a replaced resource.
func hiddenFromList(err error) bool {
	return errors.Is(err, ErrNotFound) || errors.Is(err, backend.ErrNotFound) || errors.Is(err, backend.ErrForbidden) || apierrors.IsForbidden(err)
}

// CancelFor cancels only the caller's task after checking current backend access.
func (m *Manager) CancelFor(ctx context.Context, id string) (Job, error) {
	if _, err := m.GetFor(ctx, id); err != nil {
		return Job{}, err
	}
	return m.Cancel(id)
}

func cloneObject(object *backend.JobObject) *backend.JobObject {
	if object == nil {
		return nil
	}
	copy := *object
	return &copy
}

func snapshot(job Job) Job { job.Object = cloneObject(job.Object); return job }
