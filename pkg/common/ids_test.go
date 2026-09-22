package common

import "testing"

// The network message ID is written by the inbound path, written again by the
// outbound path, and read back by the thread resolver. The resolver reports a
// lookup matching nothing as an ordinary miss, and it is right to: the first
// message of every new thread has never been bridged. That makes a divergence
// between the writing and the reading spelling completely silent, with the only
// symptom being conversations gradually opening second rooms.
//
// One constructor is what prevents it. These tests exist so that the constructor
// cannot be quietly bypassed or changed on one side.

func TestEmailToMessageID_RoundTrips(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		"CAB1234@mail.example.com",
		"<already-angled@example.com>",
		"weird:colons@example.com",
		"",
	} {
		got := MessageIDFromNetworkID(EmailToMessageID(raw))
		if got != raw {
			t.Errorf("round trip of %q gave %q", raw, got)
		}
	}
}

// A value stored before the prefix existed must still come back usable rather
// than losing leading characters.
func TestMessageIDFromNetworkID_ToleratesAnUnprefixedValue(t *testing.T) {
	t.Parallel()
	if got := MessageIDFromNetworkID("bare@example.com"); got != "bare@example.com" {
		t.Errorf("got %q, want the value unchanged", got)
	}
}

// The prefix is load-bearing rather than decorative: bridgev2 stores these in a
// table shared with every other ID this bridge mints, and the resolver's lookup
// is an exact match on the stored string.
func TestEmailToMessageID_AppliesTheNamespace(t *testing.T) {
	t.Parallel()
	if got := EmailToMessageID("a@example.com"); string(got) != "email:a@example.com" {
		t.Errorf("got %q, want email:a@example.com", got)
	}
}

// Surrounding whitespace is stripped on construction. A Message-ID arrives from
// a header and may carry it; the resolver trims its input before looking up, so
// the writer must trim too or the two disagree on exactly the messages whose
// headers are least well formed.
func TestEmailToMessageID_TrimsWhitespace(t *testing.T) {
	t.Parallel()
	spaced := EmailToMessageID("  a@example.com\t")
	clean := EmailToMessageID("a@example.com")
	if spaced != clean {
		t.Errorf("whitespace changed the ID: %q vs %q", spaced, clean)
	}
}
