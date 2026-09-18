package intent

import (
	"bytes"
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

// SourceCheckout is the unique local working tree that can supply transcript
// evidence for a run. Path is the canonical top-level of that checkout.
type SourceCheckout struct {
	Path      string
	BranchRef string
	HeadSHA   string
	CommonDir string
	bare      bool
	detached  bool
}

type worktreeRecord struct {
	Path     string
	Head     string
	Branch   string
	Bare     bool
	Detached bool
	Prunable bool
}

// ResolveSourceCheckout enumerates local worktrees of the registered working
// repository and returns the unique accessible, non-bare checkout on
// refs/heads/<branch> whose HEAD equals submittedHead. Detached gate
// worktrees and separate same-remote clones are not source evidence. Missing
// or non-unique identity returns ErrUnsafeMatch with diagnostics.
func ResolveSourceCheckout(ctx context.Context, workingPath, branch, submittedHead string) (*SourceCheckout, error) {
	workingPath = strings.TrimSpace(workingPath)
	if workingPath == "" {
		return nil, unsafeMatchError("registered working path is empty")
	}
	if err := ctx.Err(); err != nil {
		return nil, unsafeMatchError("source checkout lookup canceled: %v", err)
	}
	if _, err := os.Stat(workingPath); err != nil {
		return nil, unsafeMatchError("registered working path %s is not accessible: %v", workingPath, err)
	}

	branchRef := headsBranchRef(branch)
	if branchRef == "" {
		return nil, unsafeMatchError("run branch is empty")
	}
	submittedHead = strings.TrimSpace(submittedHead)
	if submittedHead == "" {
		return nil, unsafeMatchError("submitted head SHA is empty")
	}

	registeredTop, err := gitShowToplevelErr(ctx, workingPath)
	if err != nil {
		return nil, unsafeMatchError("cannot resolve git top-level of %s: %v", workingPath, err)
	}
	registeredCommon, err := gitCommonDirErr(ctx, workingPath)
	if err != nil {
		return nil, unsafeMatchError("cannot resolve git common dir of %s: %v", workingPath, err)
	}

	raw, err := gitOutputRawErr(ctx, registeredTop, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return nil, unsafeMatchError("git worktree list failed for %s: %v", registeredTop, err)
	}
	records := parseWorktreeListPorcelainZ(raw)

	var matches []*SourceCheckout
	var skipped []string
	for _, rec := range records {
		checkout, why, err := inspectWorktreeRecord(ctx, rec, registeredCommon, branchRef, submittedHead)
		if err != nil {
			return nil, err
		}
		if checkout != nil {
			matches = append(matches, checkout)
			continue
		}
		if why != "" {
			skipped = append(skipped, why)
		}
	}

	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) == 0 {
		detail := "none of the local worktrees are on " + branchRef + " at " + submittedHead
		if len(skipped) > 0 {
			detail += "; observed: " + strings.Join(skipped, "; ")
		}
		return nil, unsafeMatchError("source checkout for %s at %s is not available: %s", branchRef, submittedHead, detail)
	}
	var paths []string
	for _, m := range matches {
		paths = append(paths, m.Path)
	}
	return nil, unsafeMatchError("source checkout for %s at %s is not unique: %s", branchRef, submittedHead, strings.Join(paths, ", "))
}

// Verify re-reads git identity from the checkout. A branch switch, head
// movement, or disappeared path during extraction fails closed.
func (c *SourceCheckout) Verify(ctx context.Context) error {
	if c == nil {
		return unsafeMatchError("source checkout is nil")
	}
	if err := ctx.Err(); err != nil {
		return unsafeMatchError("source checkout recheck canceled: %v", err)
	}
	live, err := readCheckoutIdentity(ctx, c.Path)
	if err != nil {
		return unsafeMatchError("source checkout %s could not be rechecked: %v", c.Path, err)
	}
	if live.bare {
		return unsafeMatchError("source checkout %s became bare during extraction", c.Path)
	}
	if live.detached {
		return unsafeMatchError("source checkout %s became detached during extraction", c.Path)
	}
	if canonicalPath(live.Path) != canonicalPath(c.Path) {
		return unsafeMatchError("source checkout path changed from %s to %s during extraction", c.Path, live.Path)
	}
	if live.BranchRef != c.BranchRef {
		return unsafeMatchError("source checkout %s moved from %s to %s during extraction", c.Path, c.BranchRef, live.BranchRef)
	}
	if !headSHAsEqual(live.HeadSHA, c.HeadSHA) {
		return unsafeMatchError("source checkout %s HEAD moved from %s to %s during extraction", c.Path, c.HeadSHA, live.HeadSHA)
	}
	if live.CommonDir != c.CommonDir {
		return unsafeMatchError("source checkout %s common dir changed from %s to %s during extraction", c.Path, c.CommonDir, live.CommonDir)
	}
	return nil
}

func inspectWorktreeRecord(ctx context.Context, rec worktreeRecord, registeredCommon, branchRef, submittedHead string) (*SourceCheckout, string, error) {
	if rec.Bare {
		path := rec.Path
		if path == "" {
			path = "bare"
		}
		return nil, path + " is bare", nil
	}
	if rec.Path == "" {
		return nil, "worktree record missing path", nil
	}
	info, err := os.Stat(rec.Path)
	if err != nil {
		if rec.Prunable || os.IsNotExist(err) {
			return nil, rec.Path + " is missing or pruned", nil
		}
		return nil, "", unsafeMatchError("cannot stat worktree %s: %v", rec.Path, err)
	}
	if !info.IsDir() {
		return nil, rec.Path + " is not a directory", nil
	}

	live, err := readCheckoutIdentity(ctx, rec.Path)
	if err != nil {
		return nil, rec.Path + " identity unreadable: " + err.Error(), nil
	}
	if live.bare {
		return nil, live.Path + " is bare", nil
	}
	if live.CommonDir == "" || live.CommonDir != registeredCommon {
		return nil, live.Path + " does not share the registered repository common dir", nil
	}
	if live.detached || live.BranchRef == "" {
		return nil, live.Path + " is detached", nil
	}
	if live.BranchRef != branchRef {
		return nil, live.Path + " is on " + live.BranchRef, nil
	}
	if !headSHAsEqual(live.HeadSHA, submittedHead) {
		return nil, live.Path + " HEAD " + live.HeadSHA + " != " + submittedHead, nil
	}
	return live, "", nil
}

