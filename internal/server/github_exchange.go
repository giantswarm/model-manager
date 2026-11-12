package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/giantswarm/mcp-oauth/providers/oidc"

	"github.com/giantswarm/model-manager/internal/backend"
	"github.com/giantswarm/model-manager/internal/gitops"
	"github.com/giantswarm/model-manager/internal/identity"
)

// GitHubExchangeConfig identifies a confidential RFC 8693 client and its
// operator-controlled target. No caller can select the endpoint or target.
type GitHubExchangeConfig struct {
	TokenEndpoint    string
	ClientID         string
	ClientSecretFile string
	// Audience is the broker target that releases the person's grant of
	// the App GrantApp names.
	Audience string
	CAFile   string
	APIURL   string
	// GrantApp is the provider App muster keeps the person's grant under,
	// named in the grant_required answer.
	GrantApp string
}

// Validate checks required fields.
func (c GitHubExchangeConfig) Validate() error {
	endpoint, err := url.Parse(c.TokenEndpoint)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return errors.New("GitHub token exchange requires an HTTPS token endpoint without credentials, query or fragment")
	}
	if c.ClientID == "" || c.ClientSecretFile == "" || c.Audience == "" || c.GrantApp == "" {
		return errors.New("GitHub token exchange requires a client ID, client secret file, target audience and grant App")
	}
	if c.APIURL != "" {
		apiURL, err := url.Parse(c.APIURL)
		if err != nil || apiURL.Scheme != "https" || apiURL.Host == "" || apiURL.User != nil || apiURL.RawQuery != "" || apiURL.Fragment != "" {
			return errors.New("GitHub token exchange requires an HTTPS GitHub API URL")
		}
	}
	return nil
}

// NewGitHubExchange obtains the person's user grant on demand. Tokens are
// neither cached nor attached to a session; only the returned request-local
// identity holds one.
func NewGitHubExchange(configuration GitHubExchangeConfig) (gitops.GitHubUser, error) {
	if err := configuration.Validate(); err != nil {
		return nil, err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if configuration.CAFile != "" {
		pem, err := os.ReadFile(configuration.CAFile) // #nosec G304 -- operator-provided path
		if err != nil {
			return nil, fmt.Errorf("read CA file %s: %w", configuration.CAFile, err)
		}
		roots, err := x509.SystemCertPool()
		if err != nil {
			roots = x509.NewCertPool()
		}
		if !roots.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("no CA certificate in %s", configuration.CAFile)
		}
		transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}
	}
	client := &http.Client{Transport: transport, Timeout: 15 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	return newGitHubExchange(configuration, client), nil
}

func newGitHubExchange(configuration GitHubExchangeConfig, client *http.Client) gitops.GitHubUser {
	// The endpoint is operator configuration, never a caller's, and an
	// in-cluster broker resolves to a private address: the client checks
	// only that it is HTTPS.
	exchanger := oidc.NewTokenExchangeClientWithOptions(oidc.TokenExchangeClientOptions{HTTPClient: client, Logger: slog.New(slog.DiscardHandler), AllowPrivateIP: true})
	grantRequired := func(reason string) error {
		return &gitops.ErrGrantRequired{App: configuration.GrantApp, Reason: fmt.Sprintf(reason, configuration.GrantApp, configuration.GrantApp)}
	}
	return func(ctx context.Context) (*identity.GitHub, error) {
		_, ok := identity.FromContext(ctx)
		subjectToken, hasToken := identity.TokenFromContext(ctx)
		if !ok || !hasToken || subjectToken == "" {
			return nil, fmt.Errorf("%w: commit mode needs your verified platform identity to fetch your GitHub authorization", gitops.ErrAuthRequired)
		}
		// A delegated caller (an agent acting for the person) never receives
		// the person's GitHub grant.
		if claims, err := oidc.ParseUnverifiedClaims(subjectToken); err != nil || claims["act"] != nil {
			return nil, fmt.Errorf("%w: commit mode opens the pull request as you, and only you, not an agent acting for you, can authorize it", backend.ErrUnsupported)
		}
		secret, err := os.ReadFile(configuration.ClientSecretFile)
		if err != nil || len(strings.TrimSpace(string(secret))) == 0 {
			return nil, errors.New("GitHub token exchange credentials are unavailable")
		}
		result, err := exchanger.Exchange(ctx, oidc.TokenExchangeRequest{
			TokenEndpoint:      configuration.TokenEndpoint,
			SubjectToken:       subjectToken,
			SubjectTokenType:   oidc.TokenTypeIDToken,
			RequestedTokenType: oidc.TokenTypeAccessToken,
			Audience:           configuration.Audience,
			ClientID:           configuration.ClientID,
			ClientSecret:       strings.TrimSpace(string(secret)),
		})
		if err != nil {
			if refusal, ok := errors.AsType[*oidc.TokenExchangeError](err); ok {
				if refusal.StatusCode == http.StatusBadRequest && refusal.Code == "invalid_target" {
					return nil, grantRequired("this call opens a pull request as you with your grant of the GitHub App %s in muster, which you have not given: grant it (core_auth_login app=%s), then call again")
				}
				return nil, errors.New("GitHub authorization service refused the exchange")
			}
			return nil, errors.New("GitHub authorization service is unavailable")
		}
		if !strings.EqualFold(result.TokenType, "Bearer") || result.IssuedTokenType != oidc.TokenTypeAccessToken || result.ExpiresIn <= 0 || result.RefreshToken != "" {
			return nil, errors.New("GitHub authorization service returned an invalid access token response")
		}
		apiURL := strings.TrimRight(configuration.APIURL, "/")
		if apiURL == "" {
			apiURL = DefaultGitHubAPIURL
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL+"/user", nil)
		if err != nil {
			return nil, errors.New("GitHub API URL is invalid")
		}
		request.Header.Set("Authorization", "Bearer "+result.AccessToken)
		request.Header.Set("Accept", "application/vnd.github+json")
		response, err := client.Do(request)
		if err != nil {
			return nil, errors.New("GitHub user verification is unavailable")
		}
		defer func() { _ = response.Body.Close() }()
		var user struct {
			Login string `json:"login"`
		}
		if response.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(response.Body, 16*1024)).Decode(&user) != nil || user.Login == "" {
			return nil, grantRequired("GitHub refused your grant of the GitHub App %s: grant it again in muster (core_auth_login app=%s), then call again")
		}
		return &identity.GitHub{Login: user.Login, Token: result.AccessToken}, nil
	}
}
