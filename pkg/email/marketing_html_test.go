package email

import (
	"context"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"golang.org/x/net/html"
)

func mustTidy(t *testing.T, in string) string {
	t.Helper()
	out, err := tidyEmailHTML(in)
	if err != nil {
		t.Fatalf("tidyEmailHTML: %v", err)
	}
	return out
}

// assertInOrder fails unless each want appears in s, each after the previous.
func assertInOrder(t *testing.T, s string, wants ...string) {
	t.Helper()
	at := 0
	for _, w := range wants {
		i := strings.Index(s[at:], w)
		if i < 0 {
			t.Fatalf("%q missing (or out of order) in\n%s", w, s)
		}
		at += i + len(w)
	}
}

// Preheaders are written into the HTML and hidden with inline styles; the
// chat shows HTML without styles, so the bridge has to drop them.
func TestTidyEmailHTML_DropsHiddenElements(t *testing.T) {
	cases := map[string]string{
		"display none":               `<div style="display:none">Preheader</div>`,
		"display none important":     `<div style="DISPLAY: None !important; color:#fff">Preheader</div>`,
		"visibility hidden":          `<span style="visibility:hidden">Preheader</span>`,
		"opacity zero":               `<div style="opacity: 0.0">Preheader</div>`,
		"max-height zero, overflow":  `<div style="max-height:0px; overflow:hidden">Preheader</div>`,
		"height zero, overflow":      `<div style="overflow: hidden; height: 0">Preheader</div>`,
		"max-width zero, overflow":   `<div style="max-width:0;overflow:hidden">Preheader</div>`,
		"zero font size":             `<div style="font-size:0px; line-height:0">Preheader</div>`,
		"hidden attribute":           `<div hidden>Preheader</div>`,
		"litmus preheader":           `<span class="preheader" style="display:none !important; visibility:hidden; mso-hide:all; font-size:1px; color:#f4f4f4; line-height:1px; max-height:0px; max-width:0px; opacity:0; overflow:hidden;">Preheader</span>`,
		"nested inside visible cell": `<table><tr><td><div style="display:none">Preheader</div></td></tr></table>`,
		"hidden image":               `<img src="mxc://example.com/p" alt="Preheader" style="display:none">`,
	}
	for name, hidden := range cases {
		t.Run(name, func(t *testing.T) {
			out := mustTidy(t, `<p>Before</p>`+hidden+`<p>After</p>`)
			if strings.Contains(out, "Preheader") {
				t.Errorf("hidden text kept:\n%s", out)
			}
			assertInOrder(t, out, "Before", "After")
		})
	}
}

// Only styles that hide an element in every client count. mso-hide:all hides
// only from Outlook, and a zero font size whose descendants set their own
// size is how MJML lays out columns.
func TestTidyEmailHTML_KeepsVisibleStyledElements(t *testing.T) {
	cases := map[string]string{
		"overflow hidden alone":        `<div style="overflow:hidden">Visible</div>`,
		"max-height zero alone":        `<div style="max-height:0">Visible</div>`,
		"bounded height with overflow": `<div style="max-height:200px; overflow:hidden">Visible</div>`,
		"mso-hide alone":               `<div style="mso-hide:all">Visible</div>`,
		"half opacity":                 `<div style="opacity:0.5">Visible</div>`,
		"display block":                `<div style="display:block">Visible</div>`,
		"mjml column":                  `<table><tr><td style="font-size:0px;padding:10px 25px;"><div style="font-family:Arial;font-size:13px;line-height:1;">Visible</div></td></tr></table>`,
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if out := mustTidy(t, in); !strings.Contains(out, "Visible") {
				t.Errorf("visible text dropped:\n%s", out)
			}
		})
	}
}

// Text is not all a zero font size hides: images inside such a container
// (MJML's image cells) still show.
func TestTidyEmailHTML_ZeroFontKeepsImages(t *testing.T) {
	in := `<table><tr><td style="font-size:0px"><table><tr><td><img src="mxc://example.com/a" alt=""></td></tr></table></td></tr></table>`
	if out := mustTidy(t, in); !strings.Contains(out, "mxc://example.com/a") {
		t.Errorf("image dropped:\n%s", out)
	}
}

// Preview padding leaves a block of spaces behind once the invisible
// characters are gone; spacer paragraphs hold only a non-breaking space.
func TestTidyEmailHTML_DropsWhitespaceOnlyBlocks(t *testing.T) {
	in := `<p>Before</p>` +
		`<div>&#8199;&#847; &#8199;&#847; &#8199;&#847; &zwnj;&nbsp;&zwnj;&nbsp;   ` + "\u2007\u034f" + `</div>` +
		`<p>&nbsp;</p><div>   </div><h2> </h2>` +
		`<p>After</p>`
	out := mustTidy(t, in)
	want := `<p>Before</p><p>After</p>`
	if out != want {
		t.Errorf("got  %q\nwant %q", out, want)
	}
}

