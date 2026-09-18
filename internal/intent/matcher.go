package intent

import (
	"math"
	"path/filepath"
	"strings"
	"time"
)

const (
	// decisiveMatchScore is the hard raw-overlap safety floor for automatic
	// transcript selection. It is a conservative acceptance cutoff, not a
	// confidence probability. A configured intent.threshold may raise this
	// floor but cannot lower it.
	decisiveMatchScore = 0.85
	minWinnerMargin    = 0.10
	// scoreEpsilon absorbs binary rounding so exact decimal boundaries such
	// as 0.85 and a 0.10 margin still accept when the file-ratio math is exact.
	scoreEpsilon = 1e-9
)

// Match is the chosen session along with its overlap score.
type Match struct {
	Session *Session
	Score   float64
	// Confidence is the score used to rank accepted candidates after applying
	// recency. Score remains the raw file-overlap score surfaced to callers.
	Confidence float64
	// Overlap lists diff files that appeared in this session. Used purely
	// for diagnostics/telemetry.
	Overlap []string
}

type matchOptions struct {
	Threshold float64
	HeadTime  time.Time
	Logf      func(format string, args ...any)
}

// score computes the share of diff files that appear anywhere in the
// session's messages. The score is symmetric in the sense that a session
// that touched many extra unrelated files is not penalized - only the
// portion of *this* diff that the session covered matters.
func score(s *Session, diffFiles []string) (float64, []string) {
	if len(diffFiles) == 0 || len(s.Messages) == 0 {
		return 0, nil
	}

	var mentioned []string
	for _, m := range s.Messages {
		for _, p := range m.FilePaths {
			mentioned = append(mentioned, p)
		}
		// Best-effort scan of assistant text for raw filenames.
		mentioned = append(mentioned, scanFilePathsInText(m.Text)...)
	}

	var overlap []string
	for _, f := range diffFiles {
		for _, p := range mentioned {
			if pathMentionMatchesDiff(p, f) {
				overlap = append(overlap, f)
				break
			}
		}
	}
	return float64(len(overlap)) / float64(len(diffFiles)), overlap
}

func pathMentionMatchesDiff(mention, diffFile string) bool {
	mention = filepath.ToSlash(filepath.Clean(strings.TrimSpace(mention)))
	mention = strings.TrimPrefix(mention, "./")
	diffFile = filepath.ToSlash(filepath.Clean(strings.TrimSpace(diffFile)))
	diffFile = strings.TrimPrefix(diffFile, "./")
	if mention == "" || diffFile == "" || mention == "." || diffFile == "." {
		return false
	}
	if mention == diffFile || strings.HasSuffix(mention, "/"+diffFile) {
		return true
	}
	// Basename-only mentions are useful, but pathful mentions should not match
	// unrelated files that happen to share names like update.go.
	return !strings.Contains(mention, "/") && filepath.Base(diffFile) == mention
}

// pickMatch returns the unique raw-overlap winner among sessions that pass
// the multi-file and freshness filters and then the safety floor/margin.
// Recency is diagnostic only: it never lifts a candidate over the floor or
// breaks a near tie into an accepted match.
func pickMatch(sessions []*Session, diffFiles []string, threshold float64) (*Match, error) {
	return pickMatchWithOptions(sessions, diffFiles, matchOptions{Threshold: threshold})
}

func pickMatchWithOptions(sessions []*Session, diffFiles []string, opts matchOptions) (*Match, error) {
	if len(diffFiles) == 0 {
		return nil, ErrNoMatch
	}
	distinct := distinctScoredSessions(sessions, diffFiles)
	if len(distinct) == 0 {
		return nil, ErrNoMatch
	}

	var relevant []*Match
	for _, candidate := range distinct {
		ok, reason, confidence := relevantOverlapFilters(candidate.Score, len(candidate.Overlap), len(diffFiles), candidate.Session.LastActivity, opts)
		candidate.Confidence = confidence
		if !ok {
			logMatchDecision(opts, candidate, len(diffFiles), "rejected", reason)
			continue
		}
		relevant = append(relevant, candidate)
	}
	if len(relevant) == 0 {
		return nil, ErrNoMatch
	}

	winner := leadingByRawScore(relevant)
	floor := effectiveAcceptanceFloor(opts.Threshold)
	if winner.Score+scoreEpsilon < floor {
		logMatchDecision(opts, winner, len(diffFiles), "rejected", "below_floor")
		return nil, unsafeMatchError("best overlapping session score %.2f is below the %.2f acceptance floor", winner.Score, floor)
	}

	var runnerUp *Match
	for _, candidate := range relevant {
		if sameSession(candidate, winner) {
			continue
		}
		if runnerUp == nil || candidate.Score > runnerUp.Score {
			runnerUp = candidate
		}
	}
	if runnerUp != nil && winner.Score-runnerUp.Score+scoreEpsilon < minWinnerMargin {
		logMatchDecision(opts, winner, len(diffFiles), "rejected", "ambiguous")
		logMatchDecision(opts, runnerUp, len(diffFiles), "rejected", "ambiguous")
		return nil, unsafeMatchError("overlapping sessions are too close (winner %.2f, runner-up %.2f; need margin >= %.2f)", winner.Score, runnerUp.Score, minWinnerMargin)
	}

	for _, candidate := range relevant {
		if sameSession(candidate, winner) {
			logMatchDecision(opts, candidate, len(diffFiles), "accepted", "matched")
			continue
		}
		logMatchDecision(opts, candidate, len(diffFiles), "rejected", "runner_up")
	}
	return winner, nil
}

