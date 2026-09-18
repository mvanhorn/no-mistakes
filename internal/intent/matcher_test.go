package intent

import (
	"errors"
	"math"
	"testing"
	"time"
)

func TestScore_BasicOverlap(t *testing.T) {
	s := &Session{
		Messages: []Message{{
			Text:      "edited internal/foo.go",
			FilePaths: []string{"internal/bar.go"},
		}},
	}
	got, overlap := score(s, []string{"internal/foo.go", "internal/bar.go", "internal/baz.go"})
	if got <= 0 || got >= 1 {
		t.Errorf("score = %v, want strictly between 0 and 1", got)
	}
	if len(overlap) != 2 {
		t.Errorf("overlap = %v, want 2 files", overlap)
	}
}

func TestScore_BasenameOnlyMention(t *testing.T) {
	s := &Session{
		Messages: []Message{{
			Text: "look at foo.go for the change",
		}},
	}
	got, overlap := score(s, []string{"internal/sub/foo.go"})
	if got != 1.0 {
		t.Errorf("score = %v, want 1.0 (basename match)", got)
	}
	if len(overlap) != 1 {
		t.Errorf("expected overlap, got %v", overlap)
	}
}

func TestScore_NoMessages(t *testing.T) {
	s := &Session{}
	got, _ := score(s, []string{"foo.go"})
	if got != 0 {
		t.Errorf("empty session should score 0, got %v", got)
	}
}

func TestPickMatch_EqualScoresRefuseInsteadOfRecencyTieBreak(t *testing.T) {
	older := &Session{
		AgentName:    "claude",
		SessionID:    "older",
		LastActivity: time.Now().Add(-2 * time.Hour),
		Messages:     []Message{{FilePaths: []string{"foo.go"}}},
	}
	newer := &Session{
		AgentName:    "claude",
		SessionID:    "newer",
		LastActivity: time.Now(),
		Messages:     []Message{{FilePaths: []string{"foo.go"}}},
	}
	got, err := pickMatch([]*Session{older, newer}, []string{"foo.go"}, 0.1)
	if !errors.Is(err, ErrUnsafeMatch) {
		t.Fatalf("error = %v, want ErrUnsafeMatch", err)
	}
	if got != nil {
		t.Fatalf("equal 1.0 matches must refuse, got %+v", got)
	}
}

func TestPickMatch_BelowThreshold(t *testing.T) {
	s := &Session{
		LastActivity: time.Now(),
		Messages:     []Message{{FilePaths: []string{"foo.go"}}},
	}
	// 1 of 10 files matches → score 0.1, threshold 0.5 → no match.
	diff := []string{"foo.go", "a.go", "b.go", "c.go", "d.go", "e.go", "f.go", "g.go", "h.go", "i.go"}
	got, err := pickMatch([]*Session{s}, diff, 0.5)
	if !errors.Is(err, ErrNoMatch) {
		t.Fatalf("error = %v, want ErrNoMatch", err)
	}
	if got != nil {
		t.Errorf("expected no match below threshold, got %+v", got)
	}
}

func TestPickMatch_HigherScoreWins(t *testing.T) {
	low := &Session{
		LastActivity: time.Now(),
		Messages:     []Message{{FilePaths: []string{"foo.go"}}},
	}
	high := &Session{
		LastActivity: time.Now().Add(-time.Hour), // older - but should still win on score
		Messages:     []Message{{FilePaths: []string{"foo.go", "bar.go"}}},
	}
	got, err := pickMatch([]*Session{low, high}, []string{"foo.go", "bar.go"}, 0.1)
	if err != nil {
		t.Fatalf("pickMatch: %v", err)
	}
	if got == nil || got.Session != high {
		t.Errorf("expected higher-score session to win, got %+v", got)
	}
}

func TestPickMatch_RejectsSingleOverlapInMultiFileDiff(t *testing.T) {
	s := &Session{
		LastActivity: time.Now(),
		Messages:     []Message{{FilePaths: []string{"internal/cli/update_test.go"}}},
	}
	diff := []string{
		"internal/cli/update.go",
		"internal/cli/update_test.go",
		"internal/update/daemon.go",
		"internal/update/update.go",
		"internal/update/update_test.go",
	}

	got, err := pickMatch([]*Session{s}, diff, 0.2)
	if !errors.Is(err, ErrNoMatch) {
		t.Fatalf("error = %v, want ErrNoMatch", err)
	}
	if got != nil {
		t.Fatalf("expected no match for 1/5 overlap, got %+v", got)
	}
}