// A composed blank line (Gmail, Apple Mail and Outlook on the web write
// <div><br></div>) separates paragraphs and stays; a stack of them is one.
func TestTidyEmailHTML_BlankLines(t *testing.T) {
	single := `<div>One</div><div><br/></div><div>Two</div>`
	if out := mustTidy(t, single); out != single {
		t.Errorf("single blank line: got %q", out)
	}
	stack := `<div>One</div><div><br></div>` + "\n" + `<div><br></div><div><br></div><div>Two</div>`
	if out, want := mustTidy(t, stack), `<div>One</div><div><br/></div>`+"\n"+`<div>Two</div>`; out != want {
		t.Errorf("stacked blank lines: got %q\nwant %q", out, want)
	}
}

// Layout tables, where every row has at most one cell with content, become
// their cells' content in order.
func TestTidyEmailHTML_UnwrapsLayoutTables(t *testing.T) {
	in := `<table width="100%"><tr><td align="center">` +
		`<table><tr><td><h1>Title</h1></td></tr>` +
		`<tr><td height="24" style="font-size:24px">&nbsp;</td></tr>` +
		`<tr><td>Plain cell text</td></tr>` +
		`<tr><td><br></td></tr>` +
		`<tr><td>&nbsp;</td><td><p>Second <b>para</b></p></td></tr>` +
		`</table></td></tr></table>`
	out := mustTidy(t, in)
	if strings.Contains(out, "<table") || strings.Contains(out, "<td") || strings.Contains(out, "<tr") {
		t.Errorf("layout table kept:\n%s", out)
	}
	if strings.Contains(out, "\u00a0") || strings.Contains(out, "&nbsp;") || strings.Contains(out, "<br") {
		t.Errorf("spacer content kept:\n%s", out)
	}
	assertInOrder(t, out, "<h1>Title</h1>", "<div>Plain cell text</div>", "<p>Second <b>para</b></p>")
}

// A layout table that held only a line break leaves no blank block behind.
func TestTidyEmailHTML_FlattenedBreakLeavesNoBlankBlock(t *testing.T) {
	in := `<p>One</p><div><table><tr><td><br></td></tr></table></div><p>Two</p>`
	if out, want := mustTidy(t, in), `<p>One</p><p>Two</p>`; out != want {
		t.Errorf("got  %q\nwant %q", out, want)
	}
}

// A table with two cells of content in a row carries data; it stays a table
// and only loses its empty spacer rows.
func TestTidyEmailHTML_KeepsDataTables(t *testing.T) {
	in := `<table><tr><th>Item</th><th>Price</th></tr>` +
		`<tr><td>Team plan</td><td>$40</td></tr>` +
		`<tr><td>&nbsp;</td><td>&nbsp;</td></tr>` +
		`<tr><td>Total</td><td></td></tr></table>`
	out := mustTidy(t, in)
	want := `<table><tbody><tr><th>Item</th><th>Price</th></tr>` +
		`<tr><td>Team plan</td><td>$40</td></tr>` +
		`<tr><td>Total</td><td></td></tr></tbody></table>`
	if out != want {
		t.Errorf("got  %q\nwant %q", out, want)
	}
}

// A link whose only content was an image the bridge dropped is an empty
// link; links with text or a kept image stay.
func TestTidyEmailHTML_DropsEmptyLinks(t *testing.T) {
	in := `<p><a href="https://example.com/logo">  </a>Welcome</p>` +
		`<p>Read the <a href="https://example.com/docs">docs</a>.</p>` +
		`<p><a href="https://example.com/photo"><img src="mxc://example.com/abc" alt=""></a></p>`
	out := mustTidy(t, in)
	if strings.Contains(out, "https://example.com/logo") {
		t.Errorf("empty link kept:\n%s", out)
	}
	assertInOrder(t, out, `<a href="https://example.com/docs">docs</a>`, `<a href="https://example.com/photo"><img src="mxc://example.com/abc" alt=""/></a>`)
}

