// Package targetrepository derives the one Target Repository a Page's changes
// are proposed against. The explicit server flags are authoritative; when they
// are empty, the credentials Secret is. The derivation is pure so that the
// server can validate it at startup and re-read it after onboarding without a
// second source of truth.
package targetrepository

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/a-hagi613/pagerless/internal/credentials"
	"github.com/a-hagi613/pagerless/internal/github"
)

// repositoryName matches GitHub's owner/name form.
var repositoryName = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// Flags are the server's explicit Target Repository settings. An empty value
// defers to the credentials Secret, except Branch, which defaults to main.
type Flags struct {
	// URL is the Target Repository clone URL.
	URL string
	// Branch is the branch pull requests target.
	Branch string
	// Repository is the Target Repository as owner/name.
	Repository string
	// APIURL is the GitHub API base URL.
	APIURL string
}

// Repository is the resolved Target Repository.
type Repository struct {
	// URL is the HTTPS clone URL the resolver reads.
	URL string
	// Branch is the branch pull requests target.
	Branch string
	// Repository is the GitHub owner/name.
	Repository string
	// APIURL is the GitHub API base URL.
	APIURL string
}

// DefaultBranch is the branch a Pull Request targets when none is configured.
const DefaultBranch = "main"

// Resolve returns the Target Repository that the explicit flags and the
// credentials Secret name. An installation with no repository and no write
// token resolves to a zero Repository: it can diagnose Pages but has no
// repository to ground a change in. A write token without a repository is a
// misconfiguration, because the operator has supplied the credential and not
// the destination.
func Resolve(flags Flags, values map[string]string) (Repository, error) {
	apiURL := strings.TrimSpace(flags.APIURL)
	if apiURL == "" {
		apiURL = strings.TrimSpace(values[credentials.KeyGitHubAPIURL])
	}
	if apiURL == "" {
		apiURL = github.DefaultGitHubAPI
	}
	if err := validateAPIURL(apiURL); err != nil {
		return Repository{}, err
	}

	repository := strings.TrimSpace(flags.Repository)
	cloneURL := strings.TrimSpace(flags.URL)
	if cloneURL == "" {
		if repository == "" {
			repository = strings.TrimSpace(values[credentials.KeyGitHubRepo])
		}
	} else {
		derived, err := github.RepositoryFromURL(cloneURL)
		switch {
		case err != nil && repository == "":
			return Repository{}, fmt.Errorf("the Target Repository URL is not usable: %w", err)
		case err != nil:
			// An explicit owner/name and a local test repository URL are both
			// authoritative. The API writer uses the owner/name; git Actions use
			// the URL.
		case repository == "":
			repository = derived
		case !strings.EqualFold(repository, derived):
			return Repository{}, fmt.Errorf(
				"the Target Repository URL names %s but --github-repository names %s", derived, repository)
		}
	}
	if repository == "" {
		if strings.TrimSpace(values[credentials.KeyGitHubToken]) != "" {
			return Repository{}, errors.New(
				"GITHUB_TOKEN is set but GITHUB_REPO is empty: set GITHUB_REPO to owner/name so Pagerless can open a pull request")
		}
		return Repository{}, nil
	}
	if !repositoryName.MatchString(repository) {
		return Repository{}, fmt.Errorf("GITHUB_REPO must be owner/name, got %q", repository)
	}
	if cloneURL == "" {
		cloneURL = "https://" + cloneHost(apiURL) + "/" + repository + ".git"
	}
	branch := strings.TrimSpace(flags.Branch)
	if branch == "" {
		branch = DefaultBranch
	}
	return Repository{
		URL:        cloneURL,
		Branch:     branch,
		Repository: repository,
		APIURL:     apiURL,
	}, nil
}

// validateAPIURL refuses an API URL that is not a plain HTTP(S) base URL.
func validateAPIURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return fmt.Errorf("GITHUB_API_URL must be an HTTP(S) URL, got %q", raw)
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("GITHUB_API_URL must not carry credentials, a query, or a fragment, got %q", raw)
	}
	return nil
}

// cloneHost maps the API host to the Git host. github.com's API host is
// api.github.com; a GitHub Enterprise API URL is served from the same host.
func cloneHost(apiURL string) string {
	parsed, err := url.Parse(apiURL)
	if err != nil || parsed.Host == "" {
		return "github.com"
	}
	if strings.EqualFold(parsed.Host, "api.github.com") {
		return "github.com"
	}
	return parsed.Host
}
