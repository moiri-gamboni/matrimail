package email

import (
	"strings"
	"testing"
	"time"
)

func TestStripQuotedReply_GmailAttribution(t *testing.T) {
	body := `Sounds good — let's go with option B.

Thanks!

On Mon, May 19, 2026 at 10:30 AM John Doe <john@example.com> wrote:

> Hey, just checking which option you prefer.
> A or B?
>
> Cheers,
> John`
	got := StripQuotedReply(body)
	want := "Sounds good — let's go with option B.\n\nThanks!"
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

func TestStripQuotedReply_GmailAttribution_French(t *testing.T) {
	body := `Dispo !

Le mar. 19 mai 2026, à 09 h 19, Antoine Weill-Duflos <antoine@weill-duflos.fr> a écrit :
> Hello !
>
> Avec ça on est bon ?
>
> Merci,
> Antoine`
	got := StripQuotedReply(body)
	want := "Dispo !"
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

func TestStripQuotedReply_GmailAttribution_OtherLocales(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{
			name: "german",
			body: "Klar, passt!\n\nAm Mo., 19. Mai 2026 um 10:30 schrieb John Doe <john@example.com>:\n> hi",
		},
		{
			name: "spanish",
			body: "Vale.\n\nEl lun, 19 may 2026 a las 10:30, John Doe <john@example.com> escribió:\n> hola",
		},
		{
			name: "italian",
			body: "Ok!\n\nIl lun 19 mag 2026, 10:30 John Doe <john@example.com> ha scritto:\n> ciao",
		},
		{
			name: "portuguese",
			body: "Beleza.\n\nEm seg., 19 de mai. de 2026 às 10:30, John Doe <john@example.com> escreveu:\n> oi",
		},
		{
			name: "dutch",
			body: "Prima.\n\nOp ma 19 mei 2026 om 10:30 schreef John Doe <john@example.com>:\n> hoi",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := StripQuotedReply(tc.body)
			if strings.Contains(got, "wrote") || strings.Contains(got, "écrit") ||
				strings.Contains(got, "schrieb") || strings.Contains(got, "escribió") ||
				strings.Contains(got, "ha scritto") || strings.Contains(got, "escreveu") ||
				strings.Contains(got, "schreef") {
				t.Errorf("attribution leaked into stripped body: %q", got)
			}
			if strings.HasPrefix(got, ">") || strings.Contains(got, "\n>") {
				t.Errorf("quote leaked into stripped body: %q", got)
			}
		})
	}
}

