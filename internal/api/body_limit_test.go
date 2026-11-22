package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRESTBodyLimitUsesProblemDetails(t *testing.T) {
	fixture := newFixture(t, false)
	for _, size := range []int{maxBodyBytes, maxBodyBytes + 1} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			body := `{"model":"qwen3:0.6b"}`
			body += strings.Repeat(" ", size-len(body))
			request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, fixture.srv.URL+Prefix+"/models/unload", strings.NewReader(body))
			require.NoError(t, err)
			request.ContentLength = -1
			response, err := fixture.srv.Client().Do(request)
			require.NoError(t, err)
			defer func() { require.NoError(t, response.Body.Close()) }()
			encoded, err := io.ReadAll(response.Body)
			require.NoError(t, err)
			if size == maxBodyBytes {
				require.Equal(t, http.StatusOK, response.StatusCode, string(encoded))
				return
			}
			require.Equal(t, http.StatusRequestEntityTooLarge, response.StatusCode)
			require.Equal(t, "application/problem+json", response.Header.Get("Content-Type"))
			var problem struct {
				Type   string
				Status int
				Detail string
				Error  errorDetail
			}
			require.NoError(t, json.Unmarshal(encoded, &problem))
			require.Equal(t, "about:blank", problem.Type)
			require.Equal(t, http.StatusRequestEntityTooLarge, problem.Status)
			require.Equal(t, "request_too_large", problem.Error.Code)
			require.Equal(t, problem.Detail, problem.Error.Message)
		})
	}
}
