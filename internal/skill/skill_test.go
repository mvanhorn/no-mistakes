package skill

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/testguidance"
)

func TestMarkdownFrontmatter(t *testing.T) {
	md := Markdown()
	if !strings.HasPrefix(md, "---\n") {
		t.Fatalf("SKILL.md must start with YAML frontmatter, got:\n%s", md[:min(40, len(md))])
	}
	for _, want := range []string{
		"name: " + Name + "\n",
		"description: " + Description + "\n",
		"user-invocable: true\n",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("frontmatter missing %q", want)
		}
	}
	// Frontmatter block must be closed before the body.
	if strings.Count(md, "---\n") < 2 {
		t.Errorf("frontmatter not closed with a second --- delimiter")
	}
	if !strings.Contains(md, "no-mistakes axi run") {
		t.Errorf("body should document the axi run command")
	}
	// The user-level install is a genuine user installation, so it must stay
	// discoverable: the internal marker that hid the old vendored repo copies
	// must not come back.
	if strings.Contains(md, "internal: true") {
		t.Errorf("Markdown() must not be marked internal")
	}
}

func TestBodyIncludesGeneratedGateStepGuard(t *testing.T) {
	md := Markdown()
	for _, want := range []string{
		"## Active validation-step boundary",
		"must inspect, fix, and return only its assigned phase",
		"`error.code: nested_gate_context`",
		"return control to the outer executor",
		"`no-mistakes axi status`",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("installed skill guard snapshot missing %q", want)
		}
	}
}

func TestBodyDocumentsTaskFirstFlow(t *testing.T) {
	md := Markdown()
	for _, want := range []string{
		"## Two ways to invoke",
		"feature branch",
		"Inspect `git status` before you change or commit anything",
		"commit only the changes that belong to the user's task",
		"passing the user's task as your `--intent`",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("body should document the task-first flow: missing %q", want)
		}
	}
	if !strings.Contains(md, testguidance.Rule) {
		t.Errorf("task-first skill missing shared test-quality guidance:\n%s", md)
	}
}

func TestBodyDocumentsAxiGateGuidance(t *testing.T) {
	md := Markdown()
	for _, want := range []string{
		"inspect it with `no-mistakes axi status`",
		"drive it with `no-mistakes axi respond`",
		"when it still matches your current `HEAD`",
		"**Review auto-fix is disabled by default**",
		"blocking and",
		"ask-user review findings park for your decision",
		"`auto_fix.review > 0`",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("body should document AXI gate guidance: missing %q", want)
		}
	}
	if strings.Contains(md, "drive it to an outcome with `axi respond`") {
		t.Errorf("body should not tell agents to resume non-parked runs with axi respond")
	}
}

func TestInstallWritesBothPaths(t *testing.T) {
	root := t.TempDir()
	written, err := Install(root)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	wantRel := []string{
		filepath.Join(".claude", "skills", Name, "SKILL.md"),
		filepath.Join(".agents", "skills", Name, "SKILL.md"),
	}
	if len(written) != len(wantRel) {
		t.Fatalf("written = %v, want %v", written, wantRel)
	}
	for i, rel := range wantRel {
		if written[i] != rel {
			t.Errorf("written[%d] = %q, want %q", i, written[i], rel)
		}
		data, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if string(data) != Markdown() {
			t.Errorf("%s content does not match Markdown()", rel)
		}
	}
}

