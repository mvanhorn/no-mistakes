package intent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// staticReader is a minimal Reader for tests.
type staticReader struct {
	name     string
	sessions []*Session
	opts     DiscoverOpts
	keepCWD  bool
}

func (s *staticReader) Name() string { return s.name }
func (s *staticReader) Discover(_ context.Context, opts DiscoverOpts) ([]*Session, error) {
	s.opts = opts
	if s.keepCWD {
		return s.sessions, nil
	}
	out := make([]*Session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		copySess := *sess
		if copySess.CWD == "" {
			copySess.CWD = opts.OriginCWD
		}
		out = append(out, &copySess)
	}
	return out, nil
}
func (s *staticReader) Load(_ context.Context, _ *Session) error { return nil }

type fixedSummarizer struct {
	summary string
	calls   int
}

func (f *fixedSummarizer) Summarize(_ context.Context, _ *Session) (string, error) {
	f.calls++
	return f.summary, nil
}

type fixedDisambiguator struct {
	selectedAgentName string
	selectedSessionID string
	calls             int
	candidates        []*Match
	err               error
}

func (f *fixedDisambiguator) Disambiguate(_ context.Context, _ []string, candidates []*Match) (DisambiguationChoice, error) {
	f.calls++
	f.candidates = candidates
	if f.err != nil {
		return DisambiguationChoice{}, f.err
	}
	return DisambiguationChoice{AgentName: f.selectedAgentName, SessionID: f.selectedSessionID}, nil
}

func TestExtract_HappyPath(t *testing.T) {
	r := &staticReader{
		name: "claude",
		sessions: []*Session{{
			SessionID:    "s1",
			LastActivity: time.Now(),
			LastMsgKey:   "k1",
			Messages: []Message{
				{Role: RoleUser, Text: "edit foo.go"},
				{Role: RoleAssistant, Text: "done", FilePaths: []string{"foo.go"}},
			},
		}},
	}
	sum := &fixedSummarizer{summary: "user edited foo"}
	got, err := Extract(context.Background(), ExtractParams{
		OriginCWD:  "/tmp/repo",
		DiffFiles:  []string{"foo.go"},
		BaseTime:   time.Now().Add(-time.Hour),
		HeadTime:   time.Now(),
		SlackDays:  3,
		Threshold:  0.2,
		Readers:    []Reader{r},
		Cache:      NewMemCache(),
		Summarizer: sum,
	})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if got.Summary != "user edited foo" {
		t.Errorf("summary = %q", got.Summary)
	}
	if got.AgentName != "claude" {
		t.Errorf("agent = %q", got.AgentName)
	}
}

func TestExtract_NoMatchBelowThreshold(t *testing.T) {
	r := &staticReader{
		name: "claude",
		sessions: []*Session{{
			SessionID:    "s1",
			LastActivity: time.Now(),
			Messages:     []Message{{Role: RoleUser, Text: "hello"}},
		}},
	}
	_, err := Extract(context.Background(), ExtractParams{
		OriginCWD:  "/tmp/repo",
		DiffFiles:  []string{"foo.go"},
		HeadTime:   time.Now(),
		BaseTime:   time.Now().Add(-time.Hour),
		Threshold:  0.5,
		Readers:    []Reader{r},
		Summarizer: &fixedSummarizer{summary: "x"},
	})
	if !errors.Is(err, ErrNoMatch) {
		t.Errorf("expected ErrNoMatch, got %v", err)
	}
}

func TestExtract_PassesUnextendedHeadTimeToReaders(t *testing.T) {
	baseTime := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	headTime := baseTime.Add(2 * time.Hour)
	r := &staticReader{
		name: "claude",
		sessions: []*Session{{
			SessionID:    "s1",
			LastActivity: headTime,
			Messages:     []Message{{Role: RoleUser, Text: "edit foo.go", FilePaths: []string{"foo.go"}}},
		}},
	}

	_, err := Extract(context.Background(), ExtractParams{
		OriginCWD:  "/tmp/repo",
		DiffFiles:  []string{"foo.go"},
		BaseTime:   baseTime,
		HeadTime:   headTime,
		SlackDays:  3,
		Threshold:  0.1,
		Readers:    []Reader{r},
		Summarizer: &fixedSummarizer{summary: "edited foo"},
	})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if !r.opts.WindowEnd.Equal(headTime) {
		t.Fatalf("WindowEnd = %v, want %v", r.opts.WindowEnd, headTime)
	}
}

