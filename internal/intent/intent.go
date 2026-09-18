package intent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// Result is what Extract returns when it successfully attaches an intent
// to a run. AgentName/SessionID/Score are surfaced for telemetry and DB
// persistence; callers store the Summary onto db.Run.Intent.
type Result struct {
	Summary   string
	AgentName string
	SessionID string
	Score     float64
}

// ExtractParams configures a single Extract call.
type ExtractParams struct {
	// HomeDir overrides the user's home directory. Empty means use os.UserHomeDir.
	HomeDir string
	// OriginCWD is the user's actual repo directory. The caller is responsible
	// for passing the original working path, NOT the no-mistakes worktree.
	OriginCWD string
	// DiffFiles is the repo-relative file set used for matching and scoring.
	DiffFiles []string
	// BaseTime is the committer time of the base SHA.
	BaseTime time.Time
	// HeadTime is the committer time of the head SHA.
	HeadTime time.Time
	// SlackDays extends WindowStart backwards. The plan called for 3 days.
	SlackDays int
	// Threshold is an additional lower bound on raw file-overlap. Effective
	// acceptance is max(decisiveMatchScore, Threshold); a low configured
	// value cannot re-enable a weak match. Non-finite values are ignored.
	Threshold float64
	// Readers are the per-agent transcript readers to consult. Order is
	// insignificant. Readers may search broadly; Extract still requires every
	// selected session to resolve to OriginCWD's canonical checkout.
	Readers []Reader
	// Cache is consulted before summarization. Pass NewMemCache() if no DB.
	Cache Cache
	// Summarizer turns the chosen session's text into a short summary.
	Summarizer Summarizer
	// Disambiguator is retained so the independent disambiguator implementation
	// stays testable. Automatic selection no longer consults it: an LLM choice
	// cannot overrule a failed margin, and a disambiguator error cannot yield
	// a fallback winner.
	Disambiguator Disambiguator
	// Recheck, if set, is invoked after a match is accepted and before a
	// summary is attached from cache or the summarizer. Used to prove the
	// source checkout still matches the run's branch and submitted head.
	Recheck func() error
	// Logf receives candidate diagnostics, including rejected sessions. Nil disables logging.
	Logf func(format string, args ...any)
}

// ErrNoMatch indicates no agent transcript matched the change. Callers
// should treat this as a normal "no intent attached" outcome, not an error.
var ErrNoMatch = errors.New("intent: no matching transcript")

// ErrUnsafeMatch indicates a positive but unusable candidate set: a weak
// sole overlap, an ambiguous pair of overlapping sessions, transcripts that
// belong to another checkout, or an unverifiable source checkout. Callers
// must not attach a summary and should surface an ask-user finding.
var ErrUnsafeMatch = errors.New("intent: unsafe transcript match")

func unsafeMatchError(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrUnsafeMatch, fmt.Sprintf(format, args...))
}

// Extract runs the discover -> scope -> match -> cache -> summarize pipeline
// and returns the final intent. It returns ErrNoMatch when there is a genuine
// absence of relevant usable transcript evidence, including zero overlap. It
// returns ErrUnsafeMatch when discovered transcripts are unscoped, weak, or
// ambiguous, or when Recheck fails. Automatic selection never consults
// Disambiguator.
func Extract(ctx context.Context, p ExtractParams) (*Result, error) {
	if p.OriginCWD == "" {
		return nil, fmt.Errorf("intent: OriginCWD is required")
	}
	if len(p.DiffFiles) == 0 {
		return nil, ErrNoMatch
	}
	if p.Cache == nil {
		p.Cache = NewMemCache()
	}
	if p.Summarizer == nil {
		return nil, fmt.Errorf("intent: Summarizer is required")
	}

	originTop := canonicalSourceCheckoutPath(ctx, p.OriginCWD)

	slack := time.Duration(maxInt(p.SlackDays, 0)) * 24 * time.Hour
	opts := DiscoverOpts{
		HomeDir:     p.HomeDir,
		OriginCWD:   originTop,
		WindowStart: p.BaseTime.Add(-slack),
		WindowEnd:   p.HeadTime,
	}

	var sessions []*Session
	for _, r := range p.Readers {
		if r == nil {
			continue
		}
		discovered, err := r.Discover(ctx, opts)
		if err != nil {
			slog.Debug("intent reader discover failed", "agent", r.Name(), "error", err)
			continue
		}
		for _, s := range discovered {
			s.AgentName = r.Name()
		}
		sessions = append(sessions, discovered...)
	}

	if len(sessions) == 0 {
		return nil, ErrNoMatch
	}

	var scoped []*Session
	for _, s := range sessions {
		if !sessionInSourceCheckout(ctx, s.CWD, originTop) {
			if p.Logf != nil {
				p.Logf("candidate agent=%s session=%s cwd=%q decision=rejected reason=unscoped",
					s.AgentName, s.SessionID, s.CWD)
			}
			continue
		}
		scoped = append(scoped, s)
	}
	if len(scoped) == 0 {
		return nil, unsafeMatchError("discovered %d transcript(s) but none belong to the source checkout %s", len(sessions), originTop)
	}

	// Load message bodies only for sessions that already proved they belong
	// to the source checkout. Sibling worktrees and same-remote clones never
	// reach scoring, cache, or summarization.
	var loaded []*Session
	for _, s := range scoped {
		var reader Reader
		for _, r := range p.Readers {
			if r != nil && r.Name() == s.AgentName {
				reader = r
				break
			}
		}
		if reader == nil {
			continue
		}
		if err := reader.Load(ctx, s); err != nil {
			slog.Debug("intent reader load failed", "agent", s.AgentName, "session", s.SessionID, "error", err)
			continue
		}
		loaded = append(loaded, s)
	}

	match, err := pickMatchWithOptions(loaded, p.DiffFiles, matchOptions{
		Threshold: p.Threshold,
		HeadTime:  p.HeadTime,
		Logf:      p.Logf,
	})
	if err != nil {
		return nil, err
	}

	if p.Recheck != nil {
		if err := p.Recheck(); err != nil {
			if errors.Is(err, ErrUnsafeMatch) {
				return nil, err
			}
			return nil, unsafeMatchError("source checkout identity changed during extraction: %v", err)
		}
	}

	key := cacheKeyFor(match.Session)
	if cached, ok := p.Cache.Get(key); ok && cached != "" {
		return &Result{
			Summary:   cached,
			AgentName: match.Session.AgentName,
			SessionID: match.Session.SessionID,
			Score:     match.Score,
		}, nil
	}

	summary, err := p.Summarizer.Summarize(ctx, match.Session)
	if err != nil {
		return nil, fmt.Errorf("intent: summarize: %w", err)
	}
	p.Cache.Put(key, summary, match.Session.AgentName, match.Session.SessionID)

	return &Result{
		Summary:   summary,
		AgentName: match.Session.AgentName,
		SessionID: match.Session.SessionID,
		Score:     match.Score,
	}, nil
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