// TestInstallUserWritesUnderHome proves the init entry point resolves the
// user's home directory and installs there, never into the working directory.
// Unset and empty CLAUDE_CONFIG_DIR both keep the default layout.
func TestInstallUserWritesUnderHome(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T)
	}{
		{
			name: "unset_override",
			setup: func(t *testing.T) {
				unsetenv(t, "CLAUDE_CONFIG_DIR")
			},
		},
		{
			name: "empty_override",
			setup: func(t *testing.T) {
				t.Setenv("CLAUDE_CONFIG_DIR", "")
			},
		},
	}
	wantRel := []string{
		filepath.Join(".claude", "skills", Name, "SKILL.md"),
		filepath.Join(".agents", "skills", Name, "SKILL.md"),
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := isolateUserHome(t)
			tt.setup(t)

			written, err := InstallUser()
			if err != nil {
				t.Fatalf("InstallUser: %v", err)
			}
			if len(written) != len(wantRel) {
				t.Fatalf("written = %v, want %v", written, wantRel)
			}
			for i, rel := range wantRel {
				if written[i] != rel {
					t.Errorf("written[%d] = %q, want %q", i, written[i], rel)
				}
				assertSkillFile(t, filepath.Join(home, rel))
			}
		})
	}
}

// TestInstallUserHonorsClaudeConfigDir writes the Claude copy under a nonempty
// absolute override (including spaces) and the vendor-neutral copy under home,
// without creating the default ~/.claude/skills layout.
func TestInstallUserHonorsClaudeConfigDir(t *testing.T) {
	home := isolateUserHome(t)
	override := filepath.Join(t.TempDir(), "claude config dir")
	t.Setenv("CLAUDE_CONFIG_DIR", override)

	written, err := InstallUser()
	if err != nil {
		t.Fatalf("InstallUser: %v", err)
	}
	wantClaude := filepath.Join(override, "skills", Name, "SKILL.md")
	wantAgents := filepath.Join(".agents", "skills", Name, "SKILL.md")
	want := []string{wantClaude, wantAgents}
	if len(written) != len(want) {
		t.Fatalf("written = %v, want %v", written, want)
	}
	for i, p := range want {
		if written[i] != p {
			t.Errorf("written[%d] = %q, want %q", i, written[i], p)
		}
	}
	assertSkillFile(t, wantClaude)
	assertSkillFile(t, filepath.Join(home, wantAgents))
	assertNotExist(t, filepath.Join(home, ".claude", "skills", Name, "SKILL.md"))
}

