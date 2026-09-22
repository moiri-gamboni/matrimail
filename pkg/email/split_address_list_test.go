package email

import (
	"net/mail"
	"testing"
)

func parseOneAddressForTest(raw string) (*mail.Address, error) { return mail.ParseAddress(raw) }

// The Gmail API parser re-emitted a display name unquoted, and the unquoted
// form fails to re-parse on the send path, so contacts named like "Doe, John"
// (the Exchange default) were dropped from reply-all with no log.
func TestSplitAddressList_QuotesNamesContainingCommas(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		raw  string
		want []string
	}{
		{
			name: "comma in display name stays parseable",
			raw:  `"Doe, John" <john@example.com>, alice@example.com`,
			want: []string{`"Doe, John" <john@example.com>`, "alice@example.com"},
		},
		{
			name: "plain name needs no quoting",
			raw:  `John Doe <john@example.com>`,
			want: []string{`"John Doe" <john@example.com>`},
		},
		{
			name: "bare address passes through",
			raw:  `bob@example.com`,
			want: []string{"bob@example.com"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := splitAddressList(tc.raw)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d entries %v, want %d %v", len(got), got, len(tc.want), tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("entry %d = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// Every entry this produces is fed to net/mail.ParseAddress on the send path,
// which drops whatever it cannot parse. That is the actual contract.
func TestSplitAddressList_OutputRoundTrips(t *testing.T) {
	t.Parallel()
	raw := `"Doe, John" <john@example.com>, "O'Neil, Pat" <pat@example.org>, plain@example.com`
	for _, entry := range splitAddressList(raw) {
		if _, err := parseOneAddressForTest(entry); err != nil {
			t.Errorf("entry %q will be silently dropped from reply-all: %v", entry, err)
		}
	}
}
