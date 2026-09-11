package intent

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	decisiveMatchScore = 0.85
	minimumMatchMargin = 0.10
)

// Match is the chosen session along with its overlap score.
type Match struct {
	Session *Session
	Score   float64
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

// pickMatch is the convenience wrapper for callers that only need a winner.
func pickMatch(sessions []*Session, diffFiles []string, threshold float64) *Match {
	return pickMatchWithOptions(sessions, diffFiles, matchOptions{Threshold: threshold})
}

func pickMatchWithOptions(sessions []*Session, diffFiles []string, opts matchOptions) *Match {
	match, _ := selectMatch(sessions, diffFiles, opts)
	return match
}

// selectMatch compares plausible contenders before applying the final floor.
// A runner-up below that floor still makes a close selection unsafe.
func selectMatch(sessions []*Session, diffFiles []string, opts matchOptions) (*Match, error) {
	var candidates []*Match
	seen := make(map[[2]string]bool)
	for _, s := range sessions {
		key := [2]string{s.AgentName, s.SessionID}
		if s.SessionID != "" && seen[key] {
			continue
		}
		seen[key] = true
		sc, overlap := score(s, diffFiles)
		reason := "plausible"
		switch {
		case len(overlap) == 0:
			reason = "no_overlap"
		case len(diffFiles) > 1 && len(overlap) < 2:
			reason = "single_overlap_multi_file_diff"
		case len(diffFiles) > 1 && sc < 0.5:
			reason = "below_plausible_overlap"
		case !opts.HeadTime.IsZero() && s.LastActivity.Before(opts.HeadTime.Add(-24*time.Hour)) && sc < 0.8:
			reason = "stale_partial"
		}
		if opts.Logf != nil {
			opts.Logf("candidate agent=%s session=%s score %.2f overlap=%d/%d decision=%s", s.AgentName, s.SessionID, sc, len(overlap), len(diffFiles), reason)
		}
		if reason == "plausible" {
			candidates = append(candidates, &Match{Session: s, Score: sc, Overlap: overlap})
		}
	}
	refuse := func(reason string) (*Match, error) {
		if opts.Logf != nil {
			opts.Logf("decision=rejected %s", reason)
		}
		return nil, fmt.Errorf("%w: %s", ErrUnsafeMatch, reason)
	}
	if len(candidates) == 0 {
		return refuse("no scoped transcript has sufficient overlap and freshness")
	}
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].Score > candidates[j].Score })
	winner := candidates[0]
	floor := decisiveMatchScore
	if opts.Threshold > floor {
		floor = opts.Threshold
	}
	if winner.Score < floor {
		return refuse(fmt.Sprintf("raw overlap %.2f is below required %.2f", winner.Score, floor))
	}
	// Both scores share the diff-file denominator. Subtract overlap counts
	// first to avoid floating-point subtraction at the exact 0.10 boundary.
	if len(candidates) > 1 && float64(len(winner.Overlap)-len(candidates[1].Overlap))/float64(len(diffFiles)) < minimumMatchMargin {
		return refuse(fmt.Sprintf("ambiguous raw overlap %.2f versus %.2f; required lead %.2f", winner.Score, candidates[1].Score, minimumMatchMargin))
	}
	if opts.Logf != nil {
		opts.Logf("decision=accepted agent=%s session=%s score %.2f", winner.Session.AgentName, winner.Session.SessionID, winner.Score)
	}
	return winner, nil
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