// Everything that carries meaning passes through: headings, emphasis,
// lists, quotes, links, and text that is escaped because it looks like
// markup. Apostrophes stay as written rather than becoming references.
func TestTidyEmailHTML_KeepsContent(t *testing.T) {
	in := `<h2>We're live</h2><p><b>bold</b> <i>it</i> a &lt; b &amp; c, Alex &lt;alex@example.com&gt;</p>` +
		`<ul><li>one</li><li>two</li></ul><blockquote>quoted</blockquote><p><a href="https://example.com/">link</a></p>`
	out := mustTidy(t, in)
	if out != in {
		t.Errorf("got  %q\nwant %q", out, in)
	}
}

// Comments (Outlook conditional blocks among them), styles and the
// document head never render in the chat.
func TestTidyEmailHTML_DropsNonRenderedMarkup(t *testing.T) {
	in := `<!DOCTYPE html><html><head><title>Subject</title><style>p{color:red}</style></head>` +
		`<body><!--[if mso]><table><tr><td><![endif]--><p>Text</p><style>.x{}</style><!--[if mso]></td></tr></table><![endif]--></body></html>`
	if out, want := mustTidy(t, in), `<p>Text</p>`; out != want {
		t.Errorf("got  %q\nwant %q", out, want)
	}
}

// The parser refuses HTML nested deeper than 512 elements; the caller keeps
// the HTML as it was.
func TestTidyEmailHTML_TooDeepIsAnError(t *testing.T) {
	in := strings.Repeat("<div>", 600) + "x" + strings.Repeat("</div>", 600)
	if _, err := tidyEmailHTML(in); err == nil {
		t.Fatal("want an error for HTML nested past the parser's limit")
	}
}

// The parser merges runs of text in quadratic time (text among table rows,
// text between ignored tags), so input past the size bulk mail keeps under
// is not parsed at all.
func TestTidyEmailHTML_TooLargeIsAnError(t *testing.T) {
	in := "<table>" + strings.Repeat("x<tr>", 200000) + "</table>"
	start := time.Now()
	if _, err := tidyEmailHTML(in); err == nil {
		t.Error("want an error for a body past the size limit")
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("refusing took %v", d)
	}
}

// Crafted input must not make the tidying pass superlinear: nesting as deep
// as the parser allows, and tens of thousands of spacer rows and blank lines.
// Parsing is left out of the timing; the size limit bounds it.
func TestTidyBody_LinearOnCraftedInput(t *testing.T) {
	inputs := map[string]func(n int) string{
		"nested layout tables": func(n int) string {
			var b strings.Builder
			for i := 0; i < n; i++ {
				// 126 levels of table/tbody/tr/td stay under the parser's
				// 512-element limit; repeat the tower n/126 times.
				depth := 126
				b.WriteString(strings.Repeat(`<table><tr><td style="font-size:0">`, depth))
				b.WriteString(`<a href="https://example.com/"> </a>x`)
				b.WriteString(strings.Repeat(`</td></tr></table>`, depth))
				i += depth - 1
			}
			return b.String()
		},
		"spacer rows": func(n int) string {
			return "<table>" + strings.Repeat("<tr><td>&nbsp;</td></tr><tr><td>x</td></tr>", n/2) + "</table>"
		},
		"blank lines": func(n int) string {
			return strings.Repeat("<div><br></div>\n", n) + "x"
		},
		"hidden and padding": func(n int) string {
			return strings.Repeat(`<div style="display:none">p</div><div>&#8199;&#847; </div>`, n/2) + "x"
		},
	}
	for name, gen := range inputs {
		t.Run(name, func(t *testing.T) {
			// Tidy eight small documents and one eight times the size: the
			// same work if the pass is linear, eight times as much if it is
			// quadratic.
			small := timeTidyBody(t, gen(2500), 8)
			large := timeTidyBody(t, gen(20000), 1)
			t.Logf("8 small: %v; 1 large: %v", small, large)
			if ratio := float64(large) / float64(small); ratio > 4 {
				t.Errorf("one 8x input took %.1fx the time of eight small ones (%v vs %v)", ratio, large, small)
			}
		})
	}
}

// timeTidyBody parses copies documents from in and returns the best of three
// timings of tidying them all; parsing is not timed.
func timeTidyBody(t *testing.T, in string, copies int) time.Duration {
	t.Helper()
	best := time.Duration(1<<63 - 1)
	for round := 0; round < 3; round++ {
		bodies := make([]*html.Node, copies)
		for i := range bodies {
			doc, err := html.Parse(strings.NewReader(in))
			if err != nil {
				t.Fatal(err)
			}
			bodies[i] = findBody(doc)
		}
		start := time.Now()
		for _, body := range bodies {
			tidyBody(body)
		}
		if d := time.Since(start); d < best {
			best = d
		}
	}
	return best
}