func TestStripQuotedReply_OutlookDivider(t *testing.T) {
	body := `Confirmed, I'll be there.

-----Original Message-----
From: Jane <jane@example.com>
Sent: Monday, May 19, 2026 9:00 AM
To: me@example.com
Subject: Meeting Friday?

Are you available Friday at 3?`
	got := StripQuotedReply(body)
	want := "Confirmed, I'll be there."
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

func TestStripQuotedReply_OutlookHeaderBlock(t *testing.T) {
	body := `Yes, that works.

From: Jane <jane@example.com>
Sent: Monday, May 19, 2026 9:00 AM
To: me@example.com
Subject: Meeting Friday?

Are you available Friday at 3?`
	got := StripQuotedReply(body)
	want := "Yes, that works."
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

func TestStripQuotedReply_TrailingQuoteOnly(t *testing.T) {
	body := `Approved.

> please confirm the budget for Q3
> — finance team`
	got := StripQuotedReply(body)
	want := "Approved."
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

func TestStripQuotedReply_NoQuotePreserved(t *testing.T) {
	body := `Just a normal email with no quote.

Multiple paragraphs.
--
Sig line should stay`
	got := StripQuotedReply(body)
	if !strings.HasPrefix(got, "Just a normal email") {
		t.Errorf("body unexpectedly mutated: %q", got)
	}
	if !strings.Contains(got, "Sig line should stay") {
		t.Errorf("signature was stripped: %q", got)
	}
}

func TestStripQuotedReply_EmptyAndWhitespace(t *testing.T) {
	if got := StripQuotedReply(""); got != "" {
		t.Errorf("empty input -> %q", got)
	}
	if got := StripQuotedReply("\n\n\n"); got != "" {
		t.Errorf("whitespace-only -> %q", got)
	}
}

func TestStripQuotedReplyHTML_GmailDiv(t *testing.T) {
	body := `<div dir="ltr">Dispo !</div><br><div class="gmail_quote gmail_quote_container"><div dir="ltr" class="gmail_attr">Le mar. 19 mai 2026, à 09 h 19, Antoine Weill-Duflos &lt;antoine@weill-duflos.fr&gt; a écrit :<br></div><blockquote class="gmail_quote" style="margin:0px 0px 0px 0.8ex"><div>Hello !</div></blockquote></div>`
	got := StripQuotedReplyHTML(body)
	if strings.Contains(got, "gmail_quote") {
		t.Errorf("gmail_quote leaked into stripped HTML: %q", got)
	}
	if strings.Contains(got, "a écrit") {
		t.Errorf("attribution leaked: %q", got)
	}
	if !strings.Contains(got, "Dispo !") {
		t.Errorf("new body lost: %q", got)
	}
	if strings.HasSuffix(got, "<br>") {
		t.Errorf("trailing <br> not trimmed: %q", got)
	}
}

func TestStripQuotedReplyHTML_GmailBlockquoteOnly(t *testing.T) {
	body := `<p>Ok</p><blockquote class="gmail_quote">previous content</blockquote>`
	got := StripQuotedReplyHTML(body)
	if strings.Contains(got, "previous content") {
		t.Errorf("quote leaked: %q", got)
	}
	if !strings.Contains(got, "<p>Ok</p>") {
		t.Errorf("new body lost: %q", got)
	}
}

func TestStripQuotedReplyHTML_AppleMail(t *testing.T) {
	body := `<div>Got it.</div><br><blockquote type="cite"><div>On May 19, John wrote:</div></blockquote>`
	got := StripQuotedReplyHTML(body)
	if strings.Contains(got, "blockquote") || strings.Contains(got, "John wrote") {
		t.Errorf("apple mail quote not stripped: %q", got)
	}
	if !strings.Contains(got, "Got it.") {
		t.Errorf("new body lost: %q", got)
	}
}

func TestStripQuotedReplyHTML_OutlookWeb(t *testing.T) {
	body := `<div>Confirmed.</div><div id="appendonsend"></div><hr><div><b>From:</b> Jane</div>`
	got := StripQuotedReplyHTML(body)
	if strings.Contains(got, "appendonsend") || strings.Contains(got, "From:") {
		t.Errorf("outlook quote not stripped: %q", got)
	}
	if !strings.Contains(got, "Confirmed.") {
		t.Errorf("new body lost: %q", got)
	}
}

func TestStripQuotedReplyHTML_NoQuotePreserved(t *testing.T) {
	body := `<div>Just a normal HTML email.</div><p>Multiple paragraphs.</p>`
	got := StripQuotedReplyHTML(body)
	if got != body {
		t.Errorf("body unexpectedly mutated: %q", got)
	}
}

func TestStripQuotedReplyHTML_EmptyAndWhitespace(t *testing.T) {
	if got := StripQuotedReplyHTML(""); got != "" {
		t.Errorf("empty input -> %q", got)
	}
}

func TestFormatGmailQuoteText_Basic(t *testing.T) {
	d := time.Date(2026, 5, 19, 10, 30, 0, 0, time.UTC)
	got := FormatGmailQuoteText(d, time.UTC, "John Doe <john@example.com>", "Hey,\n\nWhat's the plan?\nThanks,\nJohn")
	want := `On Tue, May 19, 2026 at 10:30 AM, John Doe <john@example.com> wrote:
> Hey,
>
> What's the plan?
> Thanks,
> John`
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestFormatGmailQuoteText_PreservesExistingQuoteDepth(t *testing.T) {
	d := time.Date(2026, 5, 19, 10, 30, 0, 0, time.UTC)
	parent := "My reply.\n\n> Previously quoted line"
	got := FormatGmailQuoteText(d, time.UTC, "jane@example.com", parent)
	if !strings.Contains(got, ">> Previously quoted line") {
		t.Errorf("nested quote depth not preserved: %q", got)
	}
}

func TestFormatGmailQuoteHTML_EmptyReturnsEmpty(t *testing.T) {
	got := FormatGmailQuoteHTML(time.Now(), time.UTC, "x@y.z", "")
	if got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestFormatGmailQuoteHTML_WrapsParent(t *testing.T) {
	d := time.Date(2026, 5, 19, 10, 30, 0, 0, time.UTC)
	got := FormatGmailQuoteHTML(d, time.UTC, "John <john@example.com>", "<p>Hello</p>")
	if !strings.Contains(got, `class="gmail_quote gmail_quote_container"`) {
		t.Errorf("missing gmail_quote container: %q", got)
	}
	if !strings.Contains(got, `class="gmail_attr"`) {
		t.Errorf("missing gmail_attr: %q", got)
	}
	if !strings.Contains(got, "<p>Hello</p>") {
		t.Errorf("parent HTML not embedded: %q", got)
	}
	if !strings.Contains(got, "John &lt;john@example.com&gt;") {
		t.Errorf("attribution not HTML-escaped: %q", got)
	}
}

func TestNormalizeReplySubject(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"Foo", "Re: Foo"},
		{"Re: Foo", "Re: Foo"},
		{"RE: Foo", "Re: Foo"},
		{"re: Foo", "Re: Foo"},
		{"Re: Re: Foo", "Re: Foo"},
		{"RE: re: RE: Foo", "Re: Foo"},
		{"Re[2]: Foo", "Re: Foo"},
		{"  Re:  Re: Foo  ", "Re: Foo"},
		{"", "Re: "},
	}
	for _, c := range cases {
		if got := NormalizeReplySubject(c.in); got != c.want {
			t.Errorf("NormalizeReplySubject(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// The parent's Date header usually arrives in UTC; the attribution line must
// read in the user's zone, as their own clock showed it.
func TestFormatGmailQuote_AttributionInGivenZone(t *testing.T) {
	lisbon, err := time.LoadLocation("Europe/Lisbon")
	if err != nil {
		t.Fatal(err)
	}
	d := time.Date(2026, 9, 24, 12, 34, 0, 0, time.UTC)
	want := "On Thu, Sep 24, 2026 at 1:34 PM, Alex Example"
	if got := FormatGmailQuoteText(d, lisbon, "Alex Example <alex@example.com>", "hi"); !strings.HasPrefix(got, want) {
		t.Errorf("text attribution = %q, want prefix %q", got, want)
	}
	if got := FormatGmailQuoteHTML(d, lisbon, "Alex Example <alex@example.com>", "<p>hi</p>"); !strings.Contains(got, want) {
		t.Errorf("html attribution = %q, want %q in it", got, want)
	}
}
