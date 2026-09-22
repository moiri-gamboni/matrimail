package connector

import (
	"os"
	"path/filepath"
	"testing"
)

// An existing passphrase must never be replaced by the auto-generation path.
// Before this, a first run that found a key already on disk would overwrite it,
// making every stored credential -- including a mailbox refresh token --
// permanently undecryptable, announced as a routine startup line.
func TestGenerateAndStorePassphrase_RefusesToOverwrite(t *testing.T) {
	dir := t.TempDir()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	defer func() { _ = os.Chdir(cwd) }()

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

// An unreadable primary must not be reported as "absent", because the caller
// treats absent as licence to generate a replacement over it.
func TestReadPassphraseFile_UnreadablePrimaryIsNotAbsent(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits do not deny reads")
	}
	dir := t.TempDir()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	defer func() { _ = os.Chdir(cwd) }()

	path, err := getPassphraseFilePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("secret"), 0o000); err != nil {
		t.Fatal(err)
	}

	got, err := readPassphraseFile()
	if err == nil {
		t.Fatalf("unreadable passphrase read as %q with no error", got)
	}
	if os.IsNotExist(err) {
		t.Fatal("unreadable primary reported as not-exist; the caller would regenerate over it")
	}
}
