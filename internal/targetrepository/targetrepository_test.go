package targetrepository_test

import (
	"strings"
	"testing"

	"github.com/a-hagi613/pagerless/internal/credentials"
	"github.com/a-hagi613/pagerless/internal/targetrepository"
)

const (
	testRepository = "acme/shop"
	testEnterprise = "https://ghe.example/api/v3"
)

func TestResolveDerivesGitHubDotCom(t *testing.T) {
	t.Parallel()
	got, err := targetrepository.Resolve(targetrepository.Flags{}, map[string]string{
		credentials.KeyGitHubRepo: testRepository,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.URL != "https://github.com/acme/shop.git" {
		t.Errorf("URL = %q, want https://github.com/acme/shop.git", got.URL)
	}
	if got.Repository != testRepository || got.Branch != "main" || got.APIURL != "https://api.github.com" {
		t.Errorf("repository = %+v, want acme/shop on main at the public API", got)
	}
}

func TestResolveDerivesGitHubEnterpriseFromTheAPIHost(t *testing.T) {
	t.Parallel()
	got, err := targetrepository.Resolve(targetrepository.Flags{}, map[string]string{
		credentials.KeyGitHubRepo:   "acme/shop",
		credentials.KeyGitHubAPIURL: testEnterprise,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.URL != "https://ghe.example/acme/shop.git" {
		t.Errorf("URL = %q, want the Enterprise host", got.URL)
	}
	if got.APIURL != testEnterprise {
		t.Errorf("APIURL = %q, want the configured Enterprise API", got.APIURL)
	}
}

func TestResolveRefusesAMalformedRepository(t *testing.T) {
	t.Parallel()
	for _, repository := range []string{"acme", "acme/shop/extra", "acme shop", "/shop"} {
		if _, err := targetrepository.Resolve(targetrepository.Flags{}, map[string]string{
			credentials.KeyGitHubRepo: repository,
		}); err == nil || !strings.Contains(err.Error(), "owner/name") {
			t.Errorf("GITHUB_REPO %q error = %v, want an owner/name refusal", repository, err)
		}
	}
}

func TestResolveAllowsAnEmptyRepository(t *testing.T) {
	t.Parallel()
	got, err := targetrepository.Resolve(targetrepository.Flags{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != (targetrepository.Repository{}) {
		t.Errorf("repository = %+v, want the zero value for a diagnose-only install", got)
	}
}

func TestResolveRefusesAWriteTokenWithoutARepository(t *testing.T) {
	t.Parallel()
	_, err := targetrepository.Resolve(targetrepository.Flags{}, map[string]string{
		credentials.KeyGitHubToken: "ghp-test",
	})
	if err == nil || !strings.Contains(err.Error(), "GITHUB_TOKEN is set but GITHUB_REPO is empty") {
		t.Fatalf("error = %v, want the actionable token-without-repository refusal", err)
	}
}

func TestResolveKeepsExplicitFlagsAuthoritative(t *testing.T) {
	t.Parallel()
	got, err := targetrepository.Resolve(targetrepository.Flags{
		URL:        "https://git.example/acme/shop.git",
		Branch:     "release",
		Repository: testRepository,
		APIURL:     "https://ghe.example/api/v3",
	}, map[string]string{
		credentials.KeyGitHubRepo:   "other/repo",
		credentials.KeyGitHubAPIURL: "https://api.github.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.URL != "https://git.example/acme/shop.git" || got.Branch != "release" ||
		got.Repository != testRepository || got.APIURL != testEnterprise {
		t.Errorf("repository = %+v, want the explicit flags", got)
	}
}

func TestResolveRefusesAnAPIURLWithCredentials(t *testing.T) {
	t.Parallel()
	_, err := targetrepository.Resolve(targetrepository.Flags{}, map[string]string{
		credentials.KeyGitHubRepo:   "acme/shop",
		credentials.KeyGitHubAPIURL: "https://user:secret@ghe.example/api/v3",
	})
	if err == nil || !strings.Contains(err.Error(), "must not carry credentials") {
		t.Fatalf("error = %v, want a credential refusal", err)
	}
}
