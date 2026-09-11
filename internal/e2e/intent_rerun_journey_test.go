//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestRerunIntentProvenanceJourney drives the public CLI through both rerun
// defaults and both explicit overrides. The assertions inspect the persisted
// intent and the intent-step log so a rerun cannot silently switch from
// inheritance to transcript inference.
func TestRerunIntentProvenanceJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: writeIntentScenario(t)})
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("nm init: %v\n%s", err, out)
	}

	explicit := "preserve the complete accepted requirements\nincluding exclusions and acceptance criteria\nwithout condensing them"
	explicitBranch := "feature/rerun-explicit"
	h.CommitChange(explicitBranch, "explicit.txt", "explicit\n", "add explicit fixture")
	explicitWT := h.AddWorktree(explicitBranch)
	h.PushToGate(explicitBranch)
	h.WaitForRun(explicitBranch, 90*time.Second)
	if out, err := h.RunInDir(explicitWT, "rerun", "--intent", " "); err == nil || !strings.Contains(out, "--intent must not be empty") {
		t.Fatalf("blank rerun intent should be rejected, err=%v output=%s", err, out)
	} else {
		t.Logf("CLI rejected blank explicit intent: %s", strings.TrimSpace(out))
	}
	originalExplicit := runCLIAndWait(t, h, explicitWT, explicitBranch, "rerun", "--intent", explicit)
	assertCompletedRerunFixture(t, h, originalExplicit, explicit, "agent")

	inherited := runCLIAndWait(t, h, explicitWT, explicitBranch, "rerun")
	assertCompletedRerunFixture(t, h, inherited, explicit, "rerun")
	if log := readStepLog(t, h, inherited.ID, "intent"); !strings.Contains(log, "using intent supplied by the agent") {
		t.Errorf("inherited explicit rerun should skip inference, log:\n%s", log)
	}

	overriddenExplicit := runCLIAndWait(t, h, explicitWT, explicitBranch, "rerun", "--intent", "replace the canonical requirements explicitly")
	assertCompletedRerunFixture(t, h, overriddenExplicit, "replace the canonical requirements explicitly", "agent")

	inferredBranch := "feature/rerun-inferred"
	h.CommitChange(inferredBranch, "inferred.txt", "inferred\n", "add inferred fixture")
	inferredWT := h.AddWorktree(inferredBranch)
	seedClaudeTranscript(t, h.HomeDir, inferredWT, "inferred.txt")
	h.PushToGate(inferredBranch)
	originalInferred := h.WaitForRun(inferredBranch, 90*time.Second)
	assertCompletedRerunFixture(t, h, originalInferred, "user wanted Bar() helper added", "claude")

	fresh := runCLIAndWait(t, h, inferredWT, inferredBranch, "rerun")
	originalInferredIntent := readRunIntent(t, h.NMHome, originalInferred.ID)
	if originalInferredIntent.summary == nil {
		t.Fatal("inferred original has no intent")
	}
	assertCompletedRerunFixture(t, h, fresh, *originalInferredIntent.summary, "claude")
	if log := readStepLog(t, h, fresh.ID, "intent"); !strings.Contains(log, "scanning recent agent transcripts") {
		t.Errorf("non-explicit rerun should perform fresh inference, log:\n%s", log)
	}

	// The original inferred summary must not be inherited when current evidence
	// is ambiguous, even when it is already cached from the earlier run.
	fixtureDir := filepath.Join(h.HomeDir, ".claude", "projects", testClaudeProjectDirName(inferredWT))
	raw, err := os.ReadFile(filepath.Join(fixtureDir, "e2e-session.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	competitor := strings.ReplaceAll(string(raw), "e2e-session", "competing-session")
	if err := os.WriteFile(filepath.Join(fixtureDir, "competing-session.jsonl"), []byte(competitor), 0644); err != nil {
		t.Fatal(err)
	}
	before := len(h.AgentInvocations())
	if out, err := h.RunInDir(inferredWT, "rerun"); err != nil {
		t.Fatalf("ambiguous rerun: %v\n%s", err, out)
	}
	parked := waitForStepStatus(t, h, inferredBranch, types.StepIntent, types.StepStatusAwaitingApproval, 60*time.Second)
	assertNoInferredIntent(t, h, parked.ID)
	step, ok := findStep(parked.Steps, types.StepIntent)
	if !ok || step.FindingsJSON == nil {
		t.Fatal("missing parked intent findings")
	}
	findings, err := types.ParseFindingsJSON(*step.FindingsJSON)
	if err != nil || len(findings.Items) != 1 || findings.Items[0].Action != types.ActionAskUser {
		t.Fatalf("findings %+v: %v", findings, err)
	}
	for _, inv := range h.AgentInvocations()[before:] {
		if strings.Contains(inv.Prompt, "Review the code changes") || strings.Contains(inv.Prompt, "transcript of a developer's recent conversation") {
			t.Fatal("unsafe rerun reached an agent before a decision")
		}
	}
	h.Respond(parked.ID, types.StepIntent, types.ActionApprove)
	completed := h.WaitForRun(inferredBranch, 90*time.Second)
	if completed.Status != types.RunCompleted {
		t.Fatalf("approved rerun: %s", completed.Status)
	}
	assertNoInferredIntent(t, h, completed.ID)

	overriddenInferred := runCLIAndWait(t, h, inferredWT, inferredBranch, "rerun", "--intent", "override inferred intent")
	assertCompletedRerunFixture(t, h, overriddenInferred, "override inferred intent", "agent")
}

func runCLIAndWait(t *testing.T, h *Harness, dir, branch string, args ...string) *ipc.RunInfo {
	t.Helper()
	out, err := h.RunInDir(dir, args...)
	if err != nil {
		t.Fatalf("%s: %v\n%s", strings.Join(args, " "), err, out)
	}
	if !strings.Contains(out, "Rerun started") {
		t.Fatalf("%s output missing rerun confirmation:\n%s", strings.Join(args, " "), out)
	}
	t.Logf("CLI %s: %s", strings.Join(args, " "), strings.TrimSpace(out))
	run := h.WaitForRun(branch, 90*time.Second)
	if run.Status != types.RunCompleted {
		t.Fatalf("%s status=%s error=%v", strings.Join(args, " "), run.Status, run.Error)
	}
	return run
}

func assertCompletedRerunFixture(t *testing.T, h *Harness, run *ipc.RunInfo, wantIntent, wantSource string) {
	t.Helper()
	if run.Status != types.RunCompleted {
		t.Fatalf("run %s status=%s, want completed", run.ID, run.Status)
	}
	intent := readRunIntent(t, h.NMHome, run.ID)
	if intent.summary == nil || *intent.summary != wantIntent {
		t.Fatalf("run %s intent=%v, want %q", run.ID, intent.summary, wantIntent)
	}
	if intent.source == nil || *intent.source != wantSource {
		t.Fatalf("run %s intent_source=%v, want %q", run.ID, intent.source, wantSource)
	}
	t.Logf("persisted run %s: intent_source=%q intent=%q", run.ID, *intent.source, *intent.summary)
}

func TestRerunIntentDisabledJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: writeIntentScenario(t), GlobalConfigExtra: "intent:\n  enabled: false"})
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	branch := "feature/disabled-intent"
	h.CommitChange(branch, "explicit.txt", "explicit\n", "explicit intent")
	author := h.AddWorktree(branch)
	h.PushToGate(branch)
	h.WaitForRun(branch, 90*time.Second)
	explicit := "all requirements\nincluding exclusions\nwithout condensation"
	first := runCLIAndWait(t, h, author, branch, "rerun", "--intent", explicit)
	assertCompletedRerunFixture(t, h, first, explicit, "agent")
	inherited := runCLIAndWait(t, h, author, branch, "rerun")
	assertCompletedRerunFixture(t, h, inherited, explicit, "rerun")
	if anyInvocationContains(h.AgentInvocations(), "transcript of a developer's recent conversation") {
		t.Fatal("disabled inference invoked summarizer")
	}
}