func TestPickMatch_AllowsSingleOverlapForSingleFileDiff(t *testing.T) {
	s := &Session{
		LastActivity: time.Now(),
		Messages:     []Message{{FilePaths: []string{"internal/update/update.go"}}},
	}

	got, err := pickMatch([]*Session{s}, []string{"internal/update/update.go"}, 0.2)
	if err != nil {
		t.Fatalf("pickMatch: %v", err)
	}
	if got == nil {
		t.Fatal("expected single-file match")
	}
}

func TestPickMatch_RejectsOldPartialMatch(t *testing.T) {
	headTime := time.Date(2026, 5, 15, 18, 53, 52, 0, time.UTC)
	old := &Session{
		LastActivity: headTime.Add(-48 * time.Hour),
		Messages: []Message{{FilePaths: []string{
			"internal/cli/update.go",
			"internal/cli/update_test.go",
			"internal/update/daemon.go",
		}}},
	}
	diff := []string{
		"internal/cli/update.go",
		"internal/cli/update_test.go",
		"internal/update/daemon.go",
		"internal/update/update.go",
		"internal/update/update_test.go",
	}

	got, err := pickMatchWithOptions([]*Session{old}, diff, matchOptions{Threshold: 0.2, HeadTime: headTime})
	if !errors.Is(err, ErrNoMatch) {
		t.Fatalf("error = %v, want ErrNoMatch", err)
	}
	if got != nil {
		t.Fatalf("expected old partial match to be rejected, got %+v", got)
	}
}

