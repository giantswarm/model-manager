package idtoken

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestContextRoundTrip(t *testing.T) {
	ctx := t.Context()
	_, ok := FromContext(ctx)
	require.False(t, ok)
	require.Equal(t, ctx, ContextWith(ctx, ""), "an empty token adds nothing")

	token, ok := FromContext(ContextWith(ctx, "id-token"))
	require.True(t, ok)
	require.Equal(t, "id-token", token)
}
