package server

import (
	"context"
	"crypto/tls"
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
	TokenEndpoint string
	// ClientID wins over ClientIDFile, which is read at startup.
	ClientID         string
	ClientIDFile     string
	ClientSecretFile string
	// Audience is the RFC 8693 audience: the provider App muster keeps the
	// person's grant under, also named in the grant_required answer.
	Audience string
	CAFile   string
	APIURL   string
}

// Validate checks required fields.
func (c GitHubExchangeConfig) Validate() error {
	endpoint, err := url.Parse(c.TokenEndpoint)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return errors.New("GitHub token exchange requires an HTTPS token endpoint without credentials, query or fragment")
	}
	if (c.ClientID == "" && c.ClientIDFile == "") || c.ClientSecretFile == "" || c.Audience == "" {
		return errors.New("GitHub token exchange requires a client ID or client ID file, a client secret file and an audience")
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
// identity holds one. log receives the broker's refusals.
func NewGitHubExchange(configuration GitHubExchangeConfig, log *slog.Logger) (gitops.GitHubUser, error) {
	if err := configuration.Validate(); err != nil {
		return nil, err
	}
	if configuration.ClientID == "" {
		clientID, err := os.ReadFile(configuration.ClientIDFile)
		if err != nil {
			return nil, fmt.Errorf("read GitHub token exchange client ID file %s: %w", configuration.ClientIDFile, err)
		}
		if configuration.ClientID = strings.TrimSpace(string(clientID)); configuration.ClientID == "" {
			return nil, fmt.Errorf("GitHub token exchange client ID file %s is empty", configuration.ClientIDFile)
		}
	}
	roots, err := loadRootCAs(configuration.CAFile)
	if err != nil {
		return nil, err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if roots != nil {
		transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}
	}
	client := &http.Client{Transport: transport, Timeout: 15 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	return newGitHubExchange(configuration, client, log), nil
}

func newGitHubExchange(configuration GitHubExchangeConfig, client *http.Client, log *slog.Logger) gitops.GitHubUser {
	// The endpoint is operator configuration, never a caller's, and an
	// in-cluster broker resolves to a private address: the client checks
	// only that it is HTTPS.
	exchanger := oidc.NewTokenExchangeClientWithOptions(oidc.TokenExchangeClientOptions{HTTPClient: client, Logger: slog.New(slog.DiscardHandler), AllowPrivateIP: true})
	grantRequired := func(reason string) error {
		return &gitops.ErrGrantRequired{App: configuration.Audience, Reason: fmt.Sprintf(reason, configuration.Audience, configuration.Audience)}
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
			return nil, fmt.Errorf("%w: commit mode opens the pull request as you, and only you, not an agent acting for you, can authorize it", backend.ErrForbidden)
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
			refusal, ok := errors.AsType[*oidc.TokenExchangeError](err)
			if !ok {
				return nil, fmt.Errorf("%w: GitHub authorization service is unavailable", backend.ErrUnavailable)
			}
			log.WarnContext(ctx, "GitHub token exchange refused", "audience", configuration.Audience, "status", refusal.StatusCode, "error", refusal.Code, "errorDescription", refusal.Description)
			// The broker answers invalid_target for a person without the
			// grant, with a description that does not tell it apart from an
			// audience the client may not request.
			if refusal.StatusCode == http.StatusBadRequest && refusal.Code == "invalid_target" {
				return nil, grantRequired("this call opens a pull request as you with your grant of the GitHub App muster keeps as %s, which you have not given: grant it (core_auth_login app=%s), then call again")
			}
			if refusal.StatusCode >= http.StatusInternalServerError {
				return nil, fmt.Errorf("%w: GitHub authorization service is unavailable", backend.ErrUnavailable)
			}
			return nil, fmt.Errorf("GitHub authorization service refused model-manager's token exchange (%s): check the token exchange client and audience", refusal.Code)
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
			return nil, fmt.Errorf("%w: GitHub user verification is unavailable", backend.ErrUnavailable)
		}
		defer func() { _ = response.Body.Close() }()
		if response.StatusCode == http.StatusUnauthorized {
			return nil, grantRequired("GitHub refused your grant of the GitHub App muster keeps as %s: grant it again in muster (core_auth_login app=%s), then call again")
		}
		var user struct {
			Login string `json:"login"`
		}
		if response.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(response.Body, 16*1024)).Decode(&user) != nil || user.Login == "" {
			return nil, fmt.Errorf("%w: GitHub user verification answered status %d", backend.ErrUnavailable, response.StatusCode)
		}
		return &identity.GitHub{Login: user.Login, Token: result.AccessToken}, nil
	}
}
