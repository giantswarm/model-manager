package cmd

import (
	"bytes"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOAuthEnvironment(t *testing.T) {
	tests := []struct {
		name           string
		options        serveOptions
		environment    map[string]string
		wantSet        map[string]string
		wantDeprecated []string
	}{
		{
			name:        "OAUTH_* only: model-manager's defaults fill the gaps",
			environment: map[string]string{"OAUTH_ISSUER": "https://mm.example.com", "OAUTH_DEX_ISSUER_URL": "https://dex.example.com"},
			wantSet:     map[string]string{"OAUTH_PROVIDER": "dex", "OAUTH_ENABLE_CLIENT_ID_METADATA_DOCUMENTS": "true"},
		},
		{
			name: "deprecated dex options map to their replacements",
			options: serveOptions{oauthBaseURL: "https://mm.example.com", oauthProvider: "dex", dexIssuerURL: "https://dex.example.com",
				dexClientID: "model-manager", dexClientSecret: "secret", dexCAFile: "/ca/ca.crt", oauthTrustedAudiences: "agent-platform,kubernetes",
				ssoAllowPrivateIPJWKSHosts: "keys.internal", allowPublicClientRegistration: true},
			wantSet: map[string]string{
				"OAUTH_ISSUER": "https://mm.example.com", "OAUTH_PROVIDER": "dex", "OAUTH_DEX_ISSUER_URL": "https://dex.example.com",
				"OAUTH_DEX_CLIENT_ID": "model-manager", "OAUTH_DEX_CLIENT_SECRET": "secret", "OAUTH_DEX_CA_FILE": "/ca/ca.crt",
				"OAUTH_TRUSTED_AUDIENCES": "agent-platform,kubernetes", "OAUTH_ALLOW_PRIVATE_IP_JWKS_HOSTS": "keys.internal",
				"OAUTH_ALLOW_PUBLIC_CLIENT_REGISTRATION": "true", "OAUTH_ENABLE_CLIENT_ID_METADATA_DOCUMENTS": "true",
			},
			wantDeprecated: []string{"oauth-base-url", "oauth-provider", "dex-issuer-url", "dex-client-id", "dex-client-secret", "dex-ca-file",
				"oauth-trusted-audiences", "sso-allow-private-ip-jwks-hosts", "allow-public-client-registration"},
		},
		{
			name:    "deprecated google options get the callback as redirect URL",
			options: serveOptions{oauthBaseURL: "https://mm.example.com/", oauthProvider: "google", googleClientID: "id", googleClientSecret: "secret"},
			wantSet: map[string]string{
				"OAUTH_ISSUER": "https://mm.example.com/", "OAUTH_PROVIDER": "google", "OAUTH_GOOGLE_CLIENT_ID": "id",
				"OAUTH_GOOGLE_CLIENT_SECRET": "secret", "OAUTH_GOOGLE_REDIRECT_URL": "https://mm.example.com/oauth/callback",
				"OAUTH_ENABLE_CLIENT_ID_METADATA_DOCUMENTS": "true",
			},
			wantDeprecated: []string{"oauth-base-url", "oauth-provider", "google-client-id", "google-client-secret"},
		},
		{
			name:    "an OAUTH_* variable wins over the deprecated option, which is still reported",
			options: serveOptions{dexIssuerURL: "https://old.example.com", dexClientSecret: "old"},
			environment: map[string]string{"OAUTH_DEX_ISSUER_URL": "https://new.example.com", "OAUTH_DEX_CLIENT_SECRET_FILE": "/secret",
				"OAUTH_PROVIDER": "dex", "OAUTH_ENABLE_CLIENT_ID_METADATA_DOCUMENTS": "false"},
			wantSet:        map[string]string{},
			wantDeprecated: []string{"dex-issuer-url", "dex-client-secret"},
		},
		{
			name:        "trusted audiences read from their own variable are not a deprecated option",
			options:     serveOptions{oauthTrustedAudiences: "agent-platform"},
			environment: map[string]string{"OAUTH_TRUSTED_AUDIENCES": "agent-platform"},
			wantSet:     map[string]string{"OAUTH_PROVIDER": "dex", "OAUTH_ENABLE_CLIENT_ID_METADATA_DOCUMENTS": "true"},
		},
		{
			name:        "no provider default with the authorization server off",
			environment: map[string]string{"OAUTH_ENABLE_AUTHORIZATION_SERVER": "false"},
			wantSet:     map[string]string{"OAUTH_ENABLE_CLIENT_ID_METADATA_DOCUMENTS": "true"},
		},
		{
			name:           "a deprecated provider is reported but not set with the authorization server off",
			options:        serveOptions{oauthProvider: "dex"},
			environment:    map[string]string{"OAUTH_ENABLE_AUTHORIZATION_SERVER": "false"},
			wantSet:        map[string]string{"OAUTH_ENABLE_CLIENT_ID_METADATA_DOCUMENTS": "true"},
			wantDeprecated: []string{"oauth-provider"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			lookup := func(name string) (string, bool) {
				v, ok := tc.environment[name]
				return v, ok
			}
			set, deprecated := oauthEnvironment(&tc.options, lookup)
			assert.Equal(t, tc.wantSet, set)
			var flags []string
			for _, option := range deprecated {
				flags = append(flags, option.Flag)
			}
			assert.Equal(t, tc.wantDeprecated, flags)
		})
	}
}

func TestLoadOAuthSettingsFromDeprecatedOptions(t *testing.T) {
	for _, name := range []string{"OAUTH_ISSUER", "OAUTH_PROVIDER", "OAUTH_DEX_ISSUER_URL", "OAUTH_DEX_CLIENT_ID", "OAUTH_DEX_CLIENT_SECRET",
		"OAUTH_TRUSTED_AUDIENCES", "OAUTH_ENABLE_CLIENT_ID_METADATA_DOCUMENTS"} {
		t.Setenv(name, "")
	}
	var output bytes.Buffer
	settings, err := loadOAuthSettings(&serveOptions{oauthBaseURL: "https://mm.example.com", dexIssuerURL: "https://dex.example.com",
		dexClientID: "model-manager", dexClientSecret: "dex-secret-value", oauthTrustedAudiences: "agent-platform"},
		slog.New(slog.NewTextHandler(&output, nil)))
	require.NoError(t, err)
	require.NotNil(t, settings.Dex)
	assert.Equal(t, "https://dex.example.com", settings.Dex.IssuerURL)
	assert.Equal(t, "https://mm.example.com/oauth/callback", settings.Dex.RedirectURL)
	assert.Equal(t, "https://mm.example.com", settings.Server.Issuer)
	assert.Equal(t, []string{"agent-platform"}, settings.Server.TrustedAudiences)
	assert.True(t, settings.Server.EnableClientIDMetadataDocuments)
	assert.Contains(t, output.String(), "--dex-issuer-url (DEX_ISSUER_URL) is deprecated and will be removed in the next minor release: set OAUTH_DEX_ISSUER_URL")
	assert.NotContains(t, output.String(), "dex-secret-value", "no secret value is logged")
}

func TestLoadOAuthSettingsRefusesGitHub(t *testing.T) {
	for name, value := range map[string]string{"OAUTH_ISSUER": "https://mm.example.com", "OAUTH_PROVIDER": "github",
		"OAUTH_GITHUB_CLIENT_ID": "id", "OAUTH_GITHUB_CLIENT_SECRET": "secret", "OAUTH_GITHUB_REDIRECT_URL": "https://mm.example.com/oauth/callback"} {
		t.Setenv(name, value)
	}
	t.Setenv("OAUTH_ENABLE_CLIENT_ID_METADATA_DOCUMENTS", "")
	_, err := loadOAuthSettings(&serveOptions{}, slog.New(slog.DiscardHandler))
	require.ErrorContains(t, err, "OAUTH_PROVIDER=github is not supported")
}