func TestExtract_CacheHitSkipsSummarizer(t *testing.T) {
	sess := &Session{
		SessionID:    "s1",
		LastActivity: time.Now(),
		LastMsgKey:   "k1",
		Messages:     []Message{{Role: RoleUser, Text: "x", FilePaths: []string{"foo.go"}}},
	}
	r := &staticReader{name: "claude", sessions: []*Session{sess}}
	sum := &fixedSummarizer{summary: "fresh"}
	cache := NewMemCache()
	// Pre-populate cache with the key the extractor will compute. Note we need
	// to set AgentName first because Discover does it inside Extract; mimic.
	sess.AgentName = "claude"
	cache.Put(cacheKeyFor(sess), "cached", "claude", "s1")

	got, err := Extract(context.Background(), ExtractParams{
		OriginCWD:  "/tmp/repo",
		DiffFiles:  []string{"foo.go"},
		HeadTime:   time.Now(),
		BaseTime:   time.Now().Add(-time.Hour),
		Threshold:  0.1,
		Readers:    []Reader{r},
		Cache:      cache,
		Summarizer: sum,
	})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if got.Summary != "cached" {
		t.Errorf("expected cache hit summary, got %q", got.Summary)
	}
	if sum.calls != 0 {
		t.Errorf("summarizer should not have been called, got %d calls", sum.calls)
	}
}

func TestExtract_RefusesWhenMultipleAcceptedCandidatesAreNotDecisive(t *testing.T) {
	first := &Session{
		SessionID:    "s1",
		LastActivity: time.Now(),
		LastMsgKey:   "k1",
		Messages:     []Message{{Role: RoleUser, Text: "work on foo and bar", FilePaths: []string{"foo.go", "bar.go"}}},
	}
	second := &Session{
		SessionID:    "s2",
		LastActivity: time.Now().Add(-time.Minute),
		LastMsgKey:   "k2",
		Messages:     []Message{{Role: RoleUser, Text: "work on bar and baz", FilePaths: []string{"bar.go", "baz.go"}}},
	}
	r := &staticReader{name: "claude", sessions: []*Session{first, second}}
	d := &fixedDisambiguator{selectedAgentName: "claude", selectedSessionID: "s2"}
	sum := &fixedSummarizer{summary: "selected second"}

	got, err := Extract(context.Background(), ExtractParams{
		OriginCWD:     "/tmp/repo",
		DiffFiles:     []string{"foo.go", "bar.go", "baz.go", "qux.go"},
		HeadTime:      time.Now(),
		BaseTime:      time.Now().Add(-time.Hour),
		Threshold:     0.2,
		Readers:       []Reader{r},
		Summarizer:    sum,
		Disambiguator: d,
	})
	if !errors.Is(err, ErrUnsafeMatch) {
		t.Fatalf("error = %v, want ErrUnsafeMatch", err)
	}
	if got != nil {
		t.Fatalf("unsafe match must not attach a summary, got %+v", got)
	}
	if d.calls != 0 {
		t.Fatalf("disambiguator calls = %d, want 0", d.calls)
	}
	if sum.calls != 0 {
		t.Fatalf("summarizer calls = %d, want 0", sum.calls)
	}
}

func TestExtract_DoesNotDisambiguateSingleDecisiveCandidate(t *testing.T) {
	decisive := &Session{
		SessionID:    "decisive",
		LastActivity: time.Now().Add(-time.Minute),
		Messages:     []Message{{Role: RoleUser, FilePaths: []string{"foo.go", "bar.go", "baz.go", "qux.go"}}},
	}
	partial := &Session{
		SessionID:    "partial",
		LastActivity: time.Now(),
		Messages:     []Message{{Role: RoleUser, FilePaths: []string{"foo.go", "bar.go"}}},
	}
	r := &staticReader{name: "claude", sessions: []*Session{partial, decisive}}
	d := &fixedDisambiguator{selectedAgentName: "claude", selectedSessionID: "partial"}

	got, err := Extract(context.Background(), ExtractParams{
		OriginCWD:     "/tmp/repo",
		DiffFiles:     []string{"foo.go", "bar.go", "baz.go", "qux.go"},
		HeadTime:      time.Now(),
		BaseTime:      time.Now().Add(-time.Hour),
		Threshold:     0.2,
		Readers:       []Reader{r},
		Summarizer:    &fixedSummarizer{summary: "selected decisive"},
		Disambiguator: d,
	})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if d.calls != 0 {
		t.Fatalf("disambiguator calls = %d, want 0", d.calls)
	}
	if got.SessionID != "decisive" {
		t.Fatalf("selected session = %q, want decisive", got.SessionID)
	}
}

