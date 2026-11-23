package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/giantswarm/model-manager/internal/backend"
)

func TestPublicFailuresRetainStatusWithoutUpstreamDetails(t *testing.T) {
	const sensitive = "https://user:password@private.example.test/?token=secret" // #nosec G101 -- Credential-bearing fixture exercises public redaction.
	for _, test := range []struct {
		name          string
		err           error
		status        int
		code, message string
	}{
		{"unknown", errors.New(sensitive), http.StatusBadGateway, "backend_error", "backend request failed"},
		{"expired caller", apierrors.NewUnauthorized(sensitive), http.StatusUnauthorized, "auth_required", "caller credential was rejected; retry with a fresh credential"},
		{"forbidden", apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, "private", errors.New(sensitive)), http.StatusForbidden, "forbidden", "operation forbidden for the caller"},
		{"unavailable", fmt.Errorf("%w: %s", backend.ErrUnavailable, sensitive), http.StatusServiceUnavailable, "unavailable", "backend unavailable"},
		{"validation", fmt.Errorf("%w: model is required", backend.ErrInvalid), http.StatusBadRequest, "invalid_request", "invalid request: model is required"},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			(&REST{log: slog.Default()}).writeError(recorder, test.err)
			require.Equal(t, test.status, recorder.Code)
			require.Contains(t, recorder.Body.String(), test.code)
			require.Contains(t, recorder.Body.String(), test.message)
			require.NotContains(t, recorder.Body.String(), sensitive)
			result := errResult(test.err)
			require.True(t, result.IsError)
			require.Contains(t, result.Content[0].(mcp.TextContent).Text, test.message)
			require.NotContains(t, result.Content[0].(mcp.TextContent).Text, sensitive)
		})
	}
}

func TestBackgroundPublicErrorMessages(t *testing.T) {
	for _, test := range []struct {
		err     error
		message string
	}{
		{nil, ""}, {errors.New("private upstream response"), "backend request failed"},
		{context.Canceled, "cancelled"}, {context.DeadlineExceeded, "operation timed out"},
		{fmt.Errorf("%w: private", backend.ErrConflict), "conflict"},
	} {
		require.Equal(t, test.message, backend.PublicErrorMessage(test.err))
	}
}

func TestMissingRequiredArgumentIsAnInvalidRequest(t *testing.T) {
	_, err := requiredString(mcp.CallToolRequest{}, argJobID)
	require.ErrorIs(t, err, backend.ErrInvalid)
	text := errResult(err).Content[0].(mcp.TextContent).Text
	require.Contains(t, text, "invalid_request: ")
	require.Contains(t, text, `required argument "id"`)
}