// TestInstallUserResolvesClaudeConfigDirPerInvocation proves the override is
// read at each InstallUser call rather than package init, does not mutate
// InstallBases, and still refreshes stale content idempotently.
func TestInstallUserResolvesClaudeConfigDirPerInvocation(t *testing.T) {
	home := isolateUserHome(t)
	savedBases := append([]string(nil), InstallBases...)
	first := filepath.Join(t.TempDir(), "first")
	second := filepath.Join(t.TempDir(), "second")

	t.Setenv("CLAUDE_CONFIG_DIR", first)
	if _, err := InstallUser(); err != nil {
		t.Fatalf("first InstallUser: %v", err)
	}
	assertSkillFile(t, filepath.Join(first, "skills", Name, "SKILL.md"))

	t.Setenv("CLAUDE_CONFIG_DIR", second)
	if _, err := InstallUser(); err != nil {
		t.Fatalf("second InstallUser: %v", err)
	}
	assertSkillFile(t, filepath.Join(second, "skills", Name, "SKILL.md"))
	assertSkillFile(t, filepath.Join(home, ".agents", "skills", Name, "SKILL.md"))
	assertNotExist(t, filepath.Join(home, ".claude", "skills", Name, "SKILL.md"))

	if len(InstallBases) != len(savedBases) {
		t.Fatalf("InstallBases mutated: %v, want %v", InstallBases, savedBases)
	}
	for i, base := range savedBases {
		if InstallBases[i] != base {
			t.Errorf("InstallBases[%d] = %q, want %q", i, InstallBases[i], base)
		}
	}

	stale := filepath.Join(second, "skills", Name, "SKILL.md")
	if err := os.WriteFile(stale, []byte("---\nname: "+Name+"\n---\nstale body\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallUser(); err != nil {
		t.Fatalf("refresh InstallUser: %v", err)
	}
	assertSkillFile(t, stale)
	if _, err := InstallUser(); err != nil {
		t.Fatalf("idempotent InstallUser: %v", err)
	}
	assertSkillFile(t, stale)
}

// TestInstallUserHonorsRelativeClaudeConfigDir resolves a relative override
// against the process working directory, not home or the filesystem root.
func TestInstallUserHonorsRelativeClaudeConfigDir(t *testing.T) {
	home := isolateUserHome(t)
	wd := t.TempDir()
	t.Chdir(wd)
	t.Setenv("CLAUDE_CONFIG_DIR", "claude-config")

	written, err := InstallUser()
	if err != nil {
		t.Fatalf("InstallUser: %v", err)
	}
	abs, err := filepath.Abs("claude-config")
	if err != nil {
		t.Fatalf("Abs: %v", err)
	}
	wantClaude := filepath.Join(abs, "skills", Name, "SKILL.md")
	wantAgents := filepath.Join(".agents", "skills", Name, "SKILL.md")
	if len(written) != 2 || written[0] != wantClaude || written[1] != wantAgents {
		t.Fatalf("written = %v, want [%q %q]", written, wantClaude, wantAgents)
	}
	assertSkillFile(t, wantClaude)
	assertSkillFile(t, filepath.Join(wd, "claude-config", "skills", Name, "SKILL.md"))
	assertSkillFile(t, filepath.Join(home, wantAgents))
	assertNotExist(t, filepath.Join(home, "claude-config", "skills", Name, "SKILL.md"))
	assertNotExist(t, filepath.Join(home, ".claude", "skills", Name, "SKILL.md"))
}

func TestInstallUserClaudeConfigDirDanglingSymlink(t *testing.T) {
	home := isolateUserHome(t)
	root := t.TempDir()
	link := filepath.Join(root, "link")
	symlink(t, "missing-target", link)
	t.Setenv("CLAUDE_CONFIG_DIR", link)

	written, err := InstallUser()
	if err != nil {
		t.Fatalf("InstallUser: %v", err)
	}
	logical := filepath.Join(link, "skills", Name, "SKILL.md")
	if len(written) == 0 || written[0] != logical {
		t.Fatalf("written[0] = %v, want logical path %q", written, logical)
	}
	assertSkillFile(t, logical)
	assertSkillFile(t, filepath.Join(home, ".agents", "skills", Name, "SKILL.md"))
}

func TestInstallUserClaudeConfigDirSymlinkCycle(t *testing.T) {
	isolateUserHome(t)
	root := t.TempDir()
	a := filepath.Join(root, "a")
	b := filepath.Join(root, "b")
	symlink(t, "b", a)
	symlink(t, "a", b)
	t.Setenv("CLAUDE_CONFIG_DIR", a)

	if _, err := InstallUser(); err == nil {
		t.Fatal("InstallUser succeeded with cyclic CLAUDE_CONFIG_DIR symlink")
	}
}

func TestInstallUserClaudeConfigDirFileIsNotADirectory(t *testing.T) {
	home := isolateUserHome(t)
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", file)

	if _, err := InstallUser(); err == nil {
		t.Fatal("InstallUser succeeded when CLAUDE_CONFIG_DIR is a regular file")
	}
	assertNotExist(t, filepath.Join(home, ".claude", "skills", Name, "SKILL.md"))
	if _, err := os.Stat(filepath.Join(home, ".claude")); !os.IsNotExist(err) {
		t.Fatalf("default Claude directory created after override failure: stat %v", err)
	}
}

// TestInstallAndVendoredIgnoreClaudeConfigDir keeps Install(root) and
// Vendored(repoRoot) on the explicit-root default layout even when the
// user-level override is set.
func TestInstallAndVendoredIgnoreClaudeConfigDir(t *testing.T) {
	isolateUserHome(t)
	override := filepath.Join(t.TempDir(), "custom-claude")
	t.Setenv("CLAUDE_CONFIG_DIR", override)

	root := t.TempDir()
	written, err := Install(root)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	wantRel := []string{
		filepath.Join(".claude", "skills", Name, "SKILL.md"),
		filepath.Join(".agents", "skills", Name, "SKILL.md"),
	}
	if len(written) != len(wantRel) {
		t.Fatalf("written = %v, want %v", written, wantRel)
	}
	for i, rel := range wantRel {
		if written[i] != rel {
			t.Errorf("written[%d] = %q, want %q", i, written[i], rel)
		}
		assertSkillFile(t, filepath.Join(root, rel))
	}
	assertNotExist(t, filepath.Join(override, "skills", Name, "SKILL.md"))

	vendorRoot := t.TempDir()
	for _, base := range InstallBases {
		dir := filepath.Join(vendorRoot, base, Name)
		mkdirAll(t, dir)
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("legacy"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	wantVendored := []string{
		filepath.Join(".claude", "skills", Name, "SKILL.md"),
		filepath.Join(".agents", "skills", Name, "SKILL.md"),
	}
	got := Vendored(vendorRoot)
	if len(got) != len(wantVendored) {
		t.Fatalf("Vendored = %v, want %v", got, wantVendored)
	}
	for i := range wantVendored {
		if got[i] != wantVendored[i] {
			t.Errorf("Vendored[%d] = %q, want %q", i, got[i], wantVendored[i])
		}
	}
}

func TestInstallIsIdempotent(t *testing.T) {
	root := t.TempDir()
	if _, err := Install(root); err != nil {
		t.Fatalf("first install: %v", err)
	}
	if _, err := Install(root); err != nil {
		t.Fatalf("second install: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(root, ".claude", "skills", Name, "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != Markdown() {
		t.Errorf("content drifted after re-install")
	}
}

// TestInstallSymlinkLayouts covers home directories that consolidate the two
// skill bases with a symlink. `.claude/skills` may link to `.agents/skills`,
// the whole `.claude` dir may link to `.agents`, or the link may point the
// other way. In every case Install must succeed and the skill must be
// reachable via both logical bases - including when the symlink target dir
// does not exist yet.
func TestInstallSymlinkLayouts(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, root string)
	}{
		{
			name: "claude_skills_link_target_exists",
			setup: func(t *testing.T, root string) {
				mkdirAll(t, filepath.Join(root, ".agents", "skills"))
				mkdirAll(t, filepath.Join(root, ".claude"))
				symlink(t, filepath.Join("..", ".agents", "skills"), filepath.Join(root, ".claude", "skills"))
			},
		},
		{
			name: "claude_skills_link_target_missing",
			setup: func(t *testing.T, root string) {
				mkdirAll(t, filepath.Join(root, ".claude"))
				symlink(t, filepath.Join("..", ".agents", "skills"), filepath.Join(root, ".claude", "skills"))
			},
		},
		{
			name: "claude_dir_link",
			setup: func(t *testing.T, root string) {
				mkdirAll(t, filepath.Join(root, ".agents"))
				symlink(t, ".agents", filepath.Join(root, ".claude"))
			},
		},
		{
			name: "agents_skills_link_reverse",
			setup: func(t *testing.T, root string) {
				mkdirAll(t, filepath.Join(root, ".claude", "skills"))
				mkdirAll(t, filepath.Join(root, ".agents"))
				symlink(t, filepath.Join("..", ".claude", "skills"), filepath.Join(root, ".agents", "skills"))
			},
		},
		{
			name: "agents_dir_link_reverse",
			setup: func(t *testing.T, root string) {
				mkdirAll(t, filepath.Join(root, ".claude"))
				symlink(t, ".claude", filepath.Join(root, ".agents"))
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			tt.setup(t, root)

			written, err := Install(root)
			if err != nil {
				t.Fatalf("Install: %v", err)
			}

			// Every reported path must be readable with current content.
			for _, rel := range written {
				data, err := os.ReadFile(filepath.Join(root, rel))
				if err != nil {
					t.Fatalf("read reported %s: %v", rel, err)
				}
				if string(data) != Markdown() {
					t.Errorf("%s content does not match Markdown()", rel)
				}
			}

			// The skill must be discoverable via both logical bases no matter
			// which side carries the symlink.
			for _, base := range InstallBases {
				p := filepath.Join(root, base, Name, "SKILL.md")
				data, err := os.ReadFile(p)
				if err != nil {
					t.Fatalf("skill not reachable via %s: %v", base, err)
				}
				if string(data) != Markdown() {
					t.Errorf("%s content does not match Markdown()", base)
				}
			}
		})
	}
}

// TestInstallOverwritesStaleContent guards the upgrade path: an older SKILL.md
// left by a previous binary version must be refreshed to current content when
// Install runs again.
func TestInstallOverwritesStaleContent(t *testing.T) {
	root := t.TempDir()
	stale := filepath.Join(root, ".claude", "skills", Name, "SKILL.md")
	mkdirAll(t, filepath.Dir(stale))
	if err := os.WriteFile(stale, []byte("---\nname: "+Name+"\n---\nstale body\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(root); err != nil {
		t.Fatalf("Install: %v", err)
	}
	data, err := os.ReadFile(stale)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != Markdown() {
		t.Errorf("stale SKILL.md was not refreshed to current content")
	}
}

func TestInstallRejectsSymlinkCycle(t *testing.T) {
	root := t.TempDir()
	symlink(t, ".agents", filepath.Join(root, ".claude"))
	symlink(t, ".claude", filepath.Join(root, ".agents"))

	if _, err := Install(root); err == nil {
		t.Fatalf("Install succeeded with cyclic skill directory symlinks")
	}
}

// TestVendored covers the legacy-detection helper init uses to tell users a
// repo still carries a vendored skill copy from an older no-mistakes version.
func TestVendored(t *testing.T) {
	t.Run("clean_repo", func(t *testing.T) {
		if got := Vendored(t.TempDir()); len(got) != 0 {
			t.Errorf("Vendored on a clean repo = %v, want none", got)
		}
	})

	t.Run("both_copies", func(t *testing.T) {
		root := t.TempDir()
		for _, base := range InstallBases {
			dir := filepath.Join(root, base, Name)
			mkdirAll(t, dir)
			if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("legacy"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		want := []string{
			filepath.Join(".claude", "skills", Name, "SKILL.md"),
			filepath.Join(".agents", "skills", Name, "SKILL.md"),
		}
		got := Vendored(root)
		if len(got) != len(want) {
			t.Fatalf("Vendored = %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("Vendored[%d] = %q, want %q", i, got[i], want[i])
			}
		}
	})

	t.Run("single_copy", func(t *testing.T) {
		root := t.TempDir()
		dir := filepath.Join(root, ".agents", "skills", Name)
		mkdirAll(t, dir)
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("legacy"), 0o644); err != nil {
			t.Fatal(err)
		}
		got := Vendored(root)
		if len(got) != 1 || got[0] != filepath.Join(".agents", "skills", Name, "SKILL.md") {
			t.Errorf("Vendored = %v, want only the .agents copy", got)
		}
	})

	t.Run("unrelated_skill_ignored", func(t *testing.T) {
		root := t.TempDir()
		dir := filepath.Join(root, ".claude", "skills", "other-skill")
		mkdirAll(t, dir)
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("other"), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := Vendored(root); len(got) != 0 {
			t.Errorf("Vendored must ignore unrelated skills, got %v", got)
		}
	})
}

func isolateUserHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	// os.UserHomeDir reads HOME on Unix and USERPROFILE on Windows; set both
	// so the test isolates the real home directory on every platform.
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	return home
}

func unsetenv(t *testing.T, key string) {
	t.Helper()
	orig, ok := os.LookupEnv(key)
	if err := os.Unsetenv(key); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if !ok {
			os.Unsetenv(key)
			return
		}
		os.Setenv(key, orig)
	})
}

func assertSkillFile(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(data) != Markdown() {
		t.Errorf("%s content does not match Markdown()", path)
	}
}

func assertNotExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("%s: want not exist, stat %v", path, err)
	}
}

func mkdirAll(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
