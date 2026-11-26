package kube

import (
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestTokenClaims(t *testing.T) {
	exp := time.Now().Add(time.Hour).Truncate(time.Second)
	require.Equal(t, claims{Issuer: "https://dex.example", Audience: []string{"muster"}, Expiry: exp}, tokenClaims(testJWT(t, map[string]any{"iss": "https://dex.example", "aud": "muster", "exp": exp.Unix()})))
	require.Equal(t, []string{"a", "b"}, tokenClaims(testJWT(t, map[string]any{"aud": []string{"a", "b"}})).Audience, "aud as an array")
	require.True(t, tokenClaims(testJWT(t, map[string]any{"sub": "x"})).Expiry.IsZero(), "no exp claim")
	require.Equal(t, claims{}, tokenClaims("opaque-token"))
	require.Equal(t, claims{}, tokenClaims("a.!!!.c"))
	unsigned := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`)) + "." + base64.RawURLEncoding.EncodeToString(mustJSON(t, map[string]any{"iss": "https://dex.example"})) + "."
	require.Equal(t, claims{}, tokenClaims(unsigned), "alg none is not a JWS this server accepts")
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	require.NoError(t, err)
	return encoded
}
