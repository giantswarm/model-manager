package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPublicEndpointRemovesCredentials(t *testing.T) {
	for _, test := range []struct{ endpoint, want string }{
		{"https://user:password@example.test:8443/v1?api_key=secret#token", "https://example.test:8443/v1"},
		{"http://ollama:11434", "http://ollama:11434"},
		{"kserve://serving", "kserve://serving"},
		{"http://%", ""}, {"opaque:secret", ""},
	} {
		require.Equal(t, test.want, publicEndpoint(test.endpoint))
	}
}
