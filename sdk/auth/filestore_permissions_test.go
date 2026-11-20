package auth

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestFileTokenStoreSaveTightensExistingFileMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits do not apply on Windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "claude-user.json")
	if errWrite := os.WriteFile(path, []byte(`{"type":"claude","access_token":"old"}`), 0o644); errWrite != nil {
		t.Fatal(errWrite)
	}
	if errChmod := os.Chmod(path, 0o644); errChmod != nil {
		t.Fatal(errChmod)
	}

	store := NewFileTokenStore()
	store.SetBaseDir(dir)
	auth := &cliproxyauth.Auth{
		ID:         "claude-user.json",
		Provider:   "claude",
		FileName:   "claude-user.json",
		Attributes: map[string]string{cliproxyauth.AttributePath: path},
		Metadata:   map[string]any{"type": "claude", "access_token": "new"},
	}
	if _, errSave := store.Save(context.Background(), auth); errSave != nil {
		t.Fatalf("save: %v", errSave)
	}
	info, errStat := os.Stat(path)
	if errStat != nil {
		t.Fatal(errStat)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("rewritten auth file mode = %o, want 600", got)
	}
}
