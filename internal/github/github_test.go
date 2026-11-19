package github_test

import (
	"testing"

	"github.com/a-hagi613/pagerless/internal/github"
)

func TestRepositoryFromURL(t *testing.T) {
	t.Parallel()
	const sandbox = "octocat/pagerless-sandbox"
	cases := []struct {
		url, want string
		wantErr   bool
	}{
		{url: "https://github.com/octocat/pagerless-sandbox", want: sandbox},
		{url: "https://github.com/octocat/pagerless-sandbox.git", want: sandbox},
		{url: "https://github.com/octocat/pagerless-sandbox/", want: sandbox},
		{url: "https://github.com/octocat", wantErr: true},
		{url: "https://github.com/octocat/sandbox/extra", wantErr: true},
		{url: "https://github.com/octo cat/sandbox", wantErr: true},
		{url: "not a url", wantErr: true},
		{url: "", wantErr: true},
	}
	for _, tc := range cases {
		got, err := github.RepositoryFromURL(tc.url)
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("RepositoryFromURL(%q) = %q, %v, want %q (error: %v)", tc.url, got, err, tc.want, tc.wantErr)
		}
	}
}
