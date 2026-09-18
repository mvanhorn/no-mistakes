package intent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExtract_EndToEndWithClaudeFixture(t *testing.T) {
	repoCWD := t.TempDir()
	home := writeClaudeFixture(t, repoCWD, []string{
		`{"type":"user","cwd":` + jsonString(t, repoCWD) + `,"timestamp":"2026-04-18T02:15:37.407Z","uuid":"u1","sessionId":"s1","message":{"role":"user","content":"please rewrite internal/foo.go to add a Bar() function"}}`,
		`{"type":"assistant","cwd":` + jsonString(t, repoCWD) + `,"timestamp":"2026-04-18T02:15:38.000Z","uuid":"u2","sessionId":"s1","message":{"role":"assistant","content":[{"type":"tool_use","name":"Edit","input":{"file_path":` + jsonString(t, filepath.Join(repoCWD, "internal", "foo.go")) + `}}]}}`,
	})

	fa := &fakeAgent{output: `{"summary": "user wanted Bar() helper in internal/foo.go"}`}

	got, err := Extract(context.Background(), ExtractParams{
		HomeDir:    home,
		OriginCWD:  repoCWD,
		DiffFiles:  []string{"internal/foo.go"},
		BaseTime:   time.Now().Add(-time.Hour),
		HeadTime:   time.Now(),
		SlackDays:  3,
		Threshold:  0.2,
		Readers:    AllReaders(nil),
		Cache:      NewMemCache(),
		Summarizer: NewAgentSummarizer(fa, ""),
	})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if got.AgentName != "claude" {
		t.Errorf("AgentName = %q", got.AgentName)
	}
	if got.Summary != "user wanted Bar() helper in internal/foo.go" {
		t.Errorf("summary = %q", got.Summary)
	}
}

func TestExtract_EndToEndWithPiFixture(t *testing.T) {
	repoCWD := t.TempDir()
	home := writePiFixture(t, repoCWD)

	fa := &fakeAgent{output: `{"summary": "user wanted foo helper changes in internal/foo.go"}`}

	got, err := Extract(context.Background(), ExtractParams{
		HomeDir:    home,
		OriginCWD:  repoCWD,
		DiffFiles:  []string{"internal/foo.go"},
		BaseTime:   time.Now().Add(-time.Hour),
		HeadTime:   time.Now(),
		SlackDays:  3,
		Threshold:  0.2,
		Readers:    AllReaders(nil),
		Cache:      NewMemCache(),
		Summarizer: NewAgentSummarizer(fa, ""),
	})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if got.AgentName != "pi" {
		t.Errorf("AgentName = %q", got.AgentName)
	}
	if got.SessionID != "session-1" {
		t.Errorf("SessionID = %q", got.SessionID)
	}
	if got.Summary != "user wanted foo helper changes in internal/foo.go" {
		t.Errorf("summary = %q", got.Summary)
	}
	if !strings.Contains(fa.lastPrompt, "please add a foo helper to internal/foo.go") {
		t.Errorf("prompt should include Pi user text, got %q", fa.lastPrompt)
	}
	if strings.Contains(fa.lastPrompt, "private chain of thought") || strings.Contains(fa.lastPrompt, "tool output should not become transcript text") {
		t.Errorf("prompt leaked non-user-intent Pi content: %q", fa.lastPrompt)
	}
}

func TestExtract_EndToEndSelectsOwnCheckoutAndExcludesSiblingAndClone(t *testing.T) {
	ctx := context.Background()
	mainRepo, wtA, headA, _ := setupSiblingWorktrees(t)
	checkout, err := ResolveSourceCheckout(ctx, mainRepo, "feature-a", headA)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	clone := filepath.Join(t.TempDir(), "clone")
	gitTestOutput(t, t.TempDir(), "clone", mainRepo, clone)
	gitTestOutput(t, clone, "checkout", "-B", "feature-a", headA)

	home := t.TempDir()
	writeClaudeSession(t, home, wtA, "own-session", "please add AlphaGoal to a.txt", filepath.Join(wtA, "a.txt"))
	writeClaudeSession(t, home, mainRepo, "sibling-session", "please add BetaGoal to b.txt", filepath.Join(mainRepo, "b.txt"))
	writeClaudeSession(t, home, clone, "clone-session", "please add GammaGoal to a.txt", filepath.Join(clone, "a.txt"))

	fa := &fakeAgent{output: `{"summary": "user wanted AlphaGoal"}`}
	got, err := Extract(ctx, ExtractParams{
		HomeDir:    home,
		OriginCWD:  checkout.Path,
		DiffFiles:  []string{"a.txt"},
		BaseTime:   time.Now().Add(-time.Hour),
		HeadTime:   time.Now(),
		SlackDays:  3,
		Threshold:  0.2,
		Readers:    AllReaders(nil),
		Cache:      NewMemCache(),
		Summarizer: NewAgentSummarizer(fa, ""),
		Recheck:    func() error { return checkout.Verify(ctx) },
	})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if got.SessionID != "own-session" {
		t.Fatalf("selected session = %q, want own-session", got.SessionID)
	}
	if !strings.Contains(fa.lastPrompt, "AlphaGoal") {
		t.Fatalf("summarizer prompt missing own-checkout text: %q", fa.lastPrompt)
	}
	if strings.Contains(fa.lastPrompt, "BetaGoal") || strings.Contains(fa.lastPrompt, "GammaGoal") {
		t.Fatalf("summarizer prompt leaked sibling/clone text: %q", fa.lastPrompt)
	}
}

func writeClaudeSession(t *testing.T, home, cwd, sessionID, userText, editedFile string) {
	t.Helper()
	encoded := claudeProjectDirName(cwd)
	dir := filepath.Join(home, ".claude", "projects", encoded)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	lines := []string{
		`{"type":"user","cwd":` + jsonString(t, cwd) + `,"timestamp":"2026-04-18T02:15:37.407Z","uuid":"u1","sessionId":` + jsonString(t, sessionID) + `,"message":{"role":"user","content":` + jsonString(t, userText) + `}}`,
		`{"type":"assistant","cwd":` + jsonString(t, cwd) + `,"timestamp":"2026-04-18T02:15:38.000Z","uuid":"u2","sessionId":` + jsonString(t, sessionID) + `,"message":{"role":"assistant","content":[{"type":"tool_use","name":"Edit","input":{"file_path":` + jsonString(t, editedFile) + `}}]}}`,
	}
	if err := os.WriteFile(filepath.Join(dir, sessionID+".jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}
