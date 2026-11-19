package server

import (
	"context"
	"errors"
	"fmt"
	"strings"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/credentials"
	"github.com/a-hagi613/pagerless/internal/github/pr"
	"github.com/a-hagi613/pagerless/internal/publish"
	"github.com/a-hagi613/pagerless/internal/sandbox"
	"github.com/a-hagi613/pagerless/internal/targetrepository"
	"github.com/a-hagi613/pagerless/internal/tools"
	"github.com/a-hagi613/pagerless/internal/verification"
)

// targetRepositoryResolver reads the Target Repository from the explicit flags
// and, when they are empty, from the credentials Secret. It is called at
// startup and again for each Page and publication, so a settings write after
// onboarding takes effect without a restart.
type targetRepositoryResolver func(context.Context) (targetrepository.Repository, error)

// newTargetRepositoryResolver returns a resolver over the explicit flags. The
// flags stay immutable; the credentials Secret is read on every call.
func newTargetRepositoryResolver(
	flags targetrepository.Flags, store *credentials.Store,
) targetRepositoryResolver {
	return func(ctx context.Context) (targetrepository.Repository, error) {
		values := map[string]string{}
		if store != nil {
			read, err := store.Read(ctx)
			if err == nil {
				values = read
			} else if _, missing := errors.AsType[*credentials.MissingError](err); !missing {
				return targetrepository.Repository{}, err
			}
		}
		return targetrepository.Resolve(flags, values)
	}
}

// configureTargetRepository resolves the Target Repository once at startup so
// a malformed GITHUB_REPO, or a token without a repository, fails in one line
// rather than a Page that can never be remediated. It returns the resolver the
// Page and pull request paths re-read later.
func configureTargetRepository(
	cfg *Config, store *credentials.Store,
) (targetRepositoryResolver, error) {
	resolve := newTargetRepositoryResolver(targetrepository.Flags{
		URL:        cfg.Sandbox.URL,
		Branch:     cfg.Sandbox.Branch,
		Repository: cfg.GitHubRepository,
		APIURL:     cfg.GitHubAPIURL,
	}, store)
	if cfg.DisableTargetRepository {
		return resolve, nil
	}
	startup, err := resolve(context.Background())
	if err != nil {
		return nil, fmt.Errorf("resolve the Target Repository: %w", err)
	}
	if startup.URL != "" {
		cfg.Sandbox.URL = startup.URL
		cfg.Sandbox.Branch = startup.Branch
		cfg.GitHubRepository = startup.Repository
		cfg.GitHubAPIURL = startup.APIURL
	}
	ctrl.Log.WithName("setup").Info("Target Repository", "repository", formatRepository(startup))
	return resolve, nil
}

// repositoryPublisher publishes a Proposal against the Target Repository the
// resolver returns at the time of publication. A new Publisher per call keeps
// the repository, branch, and API host current after an onboarding write.
type repositoryPublisher struct {
	resolve   targetRepositoryResolver
	token     func() (string, error)
	readToken func() (string, error)
	contents  publish.FilePatchContents
}

// Publish implements controller.Publisher.
func (p *repositoryPublisher) Publish(
	ctx context.Context, page *v1alpha1.Page,
) (publish.Result, error) {
	repository, err := p.resolve(ctx)
	if err != nil {
		return publish.Result{}, err
	}
	if repository.URL == "" || repository.Repository == "" {
		return publish.Result{}, errors.New(
			"no Target Repository is configured: set GITHUB_REPO to owner/name, " +
				"or set targetRepository.enabled=false in the chart to run diagnose-only")
	}
	return publish.New(publish.Config{
		Sandbox: sandbox.Repository{
			URL: repository.URL, Branch: repository.Branch, ReadToken: p.readToken,
		},
		GitHub: &pr.Client{
			APIURL: repository.APIURL, Repository: repository.Repository, Token: p.token,
		},
		Token:    p.token,
		Contents: p.contents,
	}).Publish(ctx, page)
}

// repositoryResolution returns the Resolution to record on a Page, or nil when
// the installation has no Target Repository.
func repositoryResolution(
	resolve targetRepositoryResolver,
) func(context.Context) (*v1alpha1.Resolution, error) {
	return func(ctx context.Context) (*v1alpha1.Resolution, error) {
		repository, err := resolve(ctx)
		if err != nil {
			return nil, err
		}
		if !strings.HasPrefix(repository.URL, "https://") {
			return nil, nil
		}
		return &v1alpha1.Resolution{
			URL:        repository.URL,
			Branch:     repository.Branch,
			Source:     v1alpha1.ResolutionSourceConfiguredMapping,
			Confidence: v1alpha1.ConfidenceConfigured,
		}, nil
	}
}