func TestExtract_RefusesWhenMultipleCandidatesAreDecisive(t *testing.T) {
	first := &Session{
		SessionID:    "s1",
		LastActivity: time.Now(),
		Messages:     []Message{{Role: RoleUser, FilePaths: []string{"foo.go", "bar.go", "baz.go", "qux.go"}}},
	}
	second := &Session{
		SessionID:    "s2",
		LastActivity: time.Now().Add(-time.Minute),
		Messages:     []Message{{Role: RoleUser, FilePaths: []string{"foo.go", "bar.go", "baz.go", "qux.go"}}},
	}
	r := &staticReader{name: "claude", sessions: []*Session{first, second}}
	d := &fixedDisambiguator{selectedAgentName: "claude", selectedSessionID: "s2"}
	sum := &fixedSummarizer{summary: "selected second"}

	got, err := Extract(context.Background(), ExtractParams{
		OriginCWD:     "/tmp/repo",
		DiffFiles:     []string{"foo.go", "bar.go", "baz.go", "qux.go"},
		HeadTime:      time.Now(),
		BaseTime:      time.Now().Add(-time.Hour),
		Threshold:     0.2,
		Readers:       []Reader{r},
		Summarizer:    sum,
		Disambiguator: d,
	})
	if !errors.Is(err, ErrUnsafeMatch) {
		t.Fatalf("error = %v, want ErrUnsafeMatch", err)
	}
	if got != nil {
		t.Fatalf("equal 1.0 matches must not attach a summary, got %+v", got)
	}
	if d.calls != 0 {
		t.Fatalf("disambiguator calls = %d, want 0", d.calls)
	}
	if sum.calls != 0 {
		t.Fatalf("summarizer calls = %d, want 0", sum.calls)
	}
}

func TestExtract_DisambiguatorChoiceCannotRescueAmbiguousSet(t *testing.T) {
	headTime := time.Now()
	claude := &staticReader{name: "claude", sessions: []*Session{{
		SessionID:    "same",
		LastActivity: headTime.Add(-time.Minute),
		Messages:     []Message{{Role: RoleUser, FilePaths: []string{"foo.go", "bar.go"}}},
	}}}
	opencode := &staticReader{name: "opencode", sessions: []*Session{{
		SessionID:    "same",
		LastActivity: headTime,
		Messages:     []Message{{Role: RoleUser, FilePaths: []string{"foo.go", "bar.go"}}},
	}}}
	d := &fixedDisambiguator{selectedAgentName: "opencode", selectedSessionID: "same"}
	sum := &fixedSummarizer{summary: "selected opencode"}

	got, err := Extract(context.Background(), ExtractParams{
		OriginCWD:     "/tmp/repo",
		DiffFiles:     []string{"foo.go", "bar.go"},
		HeadTime:      headTime,
		BaseTime:      headTime.Add(-time.Hour),
		Threshold:     0.2,
		Readers:       []Reader{claude, opencode},
		Summarizer:    sum,
		Disambiguator: d,
	})
	if !errors.Is(err, ErrUnsafeMatch) {
		t.Fatalf("error = %v, want ErrUnsafeMatch", err)
	}
	if got != nil {
		t.Fatalf("disambiguator choice must not attach a summary, got %+v", got)
	}
	if d.calls != 0 {
		t.Fatalf("disambiguator calls = %d, want 0", d.calls)
	}
	if sum.calls != 0 {
		t.Fatalf("summarizer calls = %d, want 0", sum.calls)
	}
}