// The plain-text body derived from HTML is what chat lists preview, so the
// hidden preheader must not lead it.
func TestSimpleHTMLToText_DropsHiddenPreheader(t *testing.T) {
	in := `<html><body><div style="display:none;max-height:0;overflow:hidden">Preheader text</div>` +
		`<table><tr><td><p>Hello,</p></td></tr></table></body></html>`
	if got := simpleHTMLToText(in); got != "Hello," {
		t.Errorf("got %q, want %q", got, "Hello,")
	}
}

// Flattening a table must not run the text of neighbouring cells together.
func TestSimpleHTMLToText_SeparatesFlattenedCells(t *testing.T) {
	in := `<table><tr><td>Hello there,<br><br>Some text</td></tr></table><table><tr><td>Next block</td></tr></table>`
	if got, want := simpleHTMLToText(in), "Hello there, Some text Next block"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// End to end through message conversion: an ESP newsletter (preheader,
// padding, nested layout tables, image-only links, spacer rows) arrives in
// the room as its readable content.
func TestConvertMessage_ESPNewsletterIsReadable(t *testing.T) {
	raw, err := os.ReadFile("testdata/esp_newsletter.html")
	if err != nil {
		t.Fatal(err)
	}
	htmlBody := string(raw)
	log := zerolog.Nop()
	ev := &EmailMatrixEvent{
		emailMessage: &EmailMessage{
			ParsedEmail: &ParsedEmail{HTMLContent: htmlBody, TextContent: simpleHTMLToText(htmlBody)},
			Thread:      &EmailThread{},
		},
		processor: NewProcessor(&log, nil, false, ""),
	}
	cm, err := ev.ConvertMessage(context.Background(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var body, formatted string
	for _, p := range cm.Parts {
		if p.ID == "body" {
			body, formatted = p.Content.Body, p.Content.FormattedBody
		}
	}
	if formatted == "" {
		t.Fatalf("no formatted body; parts: %+v", cm.Parts)
	}

	for _, s := range []string{body, formatted} {
		if strings.Contains(s, "nothing to do") {
			t.Errorf("preheader shown:\n%s", s)
		}
	}
	if !strings.HasPrefix(strings.TrimSpace(body), "Your plan renews on October") {
		t.Errorf("text body does not start with the heading:\n%s", body)
	}
	if n := strings.Count(formatted, "<table"); n != 1 {
		t.Errorf("want only the price table, got %d tables:\n%s", n, formatted)
	}
	if regexp.MustCompile(`<a\b[^>]*>\s*</a>`).MatchString(formatted) {
		t.Errorf("empty link kept:\n%s", formatted)
	}
	if regexp.MustCompile(`<(div|p|td)\b[^>]*>[\s\x{00a0}\x{2007}]*</(div|p|td)>`).MatchString(formatted) {
		t.Errorf("whitespace-only block kept:\n%s", formatted)
	}
	assertInOrder(t, formatted,
		">Your plan renews on October\u00a01</h1>",
		">Hello,</p>",
		"<strong>Team plan</strong>",
		"&lt;billing@example.com&gt;",
		`<th align="left">Item</th><th align="right">Price</th>`,
		"<li>Seats: 4</li>",
		`<a href="https://example.com/changelog" target="_blank">changelog</a>`,
		"The Example Team",
		"Example Inc",
		`<a href="https://example.com/unsubscribe">Unsubscribe</a>`,
	)
	if len(formatted) > len(htmlBody)/2 {
		t.Errorf("formatted body is %d bytes of %d; layout not removed", len(formatted), len(htmlBody))
	}
}

// HTML the parser refuses still reaches the room, as written.
func TestConvertMessage_UntidyableHTMLSentAsWritten(t *testing.T) {
	htmlBody := strings.Repeat("<div>", 600) + "Deep text" + strings.Repeat("</div>", 600)
	log := zerolog.Nop()
	ev := &EmailMatrixEvent{
		emailMessage: &EmailMessage{
			ParsedEmail: &ParsedEmail{HTMLContent: htmlBody, TextContent: "Deep text"},
			Thread:      &EmailThread{},
		},
		processor: NewProcessor(&log, nil, false, ""),
	}
	cm, err := ev.ConvertMessage(context.Background(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range cm.Parts {
		if p.ID == "body" {
			if p.Content.FormattedBody != htmlBody {
				t.Errorf("formatted body changed: %d bytes, want the %d written", len(p.Content.FormattedBody), len(htmlBody))
			}
			return
		}
	}
	t.Fatalf("no body part: %+v", cm.Parts)
}
