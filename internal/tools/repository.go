package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// repositoryArguments are the arguments the repository tools share. A model may
// name a repository only to repeat the Page's; naming another is refused.
type repositoryArguments struct {
	Repository string `json:"repository"`
	Path       string `json:"path"`
	Limit      int    `json:"limit"`
}

// repository runs one repository read. Every read uses the Page's one Target
// Repository and ref, whatever the arguments say.
func (b *Broker) repository(
	ctx context.Context, scope Scope, tool string, raw json.RawMessage,
) (any, error) {
	var args repositoryArguments
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidArguments, err)
	}
	if len(scope.Repositories) == 0 {
		return nil, fmt.Errorf("the Page has no Target Repository, so no repository read is possible")
	}
	repository := scope.Repositories[0]
	if args.Repository != "" && !strings.EqualFold(args.Repository, repository.Owner+"/"+repository.Name) {
		return nil, fmt.Errorf("%w: repository %s is not the Page's Target Repository", ErrOutOfScope, args.Repository)
	}
	if b.GitHub == nil {
		return nil, fmt.Errorf("repository reads are not configured")
	}
	switch tool {
	case ToolRepoTree:
		return b.GitHub.Tree(ctx, repository, repository.Ref, args.Path)
	case ToolRepoFile:
		if args.Path == "" {
			return nil, fmt.Errorf("%w: path is required", ErrInvalidArguments)
		}
		if !validRepositoryPath(args.Path) {
			return nil, fmt.Errorf("%w: path %q is not a repository-relative path", ErrOutOfScope, args.Path)
		}
		return b.GitHub.File(ctx, repository, repository.Ref, args.Path)
	case ToolRepoCommits:
		if args.Path != "" && !validRepositoryPath(args.Path) {
			return nil, fmt.Errorf("%w: path %q is not a repository-relative path", ErrOutOfScope, args.Path)
		}
		return b.GitHub.Commits(ctx, repository, repository.Ref, args.Path, args.Limit)
	default:
		return nil, fmt.Errorf("%w: the tool %s is not a repository read", ErrOutOfScope, tool)
	}
}
