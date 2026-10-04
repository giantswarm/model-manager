package gitops

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/giantswarm/gitops-commit/commit"
)

// ErrRepositoryUnavailable is a commit whose repository the person's App user
// token does not reach: the App is not installed on it (or the repository
// does not exist, or the person cannot see it). The message names the
// repository and the App to install there.
var ErrRepositoryUnavailable = errors.New("repository unavailable")

// githubAPI is GitHub's REST API.
const githubAPI = "https://api.github.com"

// opGetRepository names the repository check in an AuthError.
const opGetRepository = "get repository"

// Remote is what a commit needs of GitHub, acting as the person: the pull
// request seams, the reads of the base, and whether the person's token
// reaches a repository at all.
type Remote interface {
	commit.Remote
	commit.Reader
	// Reachable reports whether the token reaches repo: false for GitHub's
	// 404, its answer alike for a repository the App is not installed on, one
	// that does not exist and one the person cannot see.
	Reachable(ctx context.Context, repo commit.Repository) (bool, error)
}

// GitHub is gitops-commit's GitHub remote plus the repository check, on the
// person's App user token.
type GitHub struct {
	*commit.GitHub
	token  string
	apiURL string
	client *http.Client
}

// NewGitHub builds the remote of the person's App user token against the
// REST API at apiURL (https://api.github.com, or a GitHub Enterprise
// Server's …/api/v3).
func NewGitHub(token, apiURL string) (*GitHub, error) {
	var opts []commit.GitHubOption
	if apiURL == "" {
		apiURL = githubAPI
	}
	if apiURL != githubAPI {
		opts = append(opts, commit.WithBaseURL(apiURL))
	}
	gh, err := commit.NewGitHub(token, opts...)
	if err != nil {
		return nil, err
	}
	return &GitHub{GitHub: gh, token: token, apiURL: strings.TrimSuffix(apiURL, "/"), client: http.DefaultClient}, nil
}

// Reachable implements Remote with GET /repos/{owner}/{repo}.
func (g *GitHub) Reachable(ctx context.Context, repo commit.Repository) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.apiURL+"/repos/"+url.PathEscape(repo.Owner)+"/"+url.PathEscape(repo.Name), nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+g.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := g.client.Do(req)
	if err != nil {
		return false, fmt.Errorf("%s %s: %w", opGetRepository, repo, err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return false, &commit.AuthError{Op: opGetRepository, Status: resp.StatusCode}
	default:
		return false, fmt.Errorf("%s %s: %s", opGetRepository, repo, resp.Status)
	}
}

// FakeRemote is gitops-commit's Fake with the repositories the token does
// not reach.
type FakeRemote struct {
	*commit.Fake
	Unreachable []commit.Repository
}

// Reachable implements Remote.
func (f FakeRemote) Reachable(_ context.Context, repo commit.Repository) (bool, error) {
	return !slices.Contains(f.Unreachable, repo), nil
}
