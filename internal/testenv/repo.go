package testenv

import (
	"strings"
)

// Rev returns the commit that the ref names in the bare repository, or an
// empty string when the ref does not exist.
func (r *BareRepo) Rev(ref string) string {
	r.t.Helper()
	out, err := gitOutput(r.t.Context(), r.URL, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// Branches returns the names of the branches in the bare repository.
func (r *BareRepo) Branches() []string {
	r.t.Helper()
	out := git(r.t, r.URL, "for-each-ref", "--format=%(refname:short)", "refs/heads")
	return strings.Fields(out)
}

// File returns the content of the file at the ref.
func (r *BareRepo) File(ref, path string) string {
	r.t.Helper()
	return git(r.t, r.URL, "show", ref+":"+path)
}

// Diff returns the patch from one ref to another, in the form that the
// server's Validation renders: full object identifiers and fixed prefixes.
func (r *BareRepo) Diff(from, to string) string {
	r.t.Helper()
	return git(r.t, r.URL, "diff", "--no-color", "--no-ext-diff", "--no-textconv", "--no-renames",
		"--full-index", "--src-prefix=a/", "--dst-prefix=b/", from, to)
}

// Message returns the full commit message at the ref.
func (r *BareRepo) Message(ref string) string {
	r.t.Helper()
	return git(r.t, r.URL, "log", "-1", "--format=%B", ref)
}

// AuthorTime returns the author date of the commit at the ref, as Unix seconds text.
func (r *BareRepo) AuthorTime(ref string) string {
	r.t.Helper()
	return strings.TrimSpace(git(r.t, r.URL, "log", "-1", "--format=%at", ref))
}

// MergeBranch squash-merges the branch into main, as the Operator does when
// they merge a Pull Request, and returns the merge commit.
func (r *BareRepo) MergeBranch(branch, message string) string {
	r.t.Helper()
	git(r.t, r.work, "fetch", "origin")
	git(r.t, r.work, "reset", "--hard", "origin/main")
	git(r.t, r.work, "merge", "--squash", "origin/"+branch)
	git(r.t, r.work, "commit", "--message", message)
	git(r.t, r.work, "push", "origin", "HEAD:main")
	return r.Head()
}
