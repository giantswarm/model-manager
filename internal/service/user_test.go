package service

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/giantswarm/mcp-oauth/handler"
	"github.com/giantswarm/mcp-oauth/providers"
	"github.com/stretchr/testify/require"
)

func TestMutationLogNamesTheUserWithoutProfileData(t *testing.T) {
	user := &providers.UserInfo{Issuer: "https://dex.example.test", ID: "subject", ActorIssuer: "https://sts.example.test", ActorSubject: "agent", Email: "private@example.test", Name: "Private Name", Groups: []string{"private-group"}}
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))

	logger.Info("model deleted", userAttr(handler.ContextWithUserInfo(t.Context(), user)))
	var record map[string]any
	require.NoError(t, json.Unmarshal(output.Bytes(), &record))
	require.Equal(t, map[string]any{
		"issuer":  "https://dex.example.test",
		"subject": "subject",
		"actor":   map[string]any{"issuer": "https://sts.example.test", "subject": "agent"},
	}, record["user"])
	for _, private := range []string{user.Email, user.Name, user.Groups[0]} {
		require.NotContains(t, output.String(), private)
	}

	output.Reset()
	logger.Info("model unwired with its backend", userAttr(t.Context()))
	record = nil
	require.NoError(t, json.Unmarshal(output.Bytes(), &record))
	require.NotContains(t, record, "user", "application-owned work names no user")
}
