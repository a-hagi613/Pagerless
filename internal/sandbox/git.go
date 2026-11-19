package sandbox

import "context"

// Git runs the git command line with the fixed configuration that this
// package uses for every invocation: no system configuration, no prompts, and
// only the protocols a Sandbox can use. It runs in dir, or outside any
// repository when dir is empty. The environment adds to the sanitized process environment,
// stdin is the standard input when it is not nil, and the result is the
// standard output.
//
// The Sandbox helpers use it to write into the bare repository that stands in
// for the Target Repository; nothing in the server writes to a Target.
func Git(ctx context.Context, dir string, env []string, stdin []byte, args ...string) (string, error) {
	return run(ctx, dir, env, stdin, args...)
}

// Check reports whether the repository's URL and branch are usable.
func (r Repository) Check() error { return r.check() }