func TestExtract_DisambiguatorFailureDoesNotRescueAmbiguousSet(t *testing.T) {
	headTime := time.Now()
	r := &staticReader{name: "claude", sessions: []*Session{
		{
			SessionID:    "s1",
			LastActivity: headTime,
			Messages:     []Message{{Role: RoleUser, FilePaths: []string{"foo.go", "bar.go"}}},
		},
		{
			SessionID:    "s2",
			LastActivity: headTime.Add(-time.Minute),
			Messages:     []Message{{Role: RoleUser, FilePaths: []string{"foo.go", "bar.go"}}},
		},
	}}
	d := &fixedDisambiguator{err: ErrDisambiguatorCleanup}
	sum := &fixedSummarizer{summary: "fallback"}

	got, err := Extract(context.Background(), ExtractParams{
		OriginCWD:     "/tmp/repo",
		DiffFiles:     []string{"foo.go", "bar.go"},
		HeadTime:      headTime,
		BaseTime:      headTime.Add(-time.Hour),
		Threshold:     0.2,
		Readers:       []Reader{r},
		Summarizer:    sum,
		Disambiguator: d,
	})
	if !errors.Is(err, ErrUnsafeMatch) {
		t.Fatalf("error = %v, want ErrUnsafeMatch (not disambiguator fallback)", err)
	}
	if got != nil {
		t.Fatalf("disambiguator failure must not yield a fallback winner, got %+v", got)
	}
	if d.calls != 0 {
		t.Fatalf("disambiguator calls = %d, want 0", d.calls)
	}
	if sum.calls != 0 {
		t.Fatalf("summarizer calls = %d, want 0", sum.calls)
	}
}

func TestExtract_NearTieWithRecencyBoostStillRefuses(t *testing.T) {
	headTime := time.Now()
	diffFiles := []string{
		"a.go", "b.go", "c.go", "d.go", "e.go",
		"f.go", "g.go", "h.go", "i.go", "j.go",
		"k.go", "l.go", "m.go", "n.go", "o.go",
		"p.go", "q.go", "r.go", "s.go", "t.go",
	}
	decisive := &Session{
		SessionID:    "decisive",
		LastActivity: headTime.Add(-3 * time.Hour),
		Messages:     []Message{{Role: RoleUser, FilePaths: diffFiles[:17]}},
	}
	recentPartial := &Session{
		SessionID:    "recent-partial",
		LastActivity: headTime,
		Messages:     []Message{{Role: RoleUser, FilePaths: diffFiles[:16]}},
	}
	r := &staticReader{name: "claude", sessions: []*Session{recentPartial, decisive}}
	d := &fixedDisambiguator{selectedAgentName: "claude", selectedSessionID: "recent-partial"}
	sum := &fixedSummarizer{summary: "selected decisive"}

	got, err := Extract(context.Background(), ExtractParams{
		OriginCWD:     "/tmp/repo",
		DiffFiles:     diffFiles,
		HeadTime:      headTime,
		BaseTime:      headTime.Add(-time.Hour),
		Threshold:     0.2,
		Readers:       []Reader{r},
		Summarizer:    sum,
		Disambiguator: d,
	})
	if !errors.Is(err, ErrUnsafeMatch) {
		t.Fatalf("error = %v, want ErrUnsafeMatch for 0.85 vs 0.80", err)
	}
	if got != nil {
		t.Fatalf("recency must not break a near tie, got %+v", got)
	}
	if d.calls != 0 {
		t.Fatalf("disambiguator calls = %d, want 0", d.calls)
	}
}

func TestExtract_NoReaders(t *testing.T) {
	_, err := Extract(context.Background(), ExtractParams{
		OriginCWD:  "/tmp/repo",
		DiffFiles:  []string{"foo.go"},
		Summarizer: &fixedSummarizer{},
	})
	if !errors.Is(err, ErrNoMatch) {
		t.Errorf("expected ErrNoMatch with no readers, got %v", err)
	}
}