func distinctScoredSessions(sessions []*Session, diffFiles []string) []*Match {
	best := make(map[string]*Match, len(sessions))
	order := make([]string, 0, len(sessions))
	for _, s := range sessions {
		if s == nil {
			continue
		}
		sc, overlap := score(s, diffFiles)
		key := sessionKey(s)
		candidate := &Match{Session: s, Score: sc, Overlap: overlap}
		prev, ok := best[key]
		if !ok {
			best[key] = candidate
			order = append(order, key)
			continue
		}
		if candidate.Score > prev.Score || (candidate.Score == prev.Score && s.LastActivity.After(prev.Session.LastActivity)) {
			best[key] = candidate
		}
	}
	out := make([]*Match, 0, len(order))
	for _, key := range order {
		out = append(out, best[key])
	}
	return out
}

func leadingByRawScore(candidates []*Match) *Match {
	var best *Match
	for _, candidate := range candidates {
		if best == nil || candidate.Score > best.Score {
			best = candidate
		}
	}
	return best
}

func sameSession(a, b *Match) bool {
	if a == nil || b == nil || a.Session == nil || b.Session == nil {
		return a == b
	}
	return sessionKey(a.Session) == sessionKey(b.Session)
}

func sessionKey(s *Session) string {
	if s == nil {
		return ""
	}
	return s.AgentName + "\x00" + s.SessionID
}

func relevantOverlapFilters(score float64, overlapCount, diffCount int, lastActivity time.Time, opts matchOptions) (bool, string, float64) {
	if diffCount == 0 || overlapCount == 0 {
		return false, "no_overlap", score
	}
	if diffCount > 1 && overlapCount < 2 {
		return false, "single_overlap_multi_file_diff", score
	}
	confidence := score + recencyBoost(opts.HeadTime, lastActivity)
	if !opts.HeadTime.IsZero() && lastActivity.Before(opts.HeadTime.Add(-24*time.Hour)) && score < 0.8 {
		return false, "stale_partial", confidence
	}
	return true, "overlapping", confidence
}

func effectiveAcceptanceFloor(configured float64) float64 {
	if !isFinite(configured) || configured < 0 {
		return decisiveMatchScore
	}
	if configured > decisiveMatchScore {
		return configured
	}
	return decisiveMatchScore
}

func isFinite(f float64) bool {
	return !math.IsNaN(f) && !math.IsInf(f, 0)
}

func logMatchDecision(opts matchOptions, m *Match, diffCount int, decision, reason string) {
	if opts.Logf == nil || m == nil || m.Session == nil {
		return
	}
	opts.Logf("candidate agent=%s session=%s cwd=%q score %.2f confidence %.2f overlap=%d/%d decision=%s reason=%s",
		m.Session.AgentName, m.Session.SessionID, m.Session.CWD, m.Score, m.Confidence, len(m.Overlap), diffCount, decision, reason)
}

func recencyBoost(headTime, lastActivity time.Time) float64 {
	if headTime.IsZero() || lastActivity.IsZero() {
		return 0
	}
	age := headTime.Sub(lastActivity)
	if age < 0 {
		age = -age
	}
	if age <= 2*time.Hour {
		return 0.15
	}
	if age <= 24*time.Hour {
		return 0.05
	}
	return 0
}

// normalizedPathVariants returns a small set of normalized forms for a
// path so that absolute, repo-relative, and basename mentions can match.
func normalizedPathVariants(p string) []string {
	if p == "" {
		return nil
	}
	cleaned := filepath.ToSlash(filepath.Clean(strings.TrimSpace(p)))
	cleaned = strings.TrimPrefix(cleaned, "./")
	variants := map[string]bool{cleaned: true}
	if base := filepath.Base(cleaned); base != "" && base != "." {
		variants[base] = true
	}
	out := make([]string, 0, len(variants))
	for v := range variants {
		out = append(out, v)
	}
	return out
}

// scanFilePathsInText extracts plausible file paths from prose. The regex
// is intentionally permissive - false positives don't matter because the
// matcher only treats them as candidates, not ground truth.
func scanFilePathsInText(text string) []string {
	if text == "" {
		return nil
	}
	var out []string
	for _, tok := range filePathTokens.FindAllString(text, -1) {
		tok = strings.Trim(tok, "\"'`,;:()[]{}<>")
		if tok == "" {
			continue
		}
		// Require an extension or path separator to avoid matching prose words.
		if strings.ContainsAny(tok, "/\\") || strings.Contains(tok, ".") {
			out = append(out, tok)
		}
	}
	return out
}
