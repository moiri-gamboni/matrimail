package email

import "testing"

// Preview padding (runs of zero-width and joiner characters after a
// marketing mail's preview text) is removed; the same characters doing real
// work in text (combining marks, emoji and Persian joiners, bidi marks) stay.
func TestStripPreviewPadding(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"decomposed diaeresis", "Moi\u0308ri", "Moi\u0308ri"},
		{"decomposed acute", "cafe\u0301", "cafe\u0301"},
		{"family emoji", "\U0001F468\u200D\U0001F469\u200D\U0001F467", "\U0001F468\u200D\U0001F469\u200D\U0001F467"},
		{"emoji sequence after a variation selector", "\u2764\uFE0F\u200D\U0001F525", "\u2764\uFE0F\u200D\U0001F525"},
		{"persian zwnj", "\u0645\u06CC\u200C\u062E\u0648\u0627\u0647\u0645", "\u0645\u06CC\u200C\u062E\u0648\u0627\u0647\u0645"},
		{"right-to-left mark", "abc\u200Fdef", "abc\u200Fdef"},
		{
			"padding run",
			"Preview text\u200C\u00A0\u200C\u00A0\u034F\u200B\u2060\uFEFF\u00AD\u200C\u00A0",
			"Preview text\u00A0\u00A0\u00A0",
		},
		{"joiner at the edge of a word", "Hello\u200C \u200Dworld", "Hello world"},
	}
	for _, c := range cases {
		t.Run("text/"+c.name, func(t *testing.T) {
			if got := stripPreviewPadding(c.in, false); got != c.want {
				t.Errorf("got  %+q\nwant %+q", got, c.want)
			}
		})
		t.Run("html/"+c.name, func(t *testing.T) {
			in, want := "<div>"+c.in+"</div>", "<div>"+c.want+"</div>"
			if got := matrixFormattedBody(in); got != want {
				t.Errorf("got  %+q\nwant %+q", got, want)
			}
		})
	}
}

// In HTML the same characters often arrive as character references, and a
// joiner's neighbours are then references too.
func TestMatrixFormattedBody_InvisibleEntities(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"emoji as references", "<p>&#x1F468;&zwj;&#x1F469;</p>", "<p>&#x1F468;&zwj;&#x1F469;</p>"},
		{"persian zwnj as reference", "<p>\u0645\u06CC&zwnj;\u062E\u0648\u0627\u0647\u0645</p>", "<p>\u0645\u06CC&zwnj;\u062E\u0648\u0627\u0647\u0645</p>"},
		{"rlm as reference", "<p>abc&rlm;def</p>", "<p>abc&rlm;def</p>"},
		{"diaeresis as reference", "<p>Moi&#776;ri</p>", "<p>Moi&#776;ri</p>"},
		{
			"padding run as references",
			"<div>Preview text&zwnj;&nbsp;&zwnj;&nbsp;&#847;&#8203;&#x200B;&#8288;&#65279;&shy;&zwnj;&nbsp;</div>",
			"<div>Preview text&nbsp;&nbsp;&nbsp;</div>",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := matrixFormattedBody(c.in); got != c.want {
				t.Errorf("got  %+q\nwant %+q", got, c.want)
			}
		})
	}
}
