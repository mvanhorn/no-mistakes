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
	// OriginCWD is the authoring checkout proven by ResolveAuthoringCheckout.
	OriginCWD string
	// DiffFiles is the repo-relative file set used for matching and scoring.
	DiffFiles []string
	// BaseTime is the committer time of the base SHA.
	BaseTime time.Time
	// HeadTime is the committer time of the head SHA.
	HeadTime time.Time
	// SlackDays extends WindowStart backwards. The plan called for 3 days.
	SlackDays int
	// Threshold may raise, but never lower, the raw overlap safety floor.
	Threshold float64
	// Readers discover metadata; Extract enforces checkout eligibility centrally.
	Readers []Reader
	// Cache is consulted before summarization. Pass NewMemCache() if no DB.
	Cache Cache
	// Summarizer turns the chosen session's text into a short summary.
	Summarizer Summarizer
	// Logf receives scope and match diagnostics, without transcript text.
	Logf func(format string, args ...any)
}

// ErrNoMatch indicates no agent transcript matched the change. Callers
// should treat this as a normal "no intent attached" outcome, not an error.
var ErrNoMatch = errors.New("intent: no matching transcript")

// ErrUnsafeMatch indicates that discovered evidence cannot safely identify the
// authoring session. Callers must ask the operator before continuing.
var ErrUnsafeMatch = errors.New("intent: unsafe transcript match")

// Extract validates scope and raw overlap before consulting the summary cache.
func Extract(ctx context.Context, p ExtractParams) (*Result, error) {
	if p.OriginCWD == "" {
		return nil, fmt.Errorf("%w: authoring checkout is required", ErrUnsafeMatch)
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

	slack := time.Duration(maxInt(p.SlackDays, 0)) * 24 * time.Hour
	opts := DiscoverOpts{
		HomeDir:     p.HomeDir,
		OriginCWD:   canonicalPath(p.OriginCWD),
		WindowStart: p.BaseTime.Add(-slack),
		WindowEnd:   p.HeadTime,
	}

	var sessions []*Session
	rejectedScope := false
	originRoot := checkoutRoot(ctx, p.OriginCWD)
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
			if s == nil {
				continue
			}
			s.AgentName = r.Name()
			if originRoot == "" || checkoutRoot(ctx, s.CWD) != originRoot {
				rejectedScope = true
				if p.Logf != nil {
					p.Logf("decision=rejected_scope agent=%s session=%s", s.AgentName, s.SessionID)
				}
				continue
			}
			sessions = append(sessions, s)
		}
	}

	if len(sessions) == 0 {
		if rejectedScope {
			return nil, fmt.Errorf("%w: discovered transcripts do not belong to the authoring checkout", ErrUnsafeMatch)
		}
		return nil, ErrNoMatch
	}

	// Load bodies only after every reader has passed the same checkout check.
	var loaded []*Session
	for _, s := range sessions {
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

	if len(loaded) == 0 && !rejectedScope {
		return nil, ErrNoMatch
	}
	match, err := selectMatch(loaded, p.DiffFiles, matchOptions{
		Threshold: p.Threshold,
		HeadTime:  p.HeadTime,
		Logf:      p.Logf,
	})
	if err != nil {
		return nil, err
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
