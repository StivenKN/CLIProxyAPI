package cliproxy

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestEnsureAuthDirCreatesOwnerOnlyDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits do not apply on Windows")
	}
	authDir := filepath.Join(t.TempDir(), "auths")
	s := &Service{cfg: &config.Config{AuthDir: authDir}}
	if errEnsure := s.ensureAuthDir(); errEnsure != nil {
		t.Fatalf("ensureAuthDir: %v", errEnsure)
	}
	info, errStat := os.Stat(authDir)
	if errStat != nil {
		t.Fatal(errStat)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("auth dir mode = %o, want 700", got)
	}
}
