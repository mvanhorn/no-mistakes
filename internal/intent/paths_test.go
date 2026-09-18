package intent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestParseWorktreeListPorcelainZ(t *testing.T) {
	raw := []byte("worktree /tmp/main repo\x00HEAD abcdef\x00branch refs/heads/main\x00\x00worktree /tmp/feature\x00HEAD 123456\x00detached\x00\x00worktree /tmp/gone\x00HEAD deadbeef\x00branch refs/heads/gone\x00prunable\x00\x00worktree /tmp/bare.git\x00bare\x00")
	got := parseWorktreeListPorcelainZ(raw)
	if len(got) != 4 {
		t.Fatalf("records = %d, want 4: %+v", len(got), got)
	}
	if got[0].Path != "/tmp/main repo" || got[0].Branch != "refs/heads/main" {
		t.Fatalf("first record = %+v", got[0])
	}
	if !got[1].Detached {
		t.Fatalf("second record should be detached: %+v", got[1])
	}
	if !got[2].Prunable {
		t.Fatalf("third record should be prunable: %+v", got[2])
	}
	if !got[3].Bare {
		t.Fatalf("fourth record should be bare: %+v", got[3])
	}
}

func TestResolveSourceCheckout_SelectsBranchWorktreeWhenWorkingPathIsOnSibling(t *testing.T) {
	ctx := context.Background()
	mainRepo, wtA, headA, _ := setupSiblingWorktrees(t)

	got, err := ResolveSourceCheckout(ctx, mainRepo, "feature-a", headA)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if canonicalPath(got.Path) != canonicalPath(wtA) {
		t.Fatalf("path = %q, want worktree A %q", got.Path, wtA)
	}
	if got.BranchRef != "refs/heads/feature-a" {
		t.Fatalf("branch = %q", got.BranchRef)
	}
	if !headSHAsEqual(got.HeadSHA, headA) {
		t.Fatalf("head = %q, want %q", got.HeadSHA, headA)
	}
}

func TestResolveSourceCheckout_SubdirectoryAndSymlinkAliasSameCheckout(t *testing.T) {
	ctx := context.Background()
	_, wtA, headA, _ := setupSiblingWorktrees(t)

	sub := filepath.Join(wtA, "src")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	fromSub, err := ResolveSourceCheckout(ctx, sub, "feature-a", headA)
	if err != nil {
		t.Fatalf("resolve from subdirectory: %v", err)
	}
	if canonicalPath(fromSub.Path) != canonicalPath(wtA) {
		t.Fatalf("subdirectory resolved to %q, want %q", fromSub.Path, wtA)
	}

	if runtime.GOOS == "windows" {
		t.Log("skipping symlink alias on windows")
		return
	}
	alias := filepath.Join(t.TempDir(), "alias-a")
	if err := os.Symlink(wtA, alias); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	fromLink, err := ResolveSourceCheckout(ctx, alias, "feature-a", headA)
	if err != nil {
		t.Fatalf("resolve from symlink: %v", err)
	}
	if canonicalPath(fromLink.Path) != canonicalPath(wtA) {
		t.Fatalf("symlink resolved to %q, want %q", fromLink.Path, wtA)
	}
}

