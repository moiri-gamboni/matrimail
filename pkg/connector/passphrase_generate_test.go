package connector

import (
	"os"
	"path/filepath"
	"testing"
)

// An existing passphrase must never be replaced by the auto-generation path.
// A key already on disk, overwritten, makes every stored credential --
// including a mailbox refresh token -- permanently undecryptable.
func TestGenerateAndStorePassphrase_RefusesToOverwrite(t *testing.T) {
	t.Chdir(t.TempDir())

	path, err := getPassphraseFilePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("the-existing-key"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := generateAndStorePassphrase(); err == nil {
		t.Fatal("generated a passphrase over an existing file; the old key is now unrecoverable")
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "the-existing-key" {
		t.Fatalf("existing key was modified: %q", got)
	}
}
