package cmd

import (
	"log/slog"
	"os"
	"strconv"
	"strings"

	"github.com/giantswarm/mcp-oauth/oauthconfig"
)

// deprecatedOAuthOption is an option that mcp-oauth's OAUTH_* variable
// Replacement now configures. Env is the variable that also set it, if any.
type deprecatedOAuthOption struct {
	Flag        string
	Env         string
	Replacement string
}

// oauthEnvironment returns the OAUTH_* variables to set before
// oauthconfig.SettingsFromEnv reads them: each deprecated option's value under
// its replacement, then model-manager's defaults. A variable lookup finds
// wins over both. The second result lists the deprecated options in use.
func oauthEnvironment(o *serveOptions, lookup func(string) (string, bool)) (map[string]string, []deprecatedOAuthOption) {
	set := map[string]string{}
	var deprecated []deprecatedOAuthOption
	present := func(name string) bool {
		if _, ok := set[name]; ok {
			return true
		}
		for _, candidate := range []string{name, name + "_FILE"} {
			if v, ok := lookup(candidate); ok && v != "" {
				return true
			}
		}
		return false
	}
	value := func(name string) string {
		if v, ok := set[name]; ok {
			return v
		}
		v, _ := lookup(name)
		return v
	}

	publicRegistration := ""
	if o.allowPublicClientRegistration {
		publicRegistration = "true"
	}
	for _, option := range []struct {
		deprecatedOAuthOption
		value string
	}{
		{deprecatedOAuthOption{"oauth-base-url", "MODEL_MANAGER_OAUTH_BASE_URL", "OAUTH_ISSUER"}, o.oauthBaseURL},
		{deprecatedOAuthOption{"oauth-provider", "MODEL_MANAGER_OAUTH_PROVIDER", "OAUTH_PROVIDER"}, o.oauthProvider},
		{deprecatedOAuthOption{"dex-issuer-url", "DEX_ISSUER_URL", "OAUTH_DEX_ISSUER_URL"}, o.dexIssuerURL},
		{deprecatedOAuthOption{"dex-client-id", "DEX_CLIENT_ID", "OAUTH_DEX_CLIENT_ID"}, o.dexClientID},
		{deprecatedOAuthOption{"dex-client-secret", "DEX_CLIENT_SECRET", "OAUTH_DEX_CLIENT_SECRET"}, o.dexClientSecret},
		{deprecatedOAuthOption{"dex-ca-file", "DEX_CA_FILE", "OAUTH_DEX_CA_FILE"}, o.dexCAFile},
		{deprecatedOAuthOption{"google-client-id", "GOOGLE_CLIENT_ID", "OAUTH_GOOGLE_CLIENT_ID"}, o.googleClientID},
		{deprecatedOAuthOption{"google-client-secret", "GOOGLE_CLIENT_SECRET", "OAUTH_GOOGLE_CLIENT_SECRET"}, o.googleClientSecret},
		{deprecatedOAuthOption{"oauth-trusted-audiences", "", "OAUTH_TRUSTED_AUDIENCES"}, o.oauthTrustedAudiences},
		{deprecatedOAuthOption{"sso-allow-private-ip-jwks-hosts", "SSO_ALLOW_PRIVATE_IP_JWKS_HOSTS", "OAUTH_ALLOW_PRIVATE_IP_JWKS_HOSTS"}, o.ssoAllowPrivateIPJWKSHosts},
		{deprecatedOAuthOption{"allow-public-client-registration", "MODEL_MANAGER_OAUTH_ALLOW_PUBLIC_REGISTRATION", "OAUTH_ALLOW_PUBLIC_CLIENT_REGISTRATION"}, publicRegistration},
	} {
		// The flag's default is the replacement itself when Env is empty.
		if option.value == "" || option.value == value(option.Replacement) {
			continue
		}
		deprecated = append(deprecated, option.deprecatedOAuthOption)
		if !present(option.Replacement) {
			set[option.Replacement] = option.value
		}
	}

	authorizationServer := true
	if raw, ok := lookup("OAUTH_ENABLE_AUTHORIZATION_SERVER"); ok {
		if parsed, err := strconv.ParseBool(raw); err == nil {
			authorizationServer = parsed
		}
	}
	if authorizationServer && !present("OAUTH_PROVIDER") {
		set["OAUTH_PROVIDER"] = "dex"
	}
	if !present("OAUTH_ENABLE_CLIENT_ID_METADATA_DOCUMENTS") {
		set["OAUTH_ENABLE_CLIENT_ID_METADATA_DOCUMENTS"] = "true"
	}
	if issuer := value("OAUTH_ISSUER"); value("OAUTH_PROVIDER") == "google" && issuer != "" && !present("OAUTH_GOOGLE_REDIRECT_URL") {
		set["OAUTH_GOOGLE_REDIRECT_URL"] = strings.TrimRight(issuer, "/") + "/oauth/callback"
	}
	return set, deprecated
}

// loadOAuthSettings reads mcp-oauth's settings from the OAUTH_* variables,
// after setting those that deprecated options and model-manager's defaults
// supply.
func loadOAuthSettings(o *serveOptions, log *slog.Logger) (oauthconfig.Settings, error) {
	set, deprecated := oauthEnvironment(o, os.LookupEnv)
	for _, option := range deprecated {
		source := "--" + option.Flag
		if option.Env != "" {
			source += " (" + option.Env + ")"
		}
		log.Warn(source+" is deprecated and will be removed in the next minor release: set "+option.Replacement,
			"replacement", option.Replacement)
	}
	for name, v := range set {
		if err := os.Setenv(name, v); err != nil {
			return oauthconfig.Settings{}, err
		}
	}
	return oauthconfig.SettingsFromEnv()
}
