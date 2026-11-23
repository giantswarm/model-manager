package backend

import (
	"context"
	"errors"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// PublicErrorMessage classifies a failure without disclosing upstream response
// bodies, addresses or credentials. Callers retain their structured operation data.
func PublicErrorMessage(err error) string {
	if err == nil {
		return ""
	}
	switch {
	case apierrors.IsUnauthorized(err):
		return "caller credential was rejected; retry with a fresh credential"
	case apierrors.IsForbidden(err):
		return "operation forbidden for the caller"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "operation timed out"
	case errors.Is(err, ErrUnsupported):
		return err.Error()
	}
	for _, known := range []error{ErrNotFound, ErrForbidden, ErrConflict, ErrInvalid, ErrUnfit, ErrUnavailable, ErrGitOpsOwned} {
		if errors.Is(err, known) {
			return known.Error()
		}
	}
	return "backend request failed"
}