func TestResolveSourceCheckout_PathPrefixSiblingIsNotTheSameCheckout(t *testing.T) {
	ctx := context.Background()
	mainRepo, _, headA, _ := setupSiblingWorktrees(t)
	prefixSibling := mainRepo + "-extra"
	if err := os.MkdirAll(prefixSibling, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := ResolveSourceCheckout(ctx, prefixSibling, "feature-a", headA)
	if !errors.Is(err, ErrUnsafeMatch) {
		t.Fatalf("path-prefix sibling error = %v, want ErrUnsafeMatch", err)
	}
}

func TestResolveSourceCheckout_MissingDetachedMismatchedAndGitFailure(t *testing.T) {
	ctx := context.Background()
	mainRepo, wtA, headA, _ := setupSiblingWorktrees(t)

	if _, err := ResolveSourceCheckout(ctx, mainRepo, "does-not-exist", headA); !errors.Is(err, ErrUnsafeMatch) {
		t.Fatalf("missing branch error = %v, want ErrUnsafeMatch", err)
	}

	gitTestOutput(t, mainRepo, "worktree", "remove", "--force", wtA)
	detached := filepath.Join(t.TempDir(), "detached")
	gitTestOutput(t, mainRepo, "worktree", "add", "--detach", detached, headA)
	t.Cleanup(func() { gitTestOutput(t, mainRepo, "worktree", "remove", "--force", detached) })
	if _, err := ResolveSourceCheckout(ctx, mainRepo, "feature-a", headA); !errors.Is(err, ErrUnsafeMatch) {
		t.Fatalf("detached checkout error = %v, want ErrUnsafeMatch", err)
	}

	if _, err := ResolveSourceCheckout(ctx, mainRepo, "feature-b", headA); !errors.Is(err, ErrUnsafeMatch) {
		t.Fatalf("head mismatch error = %v, want ErrUnsafeMatch", err)
	}

	notGit := t.TempDir()
	if _, err := ResolveSourceCheckout(ctx, notGit, "feature-a", headA); !errors.Is(err, ErrUnsafeMatch) {
		t.Fatalf("git failure error = %v, want ErrUnsafeMatch", err)
	}

	missing := filepath.Join(t.TempDir(), "gone")
	if _, err := ResolveSourceCheckout(ctx, missing, "feature-a", headA); !errors.Is(err, ErrUnsafeMatch) {
		t.Fatalf("missing path error = %v, want ErrUnsafeMatch", err)
	}
}

func TestResolveSourceCheckout_SameRemoteCloneIsNotListed(t *testing.T) {
	ctx := context.Background()
	mainRepo, wtA, headA, _ := setupSiblingWorktrees(t)
	clone := filepath.Join(t.TempDir(), "clone")
	gitTestOutput(t, t.TempDir(), "clone", mainRepo, clone)
	gitTestOutput(t, clone, "checkout", "-B", "feature-a", headA)

	got, err := ResolveSourceCheckout(ctx, mainRepo, "feature-a", headA)
	if err != nil {
		t.Fatalf("resolve from original: %v", err)
	}
	if canonicalPath(got.Path) != canonicalPath(wtA) {
		t.Fatalf("path = %q, want original worktree %q not clone %q", got.Path, wtA, clone)
	}
}

func TestSourceCheckoutVerify_FailsWhenBranchSwitches(t *testing.T) {
	ctx := context.Background()
	mainRepo, _, _, headB := setupSiblingWorktrees(t)
	got, err := ResolveSourceCheckout(ctx, mainRepo, "feature-b", headB)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	gitTestOutput(t, mainRepo, "checkout", "--detach", headB)
	if err := got.Verify(ctx); !errors.Is(err, ErrUnsafeMatch) {
		t.Fatalf("verify after detach = %v, want ErrUnsafeMatch", err)
	}
}

func TestSessionInSourceCheckout_DistinguishesAliasesFromSiblings(t *testing.T) {
	ctx := context.Background()
	_, wtA, _, _ := setupSiblingWorktrees(t)
	origin := canonicalPath(wtA)
	sub := filepath.Join(wtA, "pkg")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if !sessionInSourceCheckout(ctx, wtA, origin) {
		t.Fatal("exact checkout path should match")
	}
	if !sessionInSourceCheckout(ctx, sub, origin) {
		t.Fatal("subdirectory of the checkout should match")
	}
	if sessionInSourceCheckout(ctx, "", origin) {
		t.Fatal("empty CWD must not match")
	}
	if sessionInSourceCheckout(ctx, origin+"-extra", origin) {
		t.Fatal("path-prefix sibling must not match")
	}
}

func setupSiblingWorktrees(t *testing.T) (mainRepo, wtA, headA, headB string) {
	t.Helper()
	mainRepo = t.TempDir()
	gitTestOutput(t, mainRepo, "init", "-b", "main")
	gitTestOutput(t, mainRepo, "config", "core.autocrlf", "false")
	writeFile(t, filepath.Join(mainRepo, "README.md"), "base\n")
	gitTestOutput(t, mainRepo, "add", "README.md")
	gitTestOutput(t, mainRepo, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "base")

	gitTestOutput(t, mainRepo, "checkout", "-b", "feature-a")
	writeFile(t, filepath.Join(mainRepo, "a.txt"), "A\n")
	gitTestOutput(t, mainRepo, "add", "a.txt")
	gitTestOutput(t, mainRepo, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "a")
	headA = gitTestOutput(t, mainRepo, "rev-parse", "HEAD")

	gitTestOutput(t, mainRepo, "checkout", "main")
	wtA = filepath.Join(t.TempDir(), "wt A")
	gitTestOutput(t, mainRepo, "worktree", "add", wtA, "feature-a")

	gitTestOutput(t, mainRepo, "checkout", "-b", "feature-b")
	writeFile(t, filepath.Join(mainRepo, "b.txt"), "B\n")
	gitTestOutput(t, mainRepo, "add", "b.txt")
	gitTestOutput(t, mainRepo, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "b")
	headB = gitTestOutput(t, mainRepo, "rev-parse", "HEAD")
	return mainRepo, wtA, headA, headB
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
