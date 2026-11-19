package pr_test

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// writePackage is the only package that may hold the write capability.
const writePackage = "github.com/a-hagi613/pagerless/internal/github/pr"

// readPackages are the read paths: the target repository client, the
// provenance ladder, Proposal Validation, the Proposal model, the report the
// agent submits, and the server's public interfaces. The import graph must not
// let any of them reach the writer.
var readPackages = []string{
	"github.com/a-hagi613/pagerless/internal/github",
	"github.com/a-hagi613/pagerless/internal/provenance",
	"github.com/a-hagi613/pagerless/internal/validation",
	"github.com/a-hagi613/pagerless/internal/proposal",
	"github.com/a-hagi613/pagerless/internal/agentapi",
	"github.com/a-hagi613/pagerless/internal/page",
}

func TestReadPathsCannotReachTheWriter(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	arguments := append([]string{"list", "-deps"}, readPackages...)
	command := exec.CommandContext(ctx, "go", arguments...)
	output, err := command.Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}
	dependencies := strings.Split(strings.TrimSpace(string(output)), "\n")
	seen := make(map[string]bool, len(dependencies))
	for _, dependency := range dependencies {
		seen[dependency] = true
	}
	for _, readPackage := range readPackages {
		if !seen[readPackage] {
			t.Fatalf("%s is not in the dependency list, so the test proves nothing", readPackage)
		}
	}
	if seen[writePackage] {
		t.Fatalf("a read path reaches %s: the write capability is not isolated", writePackage)
	}
}
