package vertex

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestSaveTokenToFileWritesOwnerOnlyFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits do not apply on Windows")
	}
	dir := t.TempDir()
	fresh := filepath.Join(dir, "fresh.json")
	existing := filepath.Join(dir, "existing.json")
	if errWrite := os.WriteFile(existing, []byte("{}"), 0o644); errWrite != nil {
		t.Fatal(errWrite)
	}
	if errChmod := os.Chmod(existing, 0o644); errChmod != nil {
		t.Fatal(errChmod)
	}

	for _, path := range []string{fresh, existing} {
		ts := &VertexCredentialStorage{ServiceAccount: map[string]any{"private_key": "secret"}}
		if errSave := ts.SaveTokenToFile(path); errSave != nil {
			t.Fatalf("save %s: %v", filepath.Base(path), errSave)
		}
		info, errStat := os.Stat(path)
		if errStat != nil {
			t.Fatal(errStat)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("%s mode = %o, want 600", filepath.Base(path), got)
		}
	}
}
