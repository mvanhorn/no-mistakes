package intent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func newScopeRepo(t *testing.T) string {
	t.Helper()
	t.Setenv("GIT_CONFIG_COUNT", "0")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("NM_HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	repo := t.TempDir()
	gitTestCmd(t, repo, "init", "-b", "main")
	gitTestCmd(t, repo, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "base")
	return repo
}

func TestResolveAuthoringCheckout(t *testing.T) {
	ctx := context.Background()
	repo := newScopeRepo(t)
	head := gitTestCmd(t, repo, "rev-parse", "HEAD")
	a := filepath.Join(t.TempDir(), "branch A with spaces")
	b := filepath.Join(t.TempDir(), "branch B")
	gitTestCmd(t, repo, "worktree", "add", "-b", "branch-a", a)
	gitTestCmd(t, repo, "worktree", "add", "-b", "branch-b", b)
	sub := filepath.Join(repo, "subdir")
	if err := os.Mkdir(sub, 0755); err != nil {
		t.Fatal(err)
	}
	for _, origin := range []string{repo, sub, b} {
		got, err := ResolveAuthoringCheckout(ctx, origin, "branch-a", head)
		if err != nil || got != canonicalPath(a) {
			t.Fatalf("origin %q: got %q, %v", origin, got, err)
		}
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(repo, alias); err == nil {
		got, err := ResolveAuthoringCheckout(ctx, alias, "branch-a", head)
		if err != nil || got != canonicalPath(a) {
			t.Fatalf("symlink: %q, %v", got, err)
		}
	}
	for _, tt := range []struct{ name, origin, branch, head string }{
		{"unknown branch", repo, "absent", head},
		{"wrong head", repo, "branch-a", "deadbeef"},
		{"no git metadata", t.TempDir(), "branch-a", head},
		{"missing path", filepath.Join(t.TempDir(), "missing"), "branch-a", head},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got, err := ResolveAuthoringCheckout(ctx, tt.origin, tt.branch, tt.head); !errors.Is(err, ErrUnsafeMatch) || got != "" {
				t.Fatalf("got %q, %v", got, err)
			}
		})
	}
	gitTestCmd(t, a, "checkout", "--detach")
	if _, err := ResolveAuthoringCheckout(ctx, repo, "branch-a", head); !errors.Is(err, ErrUnsafeMatch) {
		t.Fatalf("detached: %v", err)
	}
	gitTestCmd(t, a, "checkout", "branch-a")
	// Git allows duplicate branch checkouts with --force; neither can be chosen.
	duplicate := filepath.Join(t.TempDir(), "duplicate")
	gitTestCmd(t, repo, "worktree", "add", "--force", duplicate, "branch-a")
	if _, err := ResolveAuthoringCheckout(ctx, repo, "branch-a", head); !errors.Is(err, ErrUnsafeMatch) {
		t.Fatalf("duplicate: %v", err)
	}
	gitTestCmd(t, repo, "worktree", "remove", duplicate)
	if err := os.RemoveAll(a); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveAuthoringCheckout(ctx, repo, "branch-a", head); !errors.Is(err, ErrUnsafeMatch) {
		t.Fatalf("missing/prunable: %v", err)
	}
	gitTestCmd(t, repo, "worktree", "prune")
	if _, err := ResolveAuthoringCheckout(ctx, repo, "branch-a", head); !errors.Is(err, ErrUnsafeMatch) {
		t.Fatalf("pruned: %v", err)
	}
}

func TestCheckoutRootRejectsOtherRepositories(t *testing.T) {
	ctx := context.Background()
	repo := newScopeRepo(t)
	sub := filepath.Join(repo, "sub")
	if err := os.Mkdir(sub, 0755); err != nil {
		t.Fatal(err)
	}
	if got := checkoutRoot(ctx, sub); got != canonicalPath(repo) {
		t.Fatalf("subdir root %q", got)
	}
	gitTestCmd(t, sub, "init")
	if got := checkoutRoot(ctx, sub); got == checkoutRoot(ctx, repo) {
		t.Fatal("nested repository accepted")
	}
	clone := filepath.Join(t.TempDir(), "clone")
	gitTestCmd(t, repo, "remote", "add", "origin", "https://example.com/repo.git")
	gitTestCmd(t, repo, "clone", repo, clone)
	gitTestCmd(t, clone, "remote", "set-url", "origin", "https://example.com/repo.git")
	if !newRepoMatcher(ctx, repo).matches(ctx, clone) {
		t.Fatal("clone should remain discoverable")
	}
	if checkoutRoot(ctx, clone) == checkoutRoot(ctx, repo) {
		t.Fatal("independent clone eligible")
	}
	for _, p := range []string{"", filepath.Join(repo, "missing")} {
		if checkoutRoot(ctx, p) != "" {
			t.Fatalf("invalid path %q accepted", p)
		}
	}
}

func TestResolveAuthoringCheckout_UnavailableGit(t *testing.T) {
	repo := newScopeRepo(t)
	head := gitTestCmd(t, repo, "rev-parse", "HEAD")
	t.Setenv("PATH", t.TempDir())
	if _, err := ResolveAuthoringCheckout(context.Background(), repo, "main", head); !errors.Is(err, ErrUnsafeMatch) {
		t.Fatalf("missing Git: %v", err)
	}
}

func TestExtract_CheckoutAliasesAndNestedRepository(t *testing.T) {
	ctx := context.Background()
	repo := newScopeRepo(t)
	sub := filepath.Join(repo, "sub")
	if err := os.Mkdir(sub, 0755); err != nil {
		t.Fatal(err)
	}
	aliases := []string{repo, sub}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(repo, alias); err == nil {
		aliases = append(aliases, alias)
	}
	for _, cwd := range aliases {
		r := &staticReader{name: "test", sessions: []*Session{{SessionID: "s", CWD: cwd, Messages: []Message{{FilePaths: []string{"foo.go"}}}}}}
		got, err := Extract(ctx, ExtractParams{OriginCWD: repo, DiffFiles: []string{"foo.go"}, Readers: []Reader{r}, Summarizer: &fixedSummarizer{summary: "safe"}})
		if err != nil || got == nil {
			t.Fatalf("alias %q: %+v, %v", cwd, got, err)
		}
	}
	gitTestCmd(t, sub, "init")
	r := &staticReader{name: "future-reader", sessions: []*Session{{SessionID: "nested", CWD: sub}}}
	if _, err := Extract(ctx, ExtractParams{OriginCWD: repo, DiffFiles: []string{"foo.go"}, Readers: []Reader{r}, Summarizer: &fixedSummarizer{}}); !errors.Is(err, ErrUnsafeMatch) {
		t.Fatalf("nested repo: %v", err)
	}
	if len(r.loaded) != 0 {
		t.Fatal("loaded nested repository transcript")
	}
}