func TestExtract_LogsRejectedCandidatesWithReasons(t *testing.T) {
	r := &staticReader{
		name: "opencode",
		sessions: []*Session{{
			SessionID:    "weak",
			CWD:          "/tmp/repo",
			LastActivity: time.Now(),
			Messages:     []Message{{FilePaths: []string{"a.go"}}},
		}, {
			SessionID:    "strong",
			CWD:          "/tmp/repo",
			LastActivity: time.Now(),
			Messages:     []Message{{FilePaths: []string{"a.go", "b.go", "c.go"}}},
		}},
	}
	var logs []string
	got, err := Extract(context.Background(), ExtractParams{
		OriginCWD:  "/tmp/repo",
		DiffFiles:  []string{"a.go", "b.go", "c.go"},
		HeadTime:   time.Now(),
		BaseTime:   time.Now().Add(-time.Hour),
		Threshold:  0.2,
		Readers:    []Reader{r},
		Summarizer: &fixedSummarizer{summary: "x"},
		Logf: func(format string, args ...any) {
			logs = append(logs, fmt.Sprintf(format, args...))
		},
	})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if got.SessionID != "strong" {
		t.Fatalf("selected session = %q, want strong", got.SessionID)
	}
	joined := strings.Join(logs, "\n")
	for _, want := range []string{"candidate", "opencode", "strong", "accepted", "weak", "rejected", "single_overlap_multi_file_diff"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("logs missing %q:\n%s", want, joined)
		}
	}
}

func TestExtract_RequiresOriginCWD(t *testing.T) {
	_, err := Extract(context.Background(), ExtractParams{
		DiffFiles:  []string{"foo.go"},
		Summarizer: &fixedSummarizer{},
	})
	if err == nil {
		t.Error("expected error when OriginCWD missing")
	}
}

func TestExtract_FiltersUnscopedSessionsCentrally(t *testing.T) {
	origin := "/tmp/repo"
	sibling := &Session{
		SessionID:    "sibling",
		CWD:          "/tmp/other-worktree",
		LastActivity: time.Now(),
		Messages:     []Message{{FilePaths: []string{"foo.go"}}},
	}
	clone := &Session{
		SessionID:    "clone",
		CWD:          "/tmp/same-remote-clone",
		LastActivity: time.Now(),
		Messages:     []Message{{FilePaths: []string{"foo.go"}}},
	}
	empty := &Session{
		SessionID:    "empty",
		CWD:          "",
		LastActivity: time.Now(),
		Messages:     []Message{{FilePaths: []string{"foo.go"}}},
	}
	scoped := &Session{
		SessionID:    "scoped",
		CWD:          origin,
		LastActivity: time.Now().Add(-time.Minute),
		Messages:     []Message{{FilePaths: []string{"foo.go"}}},
	}
	r := &staticReader{name: "claude", sessions: []*Session{sibling, clone, empty, scoped}, keepCWD: true}
	sum := &fixedSummarizer{summary: "scoped only"}
	var logs []string

	got, err := Extract(context.Background(), ExtractParams{
		OriginCWD:  origin,
		DiffFiles:  []string{"foo.go"},
		HeadTime:   time.Now(),
		BaseTime:   time.Now().Add(-time.Hour),
		Threshold:  0.2,
		Readers:    []Reader{r},
		Summarizer: sum,
		Logf: func(format string, args ...any) {
			logs = append(logs, fmt.Sprintf(format, args...))
		},
	})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if got.SessionID != "scoped" {
		t.Fatalf("selected = %q, want scoped", got.SessionID)
	}
	joined := strings.Join(logs, "\n")
	for _, want := range []string{"sibling", "clone", "empty", "unscoped"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("logs missing unscoped rejection %q:\n%s", want, joined)
		}
	}
}

func TestExtract_DiscoveredUnscopedOnlyIsUnsafeNotNoMatch(t *testing.T) {
	r := &staticReader{
		name: "claude",
		sessions: []*Session{{
			SessionID:    "other",
			CWD:          "/tmp/other",
			LastActivity: time.Now(),
			Messages:     []Message{{FilePaths: []string{"foo.go"}}},
		}},
		keepCWD: true,
	}
	sum := &fixedSummarizer{summary: "should not run"}
	got, err := Extract(context.Background(), ExtractParams{
		OriginCWD:  "/tmp/repo",
		DiffFiles:  []string{"foo.go"},
		HeadTime:   time.Now(),
		BaseTime:   time.Now().Add(-time.Hour),
		Threshold:  0.2,
		Readers:    []Reader{r},
		Summarizer: sum,
	})
	if !errors.Is(err, ErrUnsafeMatch) {
		t.Fatalf("error = %v, want ErrUnsafeMatch for discovered-but-unscoped", err)
	}
	if got != nil {
		t.Fatalf("unscoped-only must not attach a summary, got %+v", got)
	}
	if sum.calls != 0 {
		t.Fatalf("summarizer calls = %d, want 0", sum.calls)
	}
	if !strings.Contains(err.Error(), "none belong to the source checkout") {
		t.Fatalf("error should distinguish unscoped from absent, got %v", err)
	}
}

