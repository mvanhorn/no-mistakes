package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// fakeIntentAgent always returns a canned summary - bypasses any real LLM.
type fakeIntentAgent struct{}

func (f *fakeIntentAgent) Name() string { return "fake" }
func (f *fakeIntentAgent) Run(_ context.Context, _ agent.RunOpts) (*agent.Result, error) {
	return &agent.Result{
		Output: []byte(`{"summary": "user wanted to add Bar() to internal/foo.go"}`),
		Text:   `{"summary": "user wanted to add Bar() to internal/foo.go"}`,
	}, nil
}
func (f *fakeIntentAgent) Close() error { return nil }

// initIntentRepo creates a real git repo with two commits and writes a
// matching Claude transcript fixture into a fake $HOME so the default
// intent extractor has something to discover.
func initIntentRepo(t *testing.T) (repoDir, fakeHome, base, head string) {
	t.Helper()
	repoDir = t.TempDir()
	gitCmd(t, repoDir, "init")
	gitCmd(t, repoDir, "config", "user.email", "test@example.com")
	gitCmd(t, repoDir, "config", "user.name", "Tester")
	if err := os.WriteFile(filepath.Join(repoDir, "internal_foo.go"), []byte("package foo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repoDir, "add", ".")
	gitCmd(t, repoDir, "commit", "-m", "base")
	base = gitCmd(t, repoDir, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(repoDir, "internal_foo.go"), []byte("package foo\nfunc Bar() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repoDir, "add", ".")
	gitCmd(t, repoDir, "commit", "-m", "head")
	head = gitCmd(t, repoDir, "rev-parse", "HEAD")

	fakeHome = t.TempDir()
	encoded := testClaudeProjectDirName(repoDir)
	claudeDir := filepath.Join(fakeHome, ".claude", "projects", encoded)
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	transcript := `{"type":"user","cwd":` + testJSONString(t, repoDir) + `,"timestamp":"2026-04-18T02:15:37.407Z","uuid":"u1","sessionId":"s1","message":{"role":"user","content":"please add Bar() to internal_foo.go"}}
{"type":"assistant","cwd":` + testJSONString(t, repoDir) + `,"timestamp":"2026-04-18T02:15:38.000Z","uuid":"u2","sessionId":"s1","message":{"role":"assistant","content":[{"type":"tool_use","name":"Edit","input":{"file_path":` + testJSONString(t, filepath.Join(repoDir, "internal_foo.go")) + `}}]}}
`
	if err := os.WriteFile(filepath.Join(claudeDir, "session.jsonl"), []byte(transcript), 0o644); err != nil {
		t.Fatal(err)
	}
	return
}

func testClaudeProjectDirName(cwd string) string {
	replacer := strings.NewReplacer("/", "-", `\`, "-", ":", "-")
	return replacer.Replace(cwd)
}

func testJSONString(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func withFakeHome(t *testing.T, fakeHome string) {
	t.Helper()
	t.Setenv("HOME", fakeHome)
	t.Setenv("USERPROFILE", fakeHome)
}

func openIntentTestDB(t *testing.T) *db.DB {
	t.Helper()
	dir := t.TempDir()
	d, err := db.Open(filepath.Join(dir, "test.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func newIntentIntegrationContext(t *testing.T, repoDir, base, head string, cfg *config.Config) *pipeline.StepContext {
	t.Helper()
	d := openIntentTestDB(t)
	repo, err := d.InsertRepo(repoDir, "https://example.com/r.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, gitCmd(t, repoDir, "branch", "--show-current"), head, base)
	if err != nil {
		t.Fatal(err)
	}
	return &pipeline.StepContext{
		Ctx:      context.Background(),
		Run:      run,
		Repo:     repo,
		WorkDir:  repoDir,
		Agent:    &fakeIntentAgent{},
		Config:   cfg,
		DB:       d,
		Log:      func(string) {},
		LogChunk: func(string) {},
		LogFile:  func(string) {},
	}
}

func TestIntentStep_Integration_AttachesSummaryToRun(t *testing.T) {
	repoDir, fakeHome, base, head := initIntentRepo(t)
	withFakeHome(t, fakeHome)

	cfg := &config.Config{
		Intent: config.Intent{Enabled: true, Threshold: 0.1, SlackDays: 3},
	}
	sctx := newIntentIntegrationContext(t, repoDir, base, head, cfg)

	outcome, err := (&IntentStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if outcome == nil || outcome.Skipped {
		t.Fatalf("expected non-skipped outcome, got %+v", outcome)
	}

	got, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Intent == nil || !strings.Contains(*got.Intent, "Bar()") {
		t.Errorf("Intent not attached: %+v", got.Intent)
	}
	if got.IntentSource == nil || *got.IntentSource != "claude" {
		t.Errorf("IntentSource = %v", got.IntentSource)
	}
	if got.IntentScore == nil || *got.IntentScore <= 0 {
		t.Errorf("IntentScore = %v", got.IntentScore)
	}
}

func TestIntentStep_Integration_DisabledIsNoOp(t *testing.T) {
	repoDir, fakeHome, base, head := initIntentRepo(t)
	withFakeHome(t, fakeHome)

	cfg := &config.Config{Intent: config.Intent{Enabled: false}}
	sctx := newIntentIntegrationContext(t, repoDir, base, head, cfg)

	outcome, err := (&IntentStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if outcome == nil || !outcome.Skipped {
		t.Errorf("expected Skipped when disabled, got %+v", outcome)
	}
	got, _ := sctx.DB.GetRun(sctx.Run.ID)
	if got.Intent != nil {
		t.Errorf("expected nil Intent when disabled, got %v", *got.Intent)
	}
}

func TestIntentStep_Integration_NoTranscriptIsNoOp(t *testing.T) {
	repoDir, _, base, head := initIntentRepo(t)
	emptyHome := t.TempDir()
	withFakeHome(t, emptyHome)

	cfg := &config.Config{Intent: config.Intent{Enabled: true, Threshold: 0.5, SlackDays: 3}}
	sctx := newIntentIntegrationContext(t, repoDir, base, head, cfg)

	outcome, err := (&IntentStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if outcome == nil || !outcome.Skipped {
		t.Errorf("expected Skipped with no matching transcript, got %+v", outcome)
	}
	got, _ := sctx.DB.GetRun(sctx.Run.ID)
	if got.Intent != nil {
		t.Errorf("expected nil Intent, got %v", *got.Intent)
	}
}

func TestIntentStep_Integration_DeletedFilesDoNotDiluteIntentMatch(t *testing.T) {
	repoDir := t.TempDir()
	gitCmd(t, repoDir, "init")
	gitCmd(t, repoDir, "config", "user.email", "test@example.com")
	gitCmd(t, repoDir, "config", "user.name", "Tester")
	if err := os.WriteFile(filepath.Join(repoDir, "active.go"), []byte("package active\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 40; i++ {
		name := filepath.Join(repoDir, fmt.Sprintf("obsolete_%02d.go", i))
		if err := os.WriteFile(name, []byte("package obsolete\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitCmd(t, repoDir, "add", ".")
	gitCmd(t, repoDir, "commit", "-m", "base")
	base := gitCmd(t, repoDir, "rev-parse", "HEAD")

	for i := 0; i < 40; i++ {
		if err := os.Remove(filepath.Join(repoDir, fmt.Sprintf("obsolete_%02d.go", i))); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(repoDir, "active.go"), []byte("package active\nfunc Run() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repoDir, "add", ".")
	gitCmd(t, repoDir, "commit", "-m", "replace obsolete code")
	head := gitCmd(t, repoDir, "rev-parse", "HEAD")

	fakeHome := t.TempDir()
	encoded := testClaudeProjectDirName(repoDir)
	claudeDir := filepath.Join(fakeHome, ".claude", "projects", encoded)
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	transcript := `{"type":"user","cwd":` + testJSONString(t, repoDir) + `,"timestamp":"2026-04-18T02:15:37.407Z","uuid":"u1","sessionId":"s1","message":{"role":"user","content":"please add Run() to active.go"}}
{"type":"assistant","cwd":` + testJSONString(t, repoDir) + `,"timestamp":"2026-04-18T02:15:38.000Z","uuid":"u2","sessionId":"s1","message":{"role":"assistant","content":[{"type":"tool_use","name":"Edit","input":{"file_path":` + testJSONString(t, filepath.Join(repoDir, "active.go")) + `}}]}}
`
	if err := os.WriteFile(filepath.Join(claudeDir, "session.jsonl"), []byte(transcript), 0o644); err != nil {
		t.Fatal(err)
	}
	withFakeHome(t, fakeHome)

	cfg := &config.Config{Intent: config.Intent{Enabled: true, Threshold: 0.2, SlackDays: 3}}
	sctx := newIntentIntegrationContext(t, repoDir, base, head, cfg)

	outcome, err := (&IntentStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if outcome == nil || outcome.Skipped {
		t.Fatalf("expected deleted files to be ignored for intent matching, got %+v", outcome)
	}

	got, _ := sctx.DB.GetRun(sctx.Run.ID)
	if got.Intent == nil {
		t.Fatal("intent not attached")
	}
	if got.IntentSource == nil || *got.IntentSource != "claude" {
		t.Fatalf("intent source = %v, want claude", got.IntentSource)
	}
	if got.IntentScore == nil || *got.IntentScore < 1 {
		t.Fatalf("intent score = %v, want full active.go match", got.IntentScore)
	}
	t.Logf("deleted 40 files and changed active.go; attached intent from claude session with score %.2f: %s", *got.IntentScore, *got.Intent)
}

func TestIntentStep_Integration_ZeroBaseSHA_NewBranchPush(t *testing.T) {
	repoDir, fakeHome, base, _ := initIntentRepo(t)
	withFakeHome(t, fakeHome)

	gitCmd(t, repoDir, "checkout", "-b", "feature", base)
	if err := os.WriteFile(filepath.Join(repoDir, "internal_foo.go"), []byte("package foo\nfunc Bar() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repoDir, "add", ".")
	gitCmd(t, repoDir, "commit", "-m", "branch head")
	branchHead := gitCmd(t, repoDir, "rev-parse", "HEAD")

	const zeroSHA = "0000000000000000000000000000000000000000"
	cfg := &config.Config{Intent: config.Intent{Enabled: true, Threshold: 0.1, SlackDays: 3}}
	sctx := newIntentIntegrationContext(t, repoDir, zeroSHA, branchHead, cfg)

	outcome, err := (&IntentStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if outcome == nil || outcome.Skipped {
		t.Fatalf("expected non-skipped outcome on zero-base path, got %+v", outcome)
	}

	got, _ := sctx.DB.GetRun(sctx.Run.ID)
	if got.Intent == nil {
		t.Fatal("zero-base diff path failed; intent not attached")
	}
	if !strings.Contains(*got.Intent, "Bar()") {
		t.Errorf("Intent = %q", *got.Intent)
	}
}

func TestIntentStep_Integration_UsesPipelineWorkDirForGitState(t *testing.T) {
	originRepo := t.TempDir()
	gitCmd(t, originRepo, "init")
	gitCmd(t, originRepo, "config", "user.email", "test@example.com")
	gitCmd(t, originRepo, "config", "user.name", "Tester")
	if err := os.WriteFile(filepath.Join(originRepo, "internal_foo.go"), []byte("package foo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, originRepo, "add", ".")
	gitCmd(t, originRepo, "commit", "-m", "base")
	base := gitCmd(t, originRepo, "rev-parse", "HEAD")

	fakeHome := t.TempDir()
	encoded := testClaudeProjectDirName(originRepo)
	claudeDir := filepath.Join(fakeHome, ".claude", "projects", encoded)
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	transcript := `{"type":"user","cwd":` + testJSONString(t, originRepo) + `,"timestamp":"2026-04-18T02:15:37.407Z","uuid":"u1","sessionId":"s1","message":{"role":"user","content":"please add Bar() to internal_foo.go"}}
{"type":"assistant","cwd":` + testJSONString(t, originRepo) + `,"timestamp":"2026-04-18T02:15:38.000Z","uuid":"u2","sessionId":"s1","message":{"role":"assistant","content":[{"type":"tool_use","name":"Edit","input":{"file_path":` + testJSONString(t, filepath.Join(originRepo, "internal_foo.go")) + `}}]}}
`
	if err := os.WriteFile(filepath.Join(claudeDir, "session.jsonl"), []byte(transcript), 0o644); err != nil {
		t.Fatal(err)
	}
	withFakeHome(t, fakeHome)

	pipelineWorkDir := filepath.Join(t.TempDir(), "worktree")
	gitCmd(t, t.TempDir(), "clone", originRepo, pipelineWorkDir)
	gitCmd(t, pipelineWorkDir, "config", "user.email", "test@example.com")
	gitCmd(t, pipelineWorkDir, "config", "user.name", "Tester")
	if err := os.WriteFile(filepath.Join(pipelineWorkDir, "internal_foo.go"), []byte("package foo\nfunc Bar() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, pipelineWorkDir, "add", ".")
	gitCmd(t, pipelineWorkDir, "commit", "-m", "head only in pipeline workdir")
	head := gitCmd(t, pipelineWorkDir, "rev-parse", "HEAD")

	cfg := &config.Config{Intent: config.Intent{Enabled: true, Threshold: 0.1, SlackDays: 3}}
	sctx := newIntentIntegrationContext(t, originRepo, base, head, cfg)
	sctx.WorkDir = pipelineWorkDir
	sctx.Run.SubmittedHeadSHA = &base

	outcome, err := (&IntentStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if outcome == nil || outcome.Skipped {
		t.Fatalf("expected non-skipped outcome when head exists in pipeline workdir, got %+v", outcome)
	}

	got, _ := sctx.DB.GetRun(sctx.Run.ID)
	if got.Intent == nil {
		t.Fatal("intent not attached")
	}
	if !strings.Contains(*got.Intent, "Bar()") {
		t.Errorf("Intent = %q", *got.Intent)
	}
}

// After a force push, Run.BaseSHA is the prior remote tip of the branch, which
// may be unreachable in the worktree (rewritten away or never fetched). The
// step must fall back to merge-base against the default branch instead of
// trusting the orphaned SHA, otherwise `git diff <orphaned>..<head>` fails
// with "Invalid revision range" and intent silently skips.
func TestIntentStep_Integration_ForcePushedOrphanedBaseSHA(t *testing.T) {
	repoDir, fakeHome, _, _ := initIntentRepo(t)
	withFakeHome(t, fakeHome)

	// Branch off main and add a feature commit that touches internal_foo.go.
	// initIntentRepo's main HEAD already contains func Bar(), so vary the
	// content here to produce a real diff between feature and main.
	gitCmd(t, repoDir, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(repoDir, "internal_foo.go"), []byte("package foo\nfunc Bar() { /* feature */ }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repoDir, "add", ".")
	gitCmd(t, repoDir, "commit", "-m", "feature head")
	branchHead := gitCmd(t, repoDir, "rev-parse", "HEAD")

	// Simulate a force-pushed branch: BaseSHA is a non-zero, non-existent
	// commit (the previous remote tip that got rewritten away).
	const orphanedBaseSHA = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	cfg := &config.Config{Intent: config.Intent{Enabled: true, Threshold: 0.1, SlackDays: 3}}
	sctx := newIntentIntegrationContext(t, repoDir, orphanedBaseSHA, branchHead, cfg)

	outcome, err := (&IntentStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if outcome == nil || outcome.Skipped {
		t.Fatalf("expected non-skipped outcome on force-pushed branch, got %+v", outcome)
	}

	got, _ := sctx.DB.GetRun(sctx.Run.ID)
	if got.Intent == nil {
		t.Fatal("force-pushed branch: intent not attached")
	}
	if !strings.Contains(*got.Intent, "Bar()") {
		t.Errorf("Intent = %q", *got.Intent)
	}
}

// Ensure the step honors its internal timeout by not hanging beyond it.
func TestIntentStep_Integration_RespectsTimeout(t *testing.T) {
	repoDir, fakeHome, base, head := initIntentRepo(t)
	withFakeHome(t, fakeHome)

	cfg := &config.Config{Intent: config.Intent{Enabled: true, Threshold: 0.1, SlackDays: 3}}
	sctx := newIntentIntegrationContext(t, repoDir, base, head, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sctx.Ctx = ctx

	done := make(chan struct{})
	go func() {
		(&IntentStep{}).Execute(sctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(intentExtractTimeout + 5*time.Second):
		t.Fatal("IntentStep.Execute did not return within budget")
	}
}

func TestIntentStep_Integration_ConcurrentWorktreeScope(t *testing.T) {
	for _, kind := range []string{"scoped-winner", "missing-session", "unreadable-session", "moved-head", "legacy-head", "weak-score", "ambiguous-score"} {
		t.Run(kind, func(t *testing.T) {
			repoDir, home, base, head := initIntentRepo(t)
			withFakeHome(t, home)
			author := filepath.Join(t.TempDir(), "author A")
			sibling := filepath.Join(t.TempDir(), "sibling B")
			gitCmd(t, repoDir, "worktree", "add", "-b", "author-a", author, head)
			gitCmd(t, repoDir, "worktree", "add", "-b", "sibling-b", sibling, head)
			fixture := filepath.Join(home, ".claude", "projects", testClaudeProjectDirName(repoDir), "session.jsonl")
			original, err := os.ReadFile(fixture)
			if err != nil {
				t.Fatal(err)
			}
			for _, cwd := range []string{author, sibling} {
				dir := filepath.Join(home, ".claude", "projects", testClaudeProjectDirName(cwd))
				if err := os.MkdirAll(dir, 0755); err != nil {
					t.Fatal(err)
				}
				oldJSON, newJSON := testJSONString(t, repoDir), testJSONString(t, cwd)
				content := strings.ReplaceAll(string(original), oldJSON[1:len(oldJSON)-1], newJSON[1:len(newJSON)-1])
				if cwd == sibling {
					content = strings.ReplaceAll(content, `"s1"`, `"sibling"`)
				}
				if err := os.WriteFile(filepath.Join(dir, "session.jsonl"), []byte(content), 0644); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Remove(fixture); err != nil {
				t.Fatal(err)
			}
			authorFixture := filepath.Join(home, ".claude", "projects", testClaudeProjectDirName(author), "session.jsonl")
			switch kind {
			case "missing-session":
				if err := os.Remove(authorFixture); err != nil {
					t.Fatal(err)
				}
			case "unreadable-session":
				if err := os.WriteFile(authorFixture, []byte("invalid transcript\n"), 0644); err != nil {
					t.Fatal(err)
				}
			case "moved-head":
				gitCmd(t, author, "commit", "--allow-empty", "-m", "advanced")
			case "weak-score":
				for _, name := range []string{"bar.go", "baz.go"} {
					if err := os.WriteFile(filepath.Join(author, name), []byte("package foo\n"), 0644); err != nil {
						t.Fatal(err)
					}
				}
				gitCmd(t, author, "add", ".")
				gitCmd(t, author, "commit", "-m", "multi-file change")
				head = gitCmd(t, author, "rev-parse", "HEAD")
				raw, err := os.ReadFile(authorFixture)
				if err != nil {
					t.Fatal(err)
				}
				content := strings.ReplaceAll(string(raw), "please add Bar() to internal_foo.go", "please add Bar() to internal_foo.go and bar.go")
				if err := os.WriteFile(authorFixture, []byte(content), 0644); err != nil {
					t.Fatal(err)
				}
			case "ambiguous-score":
				raw, err := os.ReadFile(authorFixture)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(filepath.Dir(authorFixture), "competitor.jsonl"), raw, 0644); err != nil {
					t.Fatal(err)
				}
			}
			sctx := newIntentIntegrationContext(t, repoDir, base, head, &config.Config{Intent: config.Intent{Enabled: true, Threshold: .2, SlackDays: 3}})
			sctx.Run.Branch = "author-a"
			if kind == "legacy-head" {
				sctx.Run.SubmittedHeadSHA = nil
			}
			counting := &countingIntentAgent{}
			sctx.Agent = counting
			outcome, err := (&IntentStep{}).Execute(sctx)
			if err != nil {
				t.Fatal(err)
			}
			got, err := sctx.DB.GetRun(sctx.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			safe := kind == "scoped-winner" || kind == "legacy-head"
			if safe {
				if outcome.NeedsApproval || got.Intent == nil || counting.calls != 1 {
					t.Fatalf("safe result %+v intent=%v calls=%d", outcome, got.Intent, counting.calls)
				}
			} else {
				assertUnsafeIntentOutcome(t, sctx, outcome)
				if counting.calls != 0 {
					t.Fatalf("unsafe inference invoked summarizer %d times", counting.calls)
				}
			}
		})
	}
}

type countingIntentAgent struct {
	fakeIntentAgent
	calls int
}

func (a *countingIntentAgent) Run(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
	a.calls++
	return a.fakeIntentAgent.Run(ctx, opts)
}

func assertUnsafeIntentOutcome(t *testing.T, sctx *pipeline.StepContext, outcome *pipeline.StepOutcome) {
	t.Helper()
	if outcome == nil || !outcome.NeedsApproval || outcome.Skipped || outcome.AutoFixable {
		t.Fatalf("unsafe outcome %+v", outcome)
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil || len(findings.Items) != 1 || findings.Items[0].Action != types.ActionAskUser || findings.Items[0].Severity != "warning" {
		t.Fatalf("findings %+v, %v", findings, err)
	}
	got, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Intent != nil || got.IntentSource != nil || got.IntentScore != nil || sctx.Run.Intent != nil {
		t.Fatalf("unsafe inference attached intent: %+v", got)
	}
}

func TestIntentStep_Integration_AllDeletionDiff(t *testing.T) {
	repo, home, base, _ := initIntentRepo(t)
	withFakeHome(t, home)
	gitCmd(t, repo, "rm", "internal_foo.go")
	gitCmd(t, repo, "commit", "-m", "delete file")
	head := gitCmd(t, repo, "rev-parse", "HEAD")
	sctx := newIntentIntegrationContext(t, repo, base, head, &config.Config{Intent: config.Intent{Enabled: true, Threshold: .2, SlackDays: 3}})
	outcome, err := (&IntentStep{}).Execute(sctx)
	if err != nil || outcome.NeedsApproval || sctx.Run.IntentScore == nil || *sctx.Run.IntentScore != 1 {
		t.Fatalf("outcome=%+v err=%v score=%v", outcome, err, sctx.Run.IntentScore)
	}
}
