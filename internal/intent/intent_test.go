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
	loaded   []string
}

func (s *staticReader) Name() string { return s.name }
func (s *staticReader) Discover(_ context.Context, opts DiscoverOpts) ([]*Session, error) {
	s.opts = opts
	return s.sessions, nil
}
func (s *staticReader) Load(_ context.Context, session *Session) error {
	s.loaded = append(s.loaded, session.SessionID)
	return nil
}

type fixedSummarizer struct {
	summary string
	calls   int
}

func (f *fixedSummarizer) Summarize(_ context.Context, _ *Session) (string, error) {
	f.calls++
	return f.summary, nil
}

func TestExtract_HappyPath(t *testing.T) {
	repo := newScopeRepo(t)

	r := &staticReader{
		name: "claude",
		sessions: []*Session{{
			SessionID:    "s1",
			CWD:          repo,
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
		OriginCWD:  repo,
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

func TestExtract_UnsafeBelowThreshold(t *testing.T) {
	repo := newScopeRepo(t)

	r := &staticReader{
		name: "claude",
		sessions: []*Session{{
			SessionID:    "s1",
			CWD:          repo,
			LastActivity: time.Now(),
			Messages:     []Message{{Role: RoleUser, Text: "hello"}},
		}},
	}
	_, err := Extract(context.Background(), ExtractParams{
		OriginCWD:  repo,
		DiffFiles:  []string{"foo.go"},
		HeadTime:   time.Now(),
		BaseTime:   time.Now().Add(-time.Hour),
		Threshold:  0.5,
		Readers:    []Reader{r},
		Summarizer: &fixedSummarizer{summary: "x"},
	})
	if !errors.Is(err, ErrUnsafeMatch) {
		t.Errorf("expected ErrUnsafeMatch, got %v", err)
	}
}

func TestExtract_PassesUnextendedHeadTimeToReaders(t *testing.T) {
	repo := newScopeRepo(t)

	baseTime := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	headTime := baseTime.Add(2 * time.Hour)
	r := &staticReader{
		name: "claude",
		sessions: []*Session{{
			SessionID:    "s1",
			CWD:          repo,
			LastActivity: headTime,
			Messages:     []Message{{Role: RoleUser, Text: "edit foo.go", FilePaths: []string{"foo.go"}}},
		}},
	}

	_, err := Extract(context.Background(), ExtractParams{
		OriginCWD:  repo,
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
	repo := newScopeRepo(t)

	sess := &Session{
		SessionID:    "s1",
		CWD:          repo,
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
		OriginCWD:  repo,
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

func TestExtract_NoReaders(t *testing.T) {
	repo := newScopeRepo(t)

	_, err := Extract(context.Background(), ExtractParams{
		OriginCWD:  repo,
		DiffFiles:  []string{"foo.go"},
		Summarizer: &fixedSummarizer{},
	})
	if !errors.Is(err, ErrNoMatch) {
		t.Errorf("expected ErrNoMatch with no readers, got %v", err)
	}
}

func TestExtract_LogsAcceptedAndRejectedCandidates(t *testing.T) {
	repo := newScopeRepo(t)

	r := &staticReader{
		name: "opencode",
		sessions: []*Session{{
			SessionID:    "weak",
			CWD:          repo,
			LastActivity: time.Now(),
			Messages:     []Message{{FilePaths: []string{"a.go"}}},
		}, {
			SessionID:    "strong",
			CWD:          repo,
			LastActivity: time.Now(),
			Messages:     []Message{{FilePaths: []string{"a.go", "b.go", "c.go"}}},
		}},
	}
	var logs []string
	_, err := Extract(context.Background(), ExtractParams{
		OriginCWD:  repo,
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
	joined := strings.Join(logs, "\n")
	for _, want := range []string{"candidate", "opencode", "strong", "accepted"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("logs missing %q:\n%s", want, joined)
		}
	}
	for _, want := range []string{"weak", "single_overlap_multi_file_diff"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("logs missing %q: %s", want, joined)
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

func TestExtract_RefusesBeforeCacheAndSummarizer(t *testing.T) {
	for _, kind := range []string{"scope", "missing-cwd", "floor", "margin", "different-agent"} {
		t.Run(kind, func(t *testing.T) {
			repo := newScopeRepo(t)
			files := []string{"a.go", "b.go", "c.go", "d.go", "e.go", "f.go", "g.go", "h.go", "i.go", "j.go"}
			s := &Session{AgentName: "claude", SessionID: "cached", CWD: repo, LastActivity: time.Now(), LastMsgKey: "key", Messages: []Message{{FilePaths: files}}}
			sessions := []*Session{s}
			readers := []Reader{&staticReader{name: "claude", sessions: sessions}}
			switch kind {
			case "scope":
				s.CWD = newScopeRepo(t)
			case "missing-cwd":
				s.CWD = ""
			case "floor":
				s.Messages[0].FilePaths = files[:6]
			case "margin":
				other := *s
				other.SessionID = "other"
				readers[0] = &staticReader{name: "claude", sessions: append(sessions, &other)}
			case "different-agent":
				other := *s
				readers = append(readers, &staticReader{name: "codex", sessions: []*Session{&other}})
			}
			cache := NewMemCache()
			cache.Put(cacheKeyFor(s), "must not return cached text", "claude", s.SessionID)
			sum := &fixedSummarizer{summary: "must not summarize"}
			result, err := Extract(context.Background(), ExtractParams{OriginCWD: repo, DiffFiles: files, HeadTime: time.Now(), Readers: readers, Cache: cache, Summarizer: sum})
			if !errors.Is(err, ErrUnsafeMatch) || result != nil || sum.calls != 0 {
				t.Fatalf("result=%+v err=%v summarize calls=%d", result, err, sum.calls)
			}
		})
	}
}

func TestExtract_ScopedWinnerIgnoresRejectedMetadata(t *testing.T) {
	repo := newScopeRepo(t)
	safe := &Session{SessionID: "safe", CWD: repo, Messages: []Message{{FilePaths: []string{"foo.go"}}}}
	sibling := &Session{SessionID: "sibling", CWD: newScopeRepo(t), Messages: safe.Messages}
	r := &staticReader{name: "future-reader", sessions: []*Session{sibling, safe}}
	sum := &fixedSummarizer{summary: "safe"}
	got, err := Extract(context.Background(), ExtractParams{OriginCWD: repo, DiffFiles: []string{"foo.go"}, Readers: []Reader{r}, Summarizer: sum})
	if len(r.loaded) != 1 || r.loaded[0] != "safe" {
		t.Fatalf("loaded rejected metadata: %v", r.loaded)
	}
	if err != nil || got.SessionID != "safe" {
		t.Fatalf("got %+v, %v", got, err)
	}
}
