package gitops

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/giantswarm/gitops-commit/commit"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGitHubReachable maps GET /repos/{owner}/{repo} to whether the
// person's token reaches the repository.
func TestGitHubReachable(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		want   bool
		auth   bool
	}{
		"installed":     {status: http.StatusOK, want: true},
		"not installed": {status: http.StatusNotFound},
		"bad token":     {status: http.StatusUnauthorized, auth: true},
		"forbidden":     {status: http.StatusForbidden, auth: true},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/api/v3/repos/giantswarm/lab-fleet", r.URL.Path)
				assert.Equal(t, "Bearer ghu_x", r.Header.Get("Authorization"))
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()
			gh, err := NewGitHub("ghu_x", srv.URL+"/api/v3")
			require.NoError(t, err)
			ok, err := gh.Reachable(context.Background(), fleet)
			if tc.auth {
				var auth *commit.AuthError
				require.ErrorAs(t, err, &auth)
				assert.Equal(t, tc.status, auth.Status)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, ok)
		})
	}
}
