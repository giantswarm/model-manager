package ollama

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/model-manager/internal/backend"
)

func TestMappedNotFoundOmitsUpstreamBody(t *testing.T) {
	err := mapErr(&APIError{Status: http.StatusNotFound, Message: "private upstream credential"}, "model")
	require.ErrorIs(t, err, backend.ErrNotFound)
	require.EqualError(t, err, "model not found: model")
}
