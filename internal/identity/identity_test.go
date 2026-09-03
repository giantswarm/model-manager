package identity

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestContextRoundTrip(t *testing.T) {
	ctx := context.Background()
	_, ok := FromContext(ctx)
	assert.False(t, ok)
	assert.Equal(t, "service-account", LogAttr(ctx).Value.String())
	assert.Equal(t, "", Caller(ctx))

	id := &Identity{Subject: "sub-1", Email: "admin@lab.local", Groups: []string{"platform-admins"}, Source: SourceSSO}
	ctx = ContextWith(ctx, id)
	got, ok := FromContext(ctx)
	require.True(t, ok)
	assert.Equal(t, id, got)
	assert.Equal(t, "admin@lab.local", Caller(ctx))
	assert.Equal(t, "admin@lab.local", LogAttr(ctx).Value.String())

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

func jwt(t *testing.T, claims map[string]any) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload, err := json.Marshal(claims)
	require.NoError(t, err)
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}
