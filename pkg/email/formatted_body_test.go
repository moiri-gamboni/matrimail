package email

import "testing"

// The room receives the email's HTML as formatted_body. Escaped text must
// stay escaped, or the client parses it as markup and drops it; invisible
// padding characters are removed whether written literally or as entities.
func TestMatrixFormattedBody(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{
			name: "attribution address",
			in:   `<div>On Thu, Sep 24, 2026 at 1:33 PM, Alex Example &lt;alex@example.com&gt; wrote:<br></div>`,
			want: `<div>On Thu, Sep 24, 2026 at 1:33 PM, Alex Example &lt;alex@example.com&gt; wrote:<br></div>`,
		},
		{
			name: "less-than in text",
			in:   `<p>if a &lt; b then <code>&lt;script&gt;</code> stays text</p>`,
			want: `<p>if a &lt; b then <code>&lt;script&gt;</code> stays text</p>`,
		},
		{
			name: "ampersand",
			in:   `<p>Tom &amp; Jerry &amp;amp; friends</p>`,
			want: `<p>Tom &amp; Jerry &amp;amp; friends</p>`,
		},
		{
			name: "escaped attribute",
			in:   `<img src="mxc://example.com/x" alt="a &quot;b&quot;">`,
			want: `<img src="mxc://example.com/x" alt="a &quot;b&quot;">`,
		},
		{
			name: "invisible padding as entities and literals",
			in:   "<div>Preview&#847;&zwnj;&#8203;&#x200B;​&nbsp;</div>",
			want: "<div>Preview&nbsp;</div>",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := matrixFormattedBody(c.in); got != c.want {
				t.Errorf("got  %q\nwant %q", got, c.want)
			}
		})
	}
}
