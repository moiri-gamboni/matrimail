package email

import (
	"os"
	"strings"
	"testing"
)

// The proton_reply fixtures follow the layout Proton Mail's web client
// composes for a reply with no user signature and the Proton signature
// turned off: the typed text, an empty signature block, a spacer, then the
// quote wrapper holding the attribution line and the quoted parent.
func readFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const protonReplyText = `<div style="font-family: Arial, sans-serif; font-size: 14px;">reply 3</div>`

func TestDisplayBodies_ProtonReplyShowsOnlyTheNewText(t *testing.T) {
	text, html := displayBodies(readFixture(t, "proton_reply.txt"), readFixture(t, "proton_reply.html"), true)
	if text != "reply 3" {
		t.Errorf("text = %q, want %q", text, "reply 3")
	}
	if html != protonReplyText {
		t.Errorf("html = %q, want %q", html, protonReplyText)
	}
}

func TestDisplayBodies_NewProtonMessageDropsEmptySignature(t *testing.T) {
	in := protonReplyText + "\n" + `<div style="font-family: Arial, sans-serif; font-size: 14px;" class="protonmail_signature_block protonmail_signature_block-empty">
    <div class="protonmail_signature_block-user protonmail_signature_block-empty">

            </div>

            <div class="protonmail_signature_block-proton protonmail_signature_block-empty">

            </div>
</div>
`
	_, html := displayBodies("reply 3", in, false)
	if strings.TrimSpace(html) != protonReplyText {
		t.Errorf("html = %q, want %q", html, protonReplyText)
	}
}

func TestStripQuotedReplyHTML_ProtonMail(t *testing.T) {
	got := StripQuotedReplyHTML(readFixture(t, "proton_reply.html"))
	for _, leaked := range []string{"reply 2", "wrote:", "protonmail_quote", "blockquote"} {
		if strings.Contains(got, leaked) {
			t.Errorf("%q leaked into stripped HTML: %q", leaked, got)
		}
	}
	if !strings.Contains(got, "reply 3") {
		t.Errorf("new body lost: %q", got)
	}
	if strings.HasSuffix(got, "<br></div>") {
		t.Errorf("trailing spacer not trimmed: %q", got)
	}
}

func TestStripEmptyProtonSignature_KeepsUserSignature(t *testing.T) {
	in := `<div>Hello</div><div class="protonmail_signature_block">
    <div class="protonmail_signature_block-user">
        <div>Alex Example</div>
    </div>
    <div class="protonmail_signature_block-proton protonmail_signature_block-empty">
    </div>
</div>`
	got := stripEmptyProtonSignature(in)
	if !strings.Contains(got, "Alex Example") || !strings.Contains(got, `class="protonmail_signature_block-user"`) {
		t.Errorf("user signature lost: %q", got)
	}
	if strings.Contains(got, "protonmail_signature_block-empty") {
		t.Errorf("empty Proton signature kept: %q", got)
	}
}

// A blockquote the sender wrote into their own message is content, not
// history: without a client's quote marker it stays.
func TestStripQuotedReplyHTML_OwnBlockquoteKept(t *testing.T) {
	body := `<div>As the poem goes:</div><blockquote><div>So much depends upon a red wheel barrow</div></blockquote>`
	if got := StripQuotedReplyHTML(body); got != body {
		t.Errorf("own blockquote altered: %q", got)
	}
}

func TestStripQuotedReply_ProtonPlainText(t *testing.T) {
	if got := StripQuotedReply(readFixture(t, "proton_reply.txt")); got != "reply 3" {
		t.Errorf("got %q, want %q", got, "reply 3")
	}
}
