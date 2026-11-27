package jobs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/giantswarm/mcp-oauth/handler"

	"github.com/giantswarm/model-manager/internal/backend"
	"github.com/giantswarm/model-manager/internal/idtoken"
)

// taskOwner is who a task belongs to: the user by issuer and subject (OpenID
// Connect Core 5.7), the actor of a delegated call (RFC 8693 4.1) and the
// OAuth client.
type taskOwner struct{ Issuer, Subject, ActorIssuer, ActorSubject, ClientID string }
type operationKey struct {
	Owner                                                           taskOwner
	Type                                                            Type
	Backend                                                         backend.Name
	Model, Node, Preset, Resource, Parameters, Authorization, Scope string
	Wire                                                            bool
	Object                                                          backend.JobObject
}

// ownerFrom is the owner of a task the request starts or reads. mcp-oauth
// guarantees the issuer and subject of a validated user. A delegated actor
// has a subject and an optional issuer; a task also needs the client.
// A request without a user owns nothing, unless it carries an ID token,
// which only a validated user can.
func ownerFrom(ctx context.Context) (taskOwner, error) {
	user, ok := handler.UserInfoFromContext(ctx)
	if !ok || user == nil {
		if _, hasToken := idtoken.FromContext(ctx); hasToken {
			return taskOwner{}, ErrIdentityRequired
		}
		return taskOwner{}, nil
	}
	if user.ClientID == "" {
		return taskOwner{}, ErrIdentityRequired
	}
	return taskOwner{Issuer: user.Issuer, Subject: user.ID, ActorIssuer: user.ActorIssuer, ActorSubject: user.ActorSubject, ClientID: user.ClientID}, nil
}

// authorizationKey digests what the request is authorized with: its scopes
// (RFC 6749 3.3) and its ID token. Active tasks are shared only between
// requests with the same authorization.
func authorizationKey(ctx context.Context) string {
	if user, ok := handler.UserInfoFromContext(ctx); !ok || user == nil {
		return ""
	}
	scopes := slices.Clone(grantedScopes(ctx))
	slices.Sort(scopes)
	scopes = slices.Compact(scopes)
	credential, _ := idtoken.FromContext(ctx)
	credentialDigest := sha256.Sum256([]byte(credential))
	encoded, _ := json.Marshal(struct {
		Scopes     []string
		Credential [32]byte
	}{Scopes: scopes, Credential: credentialDigest})
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

// grantedScopes are the scopes mcp-oauth validated for this request, else
// those recorded for the user.
func grantedScopes(ctx context.Context) []string {
	if scopes, ok := handler.ScopesFromContext(ctx); ok && len(scopes) > 0 {
		return scopes
	}
	if user, ok := handler.UserInfoFromContext(ctx); ok && user != nil {
		return user.Scopes
	}
	return nil
}

// requestedBy is the attribution recorded on a job: the user's email, else
// the subject, or "" without a user.
func requestedBy(ctx context.Context) string {
	user, ok := handler.UserInfoFromContext(ctx)
	switch {
	case !ok || user == nil:
		return ""
	case user.Email != "":
		return user.Email
	default:
		return user.ID
	}
}

// CheckUser requires a user who can own a task, or explicit anonymous mode.
func (m *Manager) CheckUser(ctx context.Context) error {
	owner, err := ownerFrom(ctx)
	if err != nil {
		return err
	}
	if owner == (taskOwner{}) && !m.allowAnonymous {
		return ErrIdentityRequired
	}
	return ctx.Err()
}

// GetFor returns a task the user owns after checking current backend access.
func (m *Manager) GetFor(ctx context.Context, id string) (Job, error) {
	owner, err := ownerFrom(ctx)
	if err != nil {
		return Job{}, err
	}
	if owner == (taskOwner{}) && !m.allowAnonymous {
		return Job{}, ErrIdentityRequired
	}
	m.mu.Lock()
	entry, ok := m.jobs[id]
	if !ok || entry.owner != owner {
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

// ListFor omits tasks the user does not own or cannot currently access.
func (m *Manager) ListFor(ctx context.Context, backendName backend.Name) ([]Job, error) {
	owner, err := ownerFrom(ctx)
	if err != nil {
		return nil, err
	}
	if owner == (taskOwner{}) && !m.allowAnonymous {
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