func readCheckoutIdentity(ctx context.Context, dir string) (*SourceCheckout, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	top, err := gitShowToplevelErr(ctx, dir)
	if err != nil {
		return nil, err
	}
	common, err := gitCommonDirErr(ctx, dir)
	if err != nil {
		return nil, err
	}
	bareOut, err := gitOutputErr(ctx, dir, "rev-parse", "--is-bare-repository")
	if err != nil {
		return nil, err
	}
	head, err := gitOutputErr(ctx, dir, "rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	checkout := &SourceCheckout{
		Path:      top,
		HeadSHA:   strings.TrimSpace(head),
		CommonDir: common,
		bare:      strings.EqualFold(strings.TrimSpace(bareOut), "true"),
	}
	ref, err := gitOutputErr(ctx, dir, "symbolic-ref", "-q", "HEAD")
	if err != nil || strings.TrimSpace(ref) == "" {
		checkout.detached = true
		return checkout, nil
	}
	checkout.BranchRef = strings.TrimSpace(ref)
	return checkout, nil
}

func parseWorktreeListPorcelainZ(data []byte) []worktreeRecord {
	data = bytes.TrimRight(data, "\x00")
	if len(data) == 0 {
		return nil
	}
	var records []worktreeRecord
	for _, rec := range bytes.Split(data, []byte{0, 0}) {
		if len(rec) == 0 {
			continue
		}
		var wt worktreeRecord
		for _, field := range bytes.Split(rec, []byte{0}) {
			line := string(field)
			if line == "" {
				continue
			}
			key, val, _ := strings.Cut(line, " ")
			switch key {
			case "worktree":
				wt.Path = val
			case "HEAD":
				wt.Head = val
			case "branch":
				wt.Branch = val
			case "bare":
				wt.Bare = true
			case "detached":
				wt.Detached = true
			case "prunable":
				wt.Prunable = true
			}
		}
		if wt.Path != "" || wt.Bare {
			records = append(records, wt)
		}
	}
	return records
}

func headsBranchRef(branch string) string {
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return ""
	}
	if strings.HasPrefix(branch, "refs/heads/") {
		return branch
	}
	return "refs/heads/" + strings.TrimPrefix(branch, "refs/")
}

func headSHAsEqual(a, b string) bool {
	a = strings.TrimSpace(a)
	b = strings.TrimSpace(b)
	if a == "" || b == "" {
		return false
	}
	return strings.EqualFold(a, b)
}

func canonicalSourceCheckoutPath(ctx context.Context, p string) string {
	if top := gitShowToplevel(ctx, p); top != "" {
		return top
	}
	return canonicalPath(p)
}

func sessionInSourceCheckout(ctx context.Context, sessionCWD, originTop string) bool {
	originTop = canonicalPath(originTop)
	sessionCWD = strings.TrimSpace(sessionCWD)
	if originTop == "" || sessionCWD == "" {
		return false
	}
	sessionCanon := canonicalPath(sessionCWD)
	if top := gitShowToplevel(ctx, sessionCWD); top != "" {
		return top == originTop
	}
	if sessionCanon == originTop {
		return true
	}
	return pathIsWithinCheckout(sessionCanon, originTop)
}

func pathIsWithinCheckout(session, origin string) bool {
	if session == "" || origin == "" {
		return false
	}
	rel, err := filepath.Rel(origin, session)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	if rel == "" || strings.HasPrefix(rel, "..") {
		return false
	}
	return !strings.Contains(rel, ".."+string(filepath.Separator)) && rel != ".."
}

func gitShowToplevel(ctx context.Context, dir string) string {
	top, err := gitShowToplevelErr(ctx, dir)
	if err != nil {
		return ""
	}
	return top
}

func gitShowToplevelErr(ctx context.Context, dir string) (string, error) {
	out, err := gitOutputErr(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	if out == "" {
		return "", fmt.Errorf("empty --show-toplevel")
	}
	return canonicalPath(out), nil
}

func gitCommonDirErr(ctx context.Context, dir string) (string, error) {
	out, err := gitOutputErr(ctx, dir, "rev-parse", "--git-common-dir")
	if err != nil {
		return "", err
	}
	if out == "" {
		return "", fmt.Errorf("empty --git-common-dir")
	}
	if !filepath.IsAbs(out) {
		out = filepath.Join(dir, out)
	}
	return canonicalPath(out), nil
}

func gitOutputErr(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := gitOutputRawErr(ctx, dir, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func gitOutputRawErr(ctx context.Context, dir string, args ...string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	winproc.Harden(cmd)
	out, err := cmd.Output()
	if err != nil {
		stderr := ""
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = strings.TrimSpace(string(ee.Stderr))
		}
		if stderr != "" {
			return nil, fmt.Errorf("git %s: %w (%s)", strings.Join(args, " "), err, stderr)
		}
		return nil, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return out, nil
}
