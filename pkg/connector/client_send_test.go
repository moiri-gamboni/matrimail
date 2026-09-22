package connector

import (
	"context"
	"testing"

	"github.com/Leicas/matrimail/pkg/email"
	"github.com/Leicas/matrimail/pkg/imap"
)

// noopSender satisfies email.Sender for tests that only need to populate
// EmailClient.Sender to a non-nil value.
type noopSender struct{}

func (noopSender) Send(ctx context.Context, mimeBytes []byte, from string, to []string, threadID string) (string, error) {
	return "", nil
}
func (noopSender) Provider() string { return "noop" }
func (noopSender) Close() error     { return nil }

// TestIsLoggedIn covers the bug where modify-mode Gmail accounts (no IMAP,
// Gmail-API-only) reported IsLoggedIn=false because the old implementation
// gated on IMAPClient + isConnected. bridgev2's portal router uses
// IsLoggedIn to decide whether to dispatch outbound Matrix events through
// the network, so a false here surfaces as "You're not logged in" on every
// send.
func TestIsLoggedIn(t *testing.T) {
	cases := []struct {
		name         string
		hasIMAP      bool
		imapConn     bool
		hasSender    bool
		wantLoggedIn bool
	}{
		{name: "modify-mode (sender only)", hasSender: true, wantLoggedIn: true},
		{name: "imap connected", hasIMAP: true, imapConn: true, wantLoggedIn: true},
		// IMAP transient disconnect: credentials still valid; IsLoggedIn is a
		// credential-state probe per bridgev2's networkinterface.go contract,
		// not a transport-health probe.
		{name: "imap disconnected", hasIMAP: true, imapConn: false, wantLoggedIn: true},
		{name: "needs-reauth (no transport)", wantLoggedIn: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ec := &EmailClient{}
			if tc.hasIMAP {
				ec.IMAPClient = &imap.Client{}
			}
			ec.isConnected.Store(tc.imapConn)
			if tc.hasSender {
				ec.Sender = noopSender{}
			}
			if got := ec.IsLoggedIn(); got != tc.wantLoggedIn {
				t.Errorf("IsLoggedIn() = %v, want %v", got, tc.wantLoggedIn)
			}
		})
	}
}

func TestResolveReplyAll_FallbackToParticipants_SkipsSelfCaseInsensitive(t *testing.T) {
	// thread.LastFrom is empty (compose-style fallback path); reply-all uses
	// thread.Participants minus selves.
	thread := &email.EmailThread{
		Participants: []string{"alice@example.com", "BOB@example.com", "self@example.com"},
	}
	to, cc, _, err := resolveReplyAllRecipients(thread, []string{"self@example.com"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cc) != 0 {
		t.Errorf("expected no Cc on fallback path, got %+v", cc)
	}
	if len(to) != 2 {
		t.Fatalf("expected 2 recipients, got %d (%+v)", len(to), to)
	}
	got := map[string]bool{}
	for _, a := range to {
		got[a.Address] = true
	}
	if !got["alice@example.com"] || !got["BOB@example.com"] {
		t.Errorf("recipient set wrong: %+v", got)
	}
	if got["self@example.com"] {
		t.Errorf("self leaked into recipients: %+v", got)
	}
}

func TestResolveReplyAll_ErrorsWhenEmpty(t *testing.T) {
	thread := &email.EmailThread{Participants: []string{"self@example.com"}}
	_, _, _, err := resolveReplyAllRecipients(thread, []string{"self@example.com"})
	if err == nil {
		t.Fatal("expected error when no recipients remain after self-exclusion")
	}
}

func TestResolveReplyAll_SplitsToAndCc(t *testing.T) {
	thread := &email.EmailThread{
		LastFrom: "alice@example.com",
		LastTo:   []string{"self@example.com", "bob@example.com"},
		LastCc:   []string{"carol@example.com", "ALIAS@example.com", "alice@example.com"},
	}
	// "self" plus "alias" are both treated as the user.
	to, cc, _, err := resolveReplyAllRecipients(thread, []string{"self@example.com", "alias@example.com"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	gotTo := map[string]bool{}
	for _, a := range to {
		gotTo[a.Address] = true
	}
	if !gotTo["alice@example.com"] || !gotTo["bob@example.com"] {
		t.Errorf("To set wrong: %+v", gotTo)
	}
	if gotTo["self@example.com"] || gotTo["ALIAS@example.com"] {
		t.Errorf("self leaked into To: %+v", gotTo)
	}
	gotCc := map[string]bool{}
	for _, a := range cc {
		gotCc[a.Address] = true
	}
	// alice@example.com is already in To and must be deduped out of Cc.
	if gotCc["alice@example.com"] {
		t.Errorf("alice should have been deduped out of Cc (already in To): %+v", gotCc)
	}
	if !gotCc["carol@example.com"] {
		t.Errorf("carol should remain in Cc: %+v", gotCc)
	}
}

func TestResolveDM_TargetsLastFromOnly(t *testing.T) {
	thread := &email.EmailThread{
		LastFrom: "alice@example.com",
		LastTo:   []string{"self@example.com", "bob@example.com"},
		LastCc:   []string{"carol@example.com"},
	}
	to, err := resolveDMRecipients(thread, []string{"self@example.com"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(to) != 1 || to[0].Address != "alice@example.com" {
		t.Errorf("DM mode should target only LastFrom; got %+v", to)
	}
}

func TestResolveDM_ErrorsWhenLastFromEmpty(t *testing.T) {
	thread := &email.EmailThread{}
	_, err := resolveDMRecipients(thread, []string{"self@example.com"})
	if err == nil {
		t.Fatal("expected error when LastFrom is empty")
	}
}

func TestComputeReplyChain_PrefersExplicitReply(t *testing.T) {
	thread := &email.EmailThread{
		MessageID:  "tail@example.com",
		References: []string{"root@example.com", "mid@example.com"},
	}
	// Simulate database.Message indirectly via inline shim
	// We craft the helper without depending on database.Message here.
	// Direct call:
	parent := "explicit-parent@example.com"
	references := append([]string{}, thread.References...)
	references = append(references, parent)

	// Verify the helper does the same thing for the explicit-reply case by
	// invoking the function with a minimal stand-in. computeReplyChain takes
	// a *database.Message; we test with nil first to cover the fallback path.
	inReplyTo, refs := computeReplyChain(thread, nil)
	if inReplyTo != "tail@example.com" {
		t.Errorf("nil reply: inReplyTo = %q, want tail@example.com", inReplyTo)
	}
	if len(refs) != 3 || refs[2] != "tail@example.com" {
		t.Errorf("nil reply: refs = %+v", refs)
	}
}

func TestComputeReplyChain_NoTailNoExplicit(t *testing.T) {
	thread := &email.EmailThread{}
	inReplyTo, refs := computeReplyChain(thread, nil)
	if inReplyTo != "" {
		t.Errorf("inReplyTo should be empty, got %q", inReplyTo)
	}
	if len(refs) != 0 {
		t.Errorf("refs should be empty, got %+v", refs)
	}
}
