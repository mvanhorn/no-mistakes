package intent

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/winproc"
)

// resolveHome returns the home directory to use, preferring an explicit
// override (used in tests) over the OS-reported value.
func resolveHome(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	return os.UserHomeDir()
}

// canonicalPath returns the path with symlinks evaluated and cleaned. It
// silently falls back to filepath.Clean(abs) if EvalSymlinks fails (e.g.
// the path does not exist), since both sides of the comparison receive
// the same fallback treatment.
func canonicalPath(p string) string {
	if p == "" {
		return ""
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = p
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return filepath.Clean(resolved)
	}
	return filepath.Clean(abs)
}

// pathsEqual compares two paths after canonicalization.
func pathsEqual(a, b string) bool {
	return canonicalPath(a) == canonicalPath(b)
}

// repoMatcher provides broad discovery hints only. Shared Git storage or a
// remote URL never proves that a transcript belongs to the authoring checkout.
type repoMatcher struct {
	origin string
	ids    map[string]repoIdentity
}

type repoIdentity struct {
	canonical string
	commonDir string
	remote    string
}

func newRepoMatcher(ctx context.Context, originCWD string) *repoMatcher {
	m := &repoMatcher{
		origin: canonicalPath(originCWD),
		ids:    make(map[string]repoIdentity),
	}
	if m.origin != "" {
		m.ids[m.origin] = gitRepoIdentity(ctx, originCWD)
	}
	return m
}

func (m *repoMatcher) matches(ctx context.Context, cwd string) bool {
	if m == nil || m.origin == "" {
		return true
	}
	candidate := canonicalPath(cwd)
	if candidate == "" {
		return false
	}
	if candidate == m.origin {
		return true
	}
	origin := m.ids[m.origin]
	candidateID, ok := m.ids[candidate]
	if !ok {
		candidateID = gitRepoIdentity(ctx, cwd)
		m.ids[candidate] = candidateID
	}
	if origin.commonDir != "" && candidateID.commonDir != "" && origin.commonDir == candidateID.commonDir {
		return true
	}
	return origin.remote != "" && candidateID.remote != "" && origin.remote == candidateID.remote
}

func gitRepoIdentity(ctx context.Context, dir string) repoIdentity {
	id := repoIdentity{canonical: canonicalPath(dir)}
	if id.canonical == "" {
		return id
	}
	if commonDir := gitOutput(ctx, dir, "rev-parse", "--git-common-dir"); commonDir != "" {
		if !filepath.IsAbs(commonDir) {
			commonDir = filepath.Join(dir, commonDir)
		}
		id.commonDir = canonicalPath(commonDir)
	}
	remote := gitOutput(ctx, dir, "remote", "get-url", "origin")
	if remote == "" {
		if first := firstLine(gitOutput(ctx, dir, "remote")); first != "" {
			remote = gitOutput(ctx, dir, "remote", "get-url", first)
		}
	}
	id.remote = normalizeGitRemote(remote)
	return id
}

func gitOutput(ctx context.Context, dir string, args ...string) string {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	winproc.Harden(cmd)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func normalizeGitRemote(remote string) string {
	remote = strings.TrimSpace(remote)
	if remote == "" {
		return ""
	}
	if u, err := url.Parse(remote); err == nil && u.Host != "" {
		return cleanRemoteParts(u.Host, u.Path)
	}
	if at := strings.Index(remote, "@"); at >= 0 {
		rest := remote[at+1:]
		if colon := strings.Index(rest, ":"); colon >= 0 {
			return cleanRemoteParts(rest[:colon], rest[colon+1:])
		}
	}
	if filepath.IsAbs(remote) {
		return canonicalPath(remote)
	}
	return cleanRemoteParts("", remote)
}

func cleanRemoteParts(host, path string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	path = strings.Trim(strings.TrimSpace(path), "/")
	path = strings.TrimSuffix(path, ".git")
	path = strings.ToLower(path)
	if host == "" {
		return path
	}
	if path == "" {
		return host
	}
	return host + "/" + path
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

// checkoutRoot requires a live non-bare Git checkout. Subdirectories and
// symlink aliases resolve to its canonical root; nested repositories do not.
func checkoutRoot(ctx context.Context, dir string) string {
	if dir == "" {
		return ""
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return ""
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return ""
	}
	root := gitOutput(ctx, resolved, "rev-parse", "--show-toplevel")
	if root == "" {
		return ""
	}
	resolved, err = filepath.EvalSymlinks(root)
	if err != nil {
		return ""
	}
	return canonicalPath(resolved)
}

// ResolveAuthoringCheckout binds transcript scope to the sole local checkout
// holding the submitted branch and head. Gate worktrees and same-remote clones
// cannot substitute for that proof. This function performs no Git mutations.
func ResolveAuthoringCheckout(ctx context.Context, workingPath, branch, submittedHead string) (string, error) {
	unsafe := func(reason string) (string, error) { return "", fmt.Errorf("%w: %s", ErrUnsafeMatch, reason) }
	if workingPath == "" || branch == "" || submittedHead == "" {
		return unsafe("missing authoring branch or submitted head")
	}
	cmd := exec.CommandContext(ctx, "git", "-C", workingPath, "worktree", "list", "--porcelain", "-z")
	winproc.Harden(cmd)
	out, err := cmd.Output()
	if err != nil {
		return unsafe("cannot read local Git worktree metadata")
	}
	var selected string
	matches := 0
	for _, record := range strings.Split(string(out), "\x00\x00") {
		var path, ref string
		bare, prunable := false, false
		for _, field := range strings.Split(record, "\x00") {
			switch {
			case strings.HasPrefix(field, "worktree "):
				path = strings.TrimPrefix(field, "worktree ")
			case strings.HasPrefix(field, "branch "):
				ref = strings.TrimPrefix(field, "branch ")
			case field == "bare":
				bare = true
			case field == "prunable" || strings.HasPrefix(field, "prunable "):
				prunable = true
			}
		}
		if !bare && ref == "refs/heads/"+branch {
			matches++
			if prunable {
				return unsafe("authoring checkout is prunable")
			}
			selected = path
		}
	}
	if matches != 1 {
		return unsafe(fmt.Sprintf("expected one authoring checkout for branch %q; found %d", branch, matches))
	}
	root := checkoutRoot(ctx, selected)
	if root == "" || root != canonicalPath(selected) {
		return unsafe("authoring checkout is missing or unresolvable")
	}
	// Re-read live identity rather than trusting potentially stale worktree rows.
	if gitOutput(ctx, root, "symbolic-ref", "-q", "HEAD") != "refs/heads/"+branch {
		return unsafe("authoring checkout is detached or changed branch")
	}
	if gitOutput(ctx, root, "rev-parse", "HEAD") != submittedHead {
		return unsafe("authoring checkout HEAD differs from the submitted head")
	}
	registered := gitRepoIdentity(ctx, workingPath)
	actual := gitRepoIdentity(ctx, root)
	if registered.commonDir == "" || actual.commonDir != registered.commonDir {
		return unsafe("authoring checkout no longer belongs to the registered Git repository")
	}
	return root, nil
}
