package connector

import (
	"errors"
	"strings"
	"testing"
)

// Nothing re-encrypts stored credentials when the passphrase changes, so a
// changed passphrase turns every one of them into unreadable bytes. Before the
// startup check, the bridge began serving anyway and surfaced that one account
// at a time as ordinary decrypt failures, well after the restart that caused
// it. For a Gmail account the orphaned credential is a refresh token that
// cannot be recovered from anywhere.

func alwaysFails(string) (string, error) {
	return "", errors.New("cipher: message authentication failed")
}
func alwaysWorks(string) (string, error) { return "secret", nil }

// The case the check exists for: a passphrase that reads none of the stored
// credentials. Every row fails, which is what distinguishes it from damage to
// one row.
func TestKeyMatchesStored_WrongPassphraseIsRefused(t *testing.T) {
	t.Parallel()
	stored := []string{encPrefix + "aaaa", "", encPrefix + "bbbb"}

	err := keyMatchesStored(stored, alwaysFails)
	if err == nil {
		t.Fatal("no error; the bridge would start and orphan every stored credential")
	}
	// The message has to carry the recovery, because by the time anyone reads
	// it the only copy of the old passphrase may be in their shell history.
	for _, want := range []string{"MATRIMAIL_PASSPHRASE", "Restore the previous"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %s", want, err)
		}
	}
	if !strings.Contains(err.Error(), "2 stored credentials") {
		t.Errorf("error should count the affected credentials: %s", err)
	}
}

// One readable credential proves the key is right, so a single corrupt row
// must not be mistaken for a wrong passphrase and lock the user out of a
// working bridge.
func TestKeyMatchesStored_OneCorruptRowIsNotAWrongPassphrase(t *testing.T) {
	t.Parallel()
	calls := 0
	mixed := func(v string) (string, error) {
		calls++
		if strings.HasSuffix(v, "corrupt") {
			return "", errors.New("cipher: message authentication failed")
		}
		return "secret", nil
	}

	stored := []string{encPrefix + "corrupt", encPrefix + "fine"}
	if err := keyMatchesStored(stored, mixed); err != nil {
		t.Errorf("refused to start over one unreadable row: %v", err)
	}
	if calls == 0 {
		t.Error("nothing was decrypted, so the check proved nothing")
	}
}

// A fresh install has no encrypted credentials to check against. It must not
// be refused, and it must not be refused by accident either: the empty strings
// that COALESCE produces for rows without a password are not ciphertext.
func TestKeyMatchesStored_FreshInstallPasses(t *testing.T) {
	t.Parallel()
	for name, stored := range map[string][]string{
		"no rows at all":       nil,
		"rows with no secrets": {"", "", ""},
	} {
		if err := keyMatchesStored(stored, alwaysFails); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// Values that predate the current format are not evidence about the key: the
// v1 prefix is rejected by the decrypter before any key is involved, so
// treating that rejection as a passphrase mismatch would refuse to start a
// bridge whose passphrase is correct.
func TestKeyMatchesStored_UnencryptedValuesAreNotEvidence(t *testing.T) {
	t.Parallel()
	stored := []string{"v1:legacy-blob", "plaintext-password"}
	if err := keyMatchesStored(stored, alwaysWorks); err != nil {
		t.Errorf("values without the current prefix should be ignored, got: %v", err)
	}
	if err := keyMatchesStored(stored, alwaysFails); err != nil {
		t.Errorf("values without the current prefix should be ignored, got: %v", err)
	}
}