func TestExtract_CachedSummaryCannotBypassUnsafeMatch(t *testing.T) {
	sess := &Session{
		SessionID:    "weak",
		LastActivity: time.Now(),
		LastMsgKey:   "k1",
		Messages:     []Message{{Role: RoleUser, FilePaths: numberedFiles(15)[:10]}},
	}
	r := &staticReader{name: "claude", sessions: []*Session{sess}}
	sum := &fixedSummarizer{summary: "fresh"}
	cache := NewMemCache()
	sess.AgentName = "claude"
	cache.Put(cacheKeyFor(sess), "cached-unsafe", "claude", "weak")

	got, err := Extract(context.Background(), ExtractParams{
		OriginCWD:  "/tmp/repo",
		DiffFiles:  numberedFiles(15),
		HeadTime:   time.Now(),
		BaseTime:   time.Now().Add(-time.Hour),
		Threshold:  0.2,
		Readers:    []Reader{r},
		Cache:      cache,
		Summarizer: sum,
	})
	if !errors.Is(err, ErrUnsafeMatch) {
		t.Fatalf("error = %v, want ErrUnsafeMatch", err)
	}
	if got != nil {
		t.Fatalf("cached summary must not bypass acceptance, got %+v", got)
	}
	if sum.calls != 0 {
		t.Fatalf("summarizer calls = %d, want 0", sum.calls)
	}
}

func TestExtract_RecheckFailureDoesNotAttachSummary(t *testing.T) {
	r := &staticReader{name: "claude", sessions: []*Session{{
		SessionID:    "s1",
		LastActivity: time.Now(),
		LastMsgKey:   "k1",
		Messages:     []Message{{Role: RoleUser, FilePaths: []string{"foo.go"}}},
	}}}
	sum := &fixedSummarizer{summary: "should not run"}
	cache := NewMemCache()
	got, err := Extract(context.Background(), ExtractParams{
		OriginCWD:  "/tmp/repo",
		DiffFiles:  []string{"foo.go"},
		HeadTime:   time.Now(),
		BaseTime:   time.Now().Add(-time.Hour),
		Threshold:  0.2,
		Readers:    []Reader{r},
		Cache:      cache,
		Summarizer: sum,
		Recheck: func() error {
			return unsafeMatchError("branch switched during extraction")
		},
	})
	if !errors.Is(err, ErrUnsafeMatch) {
		t.Fatalf("error = %v, want ErrUnsafeMatch", err)
	}
	if got != nil {
		t.Fatalf("recheck failure must not attach a summary, got %+v", got)
	}
	if sum.calls != 0 {
		t.Fatalf("summarizer calls = %d, want 0", sum.calls)
	}
}

func TestExtract_ZeroOverlapIsNoMatch(t *testing.T) {
	r := &staticReader{name: "claude", sessions: []*Session{{
		SessionID:    "s1",
		LastActivity: time.Now(),
		Messages:     []Message{{Role: RoleUser, Text: "hello"}},
	}}}
	_, err := Extract(context.Background(), ExtractParams{
		OriginCWD:  "/tmp/repo",
		DiffFiles:  []string{"foo.go", "bar.go"},
		HeadTime:   time.Now(),
		BaseTime:   time.Now().Add(-time.Hour),
		Threshold:  0.2,
		Readers:    []Reader{r},
		Summarizer: &fixedSummarizer{summary: "x"},
	})
	if !errors.Is(err, ErrNoMatch) {
		t.Fatalf("error = %v, want ErrNoMatch for zero overlap", err)
	}
}