func numberedFiles(n int) []string {
	files := make([]string, n)
	for i := 0; i < n; i++ {
		files[i] = string(rune('a'+i%26)) + itoa(i) + ".go"
	}
	return files
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

func sessionWithFiles(id string, files []string, lastActivity time.Time) *Session {
	return &Session{
		AgentName:    "claude",
		SessionID:    id,
		LastActivity: lastActivity,
		Messages:     []Message{{FilePaths: files}},
	}
}

func TestPickMatch_WeakSoleCandidateRefusesDespiteRecencyAndLowThreshold(t *testing.T) {
	files := numberedFiles(15)
	headTime := time.Now()
	weak := sessionWithFiles("weak", files[:10], headTime)
	got, err := pickMatchWithOptions([]*Session{weak}, files, matchOptions{Threshold: 0.2, HeadTime: headTime})
	if !errors.Is(err, ErrUnsafeMatch) {
		t.Fatalf("error = %v, want ErrUnsafeMatch for 10/15 overlap", err)
	}
	if got != nil {
		t.Fatalf("weak sole candidate must not match, got %+v", got)
	}
}

func TestPickMatch_SoleFullMatchPasses(t *testing.T) {
	files := numberedFiles(15)
	s := sessionWithFiles("full", files, time.Now())
	got, err := pickMatch(sSlice(s), files, 0.2)
	if err != nil {
		t.Fatalf("pickMatch: %v", err)
	}
	if got == nil || got.Session != s {
		t.Fatalf("expected sole 1.0 match, got %+v", got)
	}
}

func TestPickMatch_StricterConfiguredFloorIsRespected(t *testing.T) {
	files := numberedFiles(20)
	s := sessionWithFiles("almost", files[:17], time.Now()) // 0.85
	got, err := pickMatch(sSlice(s), files, 0.9)
	if !errors.Is(err, ErrUnsafeMatch) {
		t.Fatalf("error = %v, want ErrUnsafeMatch against configured 0.9 floor", err)
	}
	if got != nil {
		t.Fatalf("0.85 must not pass a 0.9 configured floor, got %+v", got)
	}

	full := sessionWithFiles("full", files, time.Now())
	got, err = pickMatch(sSlice(full), files, 0.9)
	if err != nil {
		t.Fatalf("1.0 against 0.9 floor: %v", err)
	}
	if got == nil || got.Session != full {
		t.Fatalf("expected 1.0 to pass a 0.9 floor, got %+v", got)
	}
}

func TestPickMatch_AmbiguousCloseScoresRefuse(t *testing.T) {
	files := numberedFiles(100)
	headTime := time.Now()
	winner := sessionWithFiles("a", files[:85], headTime.Add(-time.Hour))
	runner := sessionWithFiles("b", files[:84], headTime)
	got, err := pickMatchWithOptions([]*Session{winner, runner}, files, matchOptions{Threshold: 0.2, HeadTime: headTime})
	if !errors.Is(err, ErrUnsafeMatch) {
		t.Fatalf("0.85 vs 0.84 error = %v, want ErrUnsafeMatch", err)
	}
	if got != nil {
		t.Fatalf("0.85 vs 0.84 must refuse, got %+v", got)
	}

	high := sessionWithFiles("high", files[:90], headTime)
	mid := sessionWithFiles("mid", files[:85], headTime.Add(-time.Minute))
	got, err = pickMatch([]*Session{high, mid}, files, 0.2)
	if !errors.Is(err, ErrUnsafeMatch) {
		t.Fatalf("0.90 vs 0.85 error = %v, want ErrUnsafeMatch", err)
	}
	if got != nil {
		t.Fatalf("0.90 vs 0.85 must refuse, got %+v", got)
	}
}

func TestPickMatch_ExactMarginBoundaryAndDecisiveWinner(t *testing.T) {
	files := numberedFiles(20)
	winner := sessionWithFiles("win", files[:19], time.Now().Add(-time.Hour)) // 0.95
	runner := sessionWithFiles("run", files[:17], time.Now())                 // 0.85
	got, err := pickMatch([]*Session{winner, runner}, files, 0.2)
	if err != nil {
		t.Fatalf("exact 0.10 margin should accept: %v", err)
	}
	if got == nil || got.Session != winner {
		t.Fatalf("expected winner at exact 0.10 margin, got %+v", got)
	}

	decisive := sessionWithFiles("decisive", files, time.Now().Add(-3*time.Hour))
	partial := sessionWithFiles("partial", files[:10], time.Now())
	got, err = pickMatch([]*Session{partial, decisive}, files, 0.2)
	if err != nil {
		t.Fatalf("decisive winner: %v", err)
	}
	if got == nil || got.Session != decisive {
		t.Fatalf("expected decisive winner, got %+v", got)
	}
}

func TestPickMatch_DuplicateSessionRecordsDoNotCreateATie(t *testing.T) {
	files := numberedFiles(4)
	first := sessionWithFiles("same", files, time.Now().Add(-time.Minute))
	dup := sessionWithFiles("same", files, time.Now())
	got, err := pickMatch([]*Session{first, dup}, files, 0.2)
	if err != nil {
		t.Fatalf("duplicate session records: %v", err)
	}
	if got == nil || got.Session.SessionID != "same" {
		t.Fatalf("expected deduped session to match, got %+v", got)
	}
}

func TestPickMatch_NonFiniteThresholdCannotBypassFloor(t *testing.T) {
	files := numberedFiles(15)
	weak := sessionWithFiles("weak", files[:10], time.Now())
	for _, threshold := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), -1} {
		got, err := pickMatch([]*Session{weak}, files, threshold)
		if !errors.Is(err, ErrUnsafeMatch) {
			t.Fatalf("threshold %v: error = %v, want ErrUnsafeMatch", threshold, err)
		}
		if got != nil {
			t.Fatalf("threshold %v must not bypass the floor, got %+v", threshold, got)
		}
	}
}

func sSlice(s *Session) []*Session { return []*Session{s} }

func TestNormalizedPathVariants(t *testing.T) {
	got := normalizedPathVariants("./internal/foo.go")
	want := map[string]bool{"internal/foo.go": true, "foo.go": true}
	for _, v := range got {
		if !want[v] {
			t.Errorf("unexpected variant %q", v)
		}
		delete(want, v)
	}
	if len(want) > 0 {
		t.Errorf("missing variants: %v", want)
	}
}
