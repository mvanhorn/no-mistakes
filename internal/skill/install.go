package skill

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var (
	claudeSkillsBase = filepath.Join(".claude", "skills")
	agentsSkillsBase = filepath.Join(".agents", "skills")
)

// InstallBases are the user-level agent skill parent directories, relative to
// the user's home directory, that init populates. `~/.claude/skills` is Claude
// Code's personal-skill location (OpenCode reads it too); `~/.agents/skills`
// is the vendor-neutral user-level convention Codex, OpenCode, Rovo Dev, and
// Pi all read. The slice is the default layout only: InstallUser may replace
// the Claude entry from CLAUDE_CONFIG_DIR, but Install, Vendored, and this
// list itself never read the environment.
var InstallBases = []string{
	claudeSkillsBase,
	agentsSkillsBase,
}

// InstallUser installs the skill into the agent skill directories for the
// current user. The vendor-neutral copy always lands at
// <home>/.agents/skills. The Claude copy lands at <home>/.claude/skills
// unless CLAUDE_CONFIG_DIR is a nonempty value, in which case it lands at
// <CLAUDE_CONFIG_DIR>/skills. Relative overrides are resolved against the
// process working directory with filepath.Abs and are used literally (no
// tilde or shell expansion); spaces are preserved. Resolution happens at
// each call, not at package init, and does not mutate InstallBases.
//
// Returned paths are home-relative for the default layout, matching
// Install(home). When the Claude destination is an override outside that
// layout, the Claude filename is the absolute logical path so callers can
// identify the file actually written. Errors resolving or writing the
// override are returned without falling back to the default Claude location.
func InstallUser() ([]string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve home directory: %w", err)
	}
	claudeDir, claudeReport, err := userClaudeSkillsDest(home)
	if err != nil {
		return nil, err
	}
	return installAt([]installDest{
		{dir: claudeDir, report: claudeReport},
		{
			dir:    filepath.Join(home, agentsSkillsBase),
			report: filepath.Join(agentsSkillsBase, Name, "SKILL.md"),
		},
	})
}

// userClaudeSkillsDest returns the Claude skills parent directory and the
// path InstallUser should report for SKILL.md. An unset or empty
// CLAUDE_CONFIG_DIR keeps the default <home>/.claude/skills layout.
func userClaudeSkillsDest(home string) (dir, report string, err error) {
	override := os.Getenv("CLAUDE_CONFIG_DIR")
	if override == "" {
		dir = filepath.Join(home, claudeSkillsBase)
		report = filepath.Join(claudeSkillsBase, Name, "SKILL.md")
		return dir, report, nil
	}
	abs, err := filepath.Abs(override)
	if err != nil {
		return "", "", fmt.Errorf("resolve CLAUDE_CONFIG_DIR: %w", err)
	}
	dir = filepath.Join(abs, "skills")
	return dir, filepath.Join(dir, Name, "SKILL.md"), nil
}

// Install writes SKILL.md into each agent skills directory under root
// (normally the user's home directory), creating directories as needed. It
// returns the root-relative paths written so the caller can report them.
// Writing is idempotent: re-running overwrites with identical content
// (refreshing a stale SKILL.md from an older version).
//
// Install is rooted in its explicit argument and does not consult
// CLAUDE_CONFIG_DIR; that override is InstallUser's concern.
//
// Users may consolidate the two bases with a symlink - `.claude/skills` ->
// `.agents/skills`, the whole `.claude` dir -> `.agents`, or the reverse. Install
// follows such links transparently, including when the symlinked target dir does
// not exist yet (a plain os.MkdirAll would fail with "file exists" on a dangling
// symlink). Both logical bases stay readable afterward via the link.
func Install(root string) ([]string, error) {
	dests := make([]installDest, 0, len(InstallBases))
	for _, base := range InstallBases {
		dests = append(dests, installDest{
			dir:    filepath.Join(root, base),
			report: filepath.Join(base, Name, "SKILL.md"),
		})
	}
	return installAt(dests)
}

// installDest is one skills parent directory to populate. dir is the
// concrete destination (for example /home/user/.claude/skills); report is
// the path returned to the caller and is not recomputed from the resolved
// directory, so a logical (possibly relative) path survives symlink resolution.
type installDest struct {
	dir    string
	report string
}

// installAt writes SKILL.md into each destination skills directory. Writing
// is idempotent. On error, destinations already written are still returned
// so callers can report partial success.
func installAt(dests []installDest) ([]string, error) {
	content := []byte(Markdown())
	written := make([]string, 0, len(dests))
	for _, dest := range dests {
		path := filepath.Join(dest.dir, Name, "SKILL.md")
		// Resolve any symlink components to a real directory before creating
		// it, so a dangling symlink in the path does not collide with MkdirAll.
		realDir, err := resolveThroughSymlinks(filepath.Dir(path))
		if err != nil {
			return written, err
		}
		if err := os.MkdirAll(realDir, 0o755); err != nil {
			return written, err
		}
		if err := os.WriteFile(filepath.Join(realDir, "SKILL.md"), content, 0o644); err != nil {
			return written, err
		}
		written = append(written, dest.report)
	}
	return written, nil
}

// Vendored reports the repo-relative paths of legacy vendored skill copies
// under repoRoot. Older no-mistakes versions wrote SKILL.md into each
// initialized repo; init uses this to tell users those copies are no longer
// needed. It never modifies the repo.
func Vendored(repoRoot string) []string {
	var found []string
	for _, base := range InstallBases {
		rel := filepath.Join(base, Name, "SKILL.md")
		if _, err := os.Stat(filepath.Join(repoRoot, rel)); err == nil {
			found = append(found, rel)
		}
	}
	return found
}

// resolveThroughSymlinks walks dir component by component and rewrites the path
// through any symlink it encounters, even when the symlink's target does not
// exist yet. The result contains no symlink components, so os.MkdirAll on it
// will not trip over a dangling symlink. dir must be absolute.
func resolveThroughSymlinks(dir string) (string, error) {
	return resolveThroughSymlinksSeen(dir, make(map[string]struct{}))
}

func resolveThroughSymlinksSeen(dir string, seen map[string]struct{}) (string, error) {
	clean := filepath.Clean(dir)
	volume := filepath.VolumeName(clean)
	cur := volume + string(filepath.Separator)
	for _, part := range strings.Split(strings.TrimPrefix(clean, volume), string(filepath.Separator)) {
		if part == "" {
			continue
		}
		cur = filepath.Join(cur, part)
		info, err := os.Lstat(cur)
		if err != nil {
			// This component does not exist yet; nothing left to resolve.
			// Remaining parts are appended verbatim onto the resolved prefix.
			continue
		}
		if info.Mode()&os.ModeSymlink == 0 {
			continue
		}
		key := filepath.Clean(cur)
		if _, ok := seen[key]; ok {
			return "", fmt.Errorf("symlink cycle resolving %s", dir)
		}
		seen[key] = struct{}{}
		target, err := os.Readlink(cur)
		if err != nil {
			return "", err
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(cur), target)
		}
		// The target may itself be or contain symlinks; resolve recursively.
		if cur, err = resolveThroughSymlinksSeen(target, seen); err != nil {
			return "", err
		}
	}
	return cur, nil
}
