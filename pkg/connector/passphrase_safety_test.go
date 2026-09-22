package connector

import (
	"os"
	"path/filepath"
	"testing"
)

// An unreadable primary must not be reported as "absent", because the caller
// treats absent as licence to generate a replacement over it.
func TestReadPassphraseFile_UnreadablePrimaryIsNotAbsent(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits do not deny reads")
	}
	t.Chdir(t.TempDir())

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
