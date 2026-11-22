package identity

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCallerLogIdentityExcludesProfileData(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	caller := &Identity{Issuer: "https://dex.example.test", Subject: "subject", ActorIssuer: "https://actor.example.test", ActorSubject: "agent", Email: "private@example.test", Groups: []string{"private-group"}}
	logger.Info("operation", LogAttr(ContextWith(t.Context(), caller)))
	require.Contains(t, output.String(), `"issuer":"https://dex.example.test"`)
	require.Contains(t, output.String(), `"subject":"subject"`)
	require.Contains(t, output.String(), `"actor_subject":"agent"`)
	require.NotContains(t, output.String(), caller.Email)
	require.NotContains(t, output.String(), caller.Groups[0])
	for _, invalid := range []*Identity{{Subject: "subject"}, {Issuer: caller.Issuer}, {Issuer: caller.Issuer, Subject: "subject", ActorSubject: "agent"}} {
		require.Equal(t, "unavailable", LogAttr(ContextWith(t.Context(), invalid)).Value.String())
	}
}

func TestContextRoundTrip(t *testing.T) {
	ctx := context.Background()
	_, ok := FromContext(ctx)
	assert.False(t, ok)
	assert.Equal(t, "service-account", LogAttr(ctx).Value.String())
	assert.Equal(t, "", Caller(ctx))

	id := &Identity{Issuer: "https://idp.example.test", Subject: "sub-1", Email: "admin@lab.local", Groups: []string{"platform-admins"}, Source: SourceSSO}
	ctx = ContextWith(ctx, id)
	got, ok := FromContext(ctx)
	require.True(t, ok)
	assert.Equal(t, id, got)
	assert.Equal(t, "admin@lab.local", Caller(ctx))
	assert.Equal(t, "caller", LogAttr(ctx).Key)
	assert.Equal(t, "issuer", LogAttr(ctx).Value.Group()[0].Key)
	assert.Equal(t, id.Issuer, LogAttr(ctx).Value.Group()[0].Value.String())

	assert.Equal(t, "sub-1", (&Identity{Subject: "sub-1"}).String(), "email wins, subject is the fallback")
	assert.Equal(t, "", (*Identity)(nil).String())

	_, ok = TokenFromContext(ctx)
	assert.False(t, ok)
	ctx = ContextWithToken(ctx, "tok")
	tok, ok := TokenFromContext(ctx)
	require.True(t, ok)
	assert.Equal(t, "tok", tok)
	assert.Same(t, ctx, ContextWithToken(ctx, ""), "an empty token adds nothing")
	assert.Same(t, ctx, ContextWith(ctx, nil), "a nil identity adds nothing")
}

func TestTokenExpiry(t *testing.T) {
	exp := time.Now().Add(time.Hour).Truncate(time.Second)
	assert.Equal(t, exp, TokenExpiry(jwt(t, map[string]any{"exp": exp.Unix()})))
	assert.True(t, TokenExpiry(jwt(t, map[string]any{"sub": "x"})).IsZero(), "no exp claim")
	assert.True(t, TokenExpiry("opaque-token").IsZero())
	assert.True(t, TokenExpiry("a.!!!.c").IsZero())
}

func TestTokenClaims(t *testing.T) {
	exp := time.Now().Add(time.Hour).Truncate(time.Second)
	c := TokenClaims(jwt(t, map[string]any{"iss": "https://dex.example", "aud": "muster", "exp": exp.Unix()}))
	assert.Equal(t, Claims{Issuer: "https://dex.example", Audience: []string{"muster"}, Expires: exp}, c)
	assert.Equal(t, []string{"a", "b"}, TokenClaims(jwt(t, map[string]any{"aud": []string{"a", "b"}})).Audience, "aud as an array")
	assert.Equal(t, Claims{}, TokenClaims("opaque-token"))
}

func jwt(t *testing.T, claims map[string]any) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload, err := json.Marshal(claims)
	require.NoError(t, err)
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}
