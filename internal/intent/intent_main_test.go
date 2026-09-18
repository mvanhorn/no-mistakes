package intent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "no-mistakes-intent-tests-")
	if err != nil {
		panic(err)
	}
	if err := os.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(dir, "gitconfig")); err != nil {
		panic(err)
	}
	if err := os.Setenv("GIT_CONFIG_NOSYSTEM", "1"); err != nil {
		panic(err)
	}
	if err := os.Unsetenv("GIT_CONFIG_COUNT"); err != nil {
		panic(err)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
