package requester

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/giantswarm/mcp-oauth/handler"
	"github.com/giantswarm/mcp-oauth/providers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCallerLogIdentityExcludesProfileData(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	caller := &providers.UserInfo{Issuer: "https://dex.example.test", ID: "subject", ActorIssuer: "https://actor.example.test", ActorSubject: "agent", Email: "private@example.test", Groups: []string{"private-group"}}
	logger.Info("operation", LogAttr(handler.ContextWithUserInfo(t.Context(), caller)))
	require.Contains(t, output.String(), `"issuer":"https://dex.example.test"`)
	require.Contains(t, output.String(), `"subject":"subject"`)
	require.Contains(t, output.String(), `"actor_subject":"agent"`)
	require.NotContains(t, output.String(), caller.Email)
	require.NotContains(t, output.String(), caller.Groups[0])
	for _, invalid := range []*providers.UserInfo{{ID: "subject"}, {Issuer: caller.Issuer}, {Issuer: caller.Issuer, ID: "subject", ActorSubject: "agent"}} {
		require.Equal(t, "unavailable", LogAttr(handler.ContextWithUserInfo(t.Context(), invalid)).Value.String())
	}
}

func TestContextRoundTrip(t *testing.T) {
	ctx := context.Background()
	_, ok := FromContext(ctx)
	assert.False(t, ok)
	assert.Equal(t, "service-account", LogAttr(ctx).Value.String())
	assert.Equal(t, "", Caller(ctx))
	assert.Nil(t, Scopes(ctx))

	info := &providers.UserInfo{Issuer: "https://idp.example.test", ID: "sub-1", Email: "admin@lab.local", Groups: []string{"platform-admins"}, Scopes: []string{"stored"}}
	ctx = handler.ContextWithUserInfo(ctx, info)
	got, ok := FromContext(ctx)
	require.True(t, ok)
	assert.Same(t, info, got)
	assert.Equal(t, "admin@lab.local", Caller(ctx))
	assert.Equal(t, "caller", LogAttr(ctx).Key)
	assert.Equal(t, "issuer", LogAttr(ctx).Value.Group()[0].Key)
	assert.Equal(t, info.Issuer, LogAttr(ctx).Value.Group()[0].Value.String())
	assert.Equal(t, []string{"stored"}, Scopes(ctx), "without validated scopes, the caller's recorded ones")
	assert.Equal(t, []string{"mcp:access"}, Scopes(handler.ContextWithScopes(ctx, []string{"mcp:access"})), "the validated scopes win")

	assert.Equal(t, "sub-1", Caller(handler.ContextWithUserInfo(t.Context(), &providers.UserInfo{ID: "sub-1"})), "email wins, subject is the fallback")

	_, ok = TokenFromContext(ctx)
	assert.False(t, ok)
	ctx = ContextWithToken(ctx, "tok")
	tok, ok := TokenFromContext(ctx)
	require.True(t, ok)
	assert.Equal(t, "tok", tok)
	assert.Same(t, ctx, ContextWithToken(ctx, ""), "an empty token adds nothing")
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