// repositoryGitHub reads the Target Repository through the API host the
// resolver currently names. The broker and the report handler accept it as a
// tools.GitHubReader, so an Enterprise host written after onboarding is used
// without restarting the server.
type repositoryGitHub struct {
	resolve  targetRepositoryResolver
	fallback string
	token    func() (string, error)
}

// client returns a read client for the current API host.
func (g repositoryGitHub) client(ctx context.Context) (*tools.HTTPGitHub, error) {
	apiURL := g.fallback
	repository, err := g.resolve(ctx)
	if err != nil {
		return nil, err
	}
	if repository.APIURL != "" {
		apiURL = repository.APIURL
	}
	return &tools.HTTPGitHub{APIURL: apiURL, Token: g.token}, nil
}

// Tree lists a bounded part of a repository tree.
func (g repositoryGitHub) Tree(
	ctx context.Context, repository tools.Repository, ref, directory string,
) (tools.Tree, error) {
	reader, err := g.client(ctx)
	if err != nil {
		return tools.Tree{}, err
	}
	return reader.Tree(ctx, repository, ref, directory)
}

// File reads one repository file.
func (g repositoryGitHub) File(
	ctx context.Context, repository tools.Repository, ref, filePath string,
) (tools.File, error) {
	reader, err := g.client(ctx)
	if err != nil {
		return tools.File{}, err
	}
	return reader.File(ctx, repository, ref, filePath)
}

// Commits lists recent commits at a ref.
func (g repositoryGitHub) Commits(
	ctx context.Context, repository tools.Repository, ref, filePath string, limit int,
) ([]tools.Commit, error) {
	reader, err := g.client(ctx)
	if err != nil {
		return nil, err
	}
	return reader.Commits(ctx, repository, ref, filePath, limit)
}

// Head returns the commit SHA a ref points at.
func (g repositoryGitHub) Head(
	ctx context.Context, repository tools.Repository, ref string,
) (string, error) {
	reader, err := g.client(ctx)
	if err != nil {
		return "", err
	}
	return reader.Head(ctx, repository, ref)
}

// secretToken reads the write token from the credentials Secret when the
// Deployment did not mount it as a file.
func secretToken(store *credentials.Store) func() (string, error) {
	return func() (string, error) {
		values, err := store.Read(context.Background())
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(values[credentials.KeyGitHubToken]), nil
	}
}

// ensureTokenSource returns the configured token source or, for a server whose
// Deployment does not mount the token file, one backed by the credentials
// Secret.
func ensureTokenSource(
	configured func() (string, error), store *credentials.Store,
) func() (string, error) {
	if configured != nil {
		return configured
	}
	return secretToken(store)
}

// configureTokenSources keeps the write and read token one source: the mounted
// file when the chart provides one, otherwise the credentials Secret.
func configureTokenSources(
	cfg *Config, store *credentials.Store,
) func() (string, error) {
	token := ensureTokenSource(cfg.GitHubToken, store)
	if cfg.GitHubReadToken == nil {
		cfg.GitHubReadToken = token
	}
	return token
}

// newVerifier builds the Verification checks and their optional GitOps and
// alert readers.
func newVerifier(cfg Config, reader client.Reader) *verification.Verifier {
	verifier := &verification.Verifier{Reader: reader, Application: cfg.ArgoCDApplication}
	if cfg.ArgoCDURL != "" {
		verifier.Applications = &verification.ArgoCD{BaseURL: cfg.ArgoCDURL, Token: cfg.ArgoCDToken}
	}
	if cfg.AlertmanagerURL != "" {
		verifier.Alerts = &verification.Alertmanager{BaseURL: cfg.AlertmanagerURL, Clock: cfg.Clock}
	}
	return verifier
}

// formatRepository names the resolved repository for a startup log line.
func formatRepository(repository targetrepository.Repository) string {
	if repository.URL == "" {
		return "none"
	}
	return fmt.Sprintf("%s on %s", repository.Repository, repository.Branch)
}
