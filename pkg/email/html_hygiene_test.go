package email

import (
	"strings"
	"testing"
)

func TestIsLikelyDecorativeImage(t *testing.T) {
	cases := []struct {
		name  string
		label string
		size  int64
		want  bool
	}{
		{"tiny gif", "logo.gif", 512, true},
		{"big photo", "photo.jpg", 200 * 1024, false},
		{"spacer label", "spacer.png", 50 * 1024, true},
		{"tracking label", "tracking-pixel.gif", 50 * 1024, true},
		{"1x1 label", "img-1x1.gif", 50 * 1024, true},
		{"normal screenshot", "screenshot.png", 80 * 1024, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isLikelyDecorativeImage(c.label, c.size); got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestIsLikelyMarketingHTML(t *testing.T) {
	// Plain personal email — not marketing.
	personalHTML := `<html><body><p>Hi Antoine,</p><p>` +
		strings.Repeat("Just wanted to say thanks for the meeting yesterday. ", 30) +
		`</p></body></html>`
	if IsLikelyMarketingHTML(personalHTML, "Hi Antoine, just a quick thanks for the meeting yesterday.") {
		t.Errorf("personal email incorrectly classified as marketing")
	}

	// Small transactional HTML — not marketing (under size threshold).
	tiny := `<html><body><table><tr><td>Order #1234 confirmed.</td></tr></table></body></html>`
	if IsLikelyMarketingHTML(tiny, "Order #1234 confirmed.") {
		t.Errorf("small email incorrectly classified as marketing")
	}

	// Marketing-style: many tables + backgrounds + little text.
	var b strings.Builder
	b.WriteString(`<html><body>`)
	for i := 0; i < 12; i++ {
		b.WriteString(`<table cellpadding="0"><tr><td style="background-image: url(cid:bg` +
			string(rune('a'+i)) + `); width:600px"><a href="#">Click</a></td></tr></table>`)
	}
	b.WriteString(strings.Repeat(" ", 16*1024))
	b.WriteString(`</body></html>`)
	marketing := b.String()
	if !IsLikelyMarketingHTML(marketing, "Click Click Click") {
		t.Errorf("marketing email not detected; len=%d", len(marketing))
	}

	// Image-heavy newsletter (no backgrounds, lots of <img>) — still marketing.
	var nl strings.Builder
	nl.WriteString(`<html><body>`)
	for i := 0; i < 20; i++ {
		nl.WriteString(`<img src="cid:hero` + string(rune('a'+i%26)) + `" alt="">`)
	}
	nl.WriteString(strings.Repeat("x", 16*1024))
	nl.WriteString(`</body></html>`)
	if !IsLikelyMarketingHTML(nl.String(), "Read more") {
		t.Errorf("image-heavy newsletter not detected")
	}

	// Long-text marketing email (e.g., embedded blog post) — preserve HTML.
	longText := strings.Repeat("This is a long-form newsletter with real content. ", 50)
	if IsLikelyMarketingHTML(marketing, longText) {
		t.Errorf("long-text newsletter incorrectly classified as marketing (should preserve HTML)")
	}
}

func TestBackgroundImageCIDs(t *testing.T) {
	cases := []struct {
		name, in string
		want     []string
	}{
		{"css", `<table><tr><td style="background-image: url(cid:Hero); padding: 10px">Click here</td></tr></table>`, []string{"hero"}},
		{"outlook attribute", `<table background="cid:bgimg"><tr><td>Hello</td></tr></table>`, []string{"bgimg"}},
		{"remote background", `<td style="background-image: url(https://example.com/bg.png)">x</td>`, nil},
		{"plain html", `<p>Hello, <strong>world</strong>!</p>`, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := backgroundImageCIDs(c.in); strings.Join(got, ",") != strings.Join(c.want, ",") {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}
