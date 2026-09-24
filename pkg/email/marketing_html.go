package email

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// tidyEmailHTML reduces an email's HTML to what reads well in a chat, where
// the client drops inline styles and layout attributes. Bulk mail relies on
// both: a preview line hidden with CSS, tables nested ten deep for
// positioning, spacer rows and paragraphs, and image-only links whose images
// the bridge does not show. Without the styles those become a visible
// preview line, blank screens of padding and deep indentation. This removes
// what a mail client would not show and flattens layout-only tables; content
// and its formatting are kept. The result is the body's content, without the
// document head.
//
// It fails for HTML over maxTidyBytes and when the parser refuses the input,
// which happens for HTML nested deeper than 512 elements.
func tidyEmailHTML(htmlBody string) (string, error) {
	if len(htmlBody) > maxTidyBytes {
		return "", fmt.Errorf("html: %d bytes is over the %d-byte tidying limit", len(htmlBody), maxTidyBytes)
	}
	doc, err := html.Parse(strings.NewReader(htmlBody))
	if err != nil {
		return "", err
	}
	body := findBody(doc)
	if body == nil {
		return "", errors.New("html: document has no body")
	}
	tidyBody(body)
	var b strings.Builder
	for c := body.FirstChild; c != nil; c = c.NextSibling {
		if err := html.Render(&b, c); err != nil {
			return "", err
		}
	}
	// The renderer writes every apostrophe as &#39;. Attribute values are
	// double-quoted, so a literal apostrophe is valid everywhere and keeps the
	// HTML readable to anything that reads the room's source.
	return strings.ReplaceAll(b.String(), "&#39;", "'"), nil
}

// maxTidyBytes bounds the input tidyEmailHTML parses. The parser joins
// adjacent runs of text by concatenation, which crafted input (text between
// table rows or ignored tags) turns quadratic: about 0.5 s at this size and
// 2 s at twice it. Gmail clips messages past about 100 KB, so bulk mail stays
// under it.
const maxTidyBytes = 128 << 10

// findBody returns the body element, which the parser places under the html
// element. A frameset document has none.
func findBody(doc *html.Node) *html.Node {
	for top := doc.FirstChild; top != nil; top = top.NextSibling {
		if top.DataAtom != atom.Html {
			continue
		}
		for c := top.FirstChild; c != nil; c = c.NextSibling {
			if c.DataAtom == atom.Body {
				return c
			}
		}
	}
	return nil
}

// tidyBody applies the rules to the body's subtree in place.
func tidyBody(body *html.Node) {
	t := &htmlTidier{cellHasContent: map[*html.Node]bool{}}
	t.tidyChildren(body)
}

// subtreeContent summarises what a subtree shows once tidied. Every decision
// is made from the children's summaries, so the pass visits each node once.
type subtreeContent struct {
	visible  bool // text other than whitespace and preview padding, or an image or rule
	hasBreak bool // a <br>
	setsFont bool // an inline style declaring a font size other than zero
	hasImage bool
}

func (s *subtreeContent) add(o subtreeContent) {
	s.visible = s.visible || o.visible
	s.hasBreak = s.hasBreak || o.hasBreak
	s.setsFont = s.setsFont || o.setsFont
	s.hasImage = s.hasImage || o.hasImage
}

type htmlTidier struct {
	// cellHasContent records, per table cell, whether it shows anything, for
	// the table the cell belongs to.
	cellHasContent map[*html.Node]bool
}

// tidyChildren tidies n's children and summarises what they show. Of a run
// of adjacent blank lines only the first stays.
func (t *htmlTidier) tidyChildren(n *html.Node) subtreeContent {
	var sum subtreeContent
	prevBlankLine := false
	for c := n.FirstChild; c != nil; {
		next := c.NextSibling
		switch c.Type {
		case html.CommentNode:
			// Includes Outlook's conditional blocks, which only Outlook reads.
			n.RemoveChild(c)
		case html.TextNode:
			if hasVisibleText(c.Data) {
				sum.visible = true
				prevBlankLine = false
			}
		case html.ElementNode:
			cs, blankLine, kept := t.tidyElement(c)
			sum.add(cs)
			switch {
			case blankLine && prevBlankLine:
				n.RemoveChild(c)
			case kept:
				prevBlankLine = blankLine
			}
		}
		c = next
	}
	return sum
}

// tidyElement tidies n, removing it or replacing it with its content where
// the rules say so. blankLine reports a kept block that shows only line
// breaks; kept is false when n left the tree.
func (t *htmlTidier) tidyElement(n *html.Node) (sum subtreeContent, blankLine, kept bool) {
	styleAttr, _ := attr(n, "style")
	style := parseInlineStyle(styleAttr)
	_, hiddenAttr := attr(n, "hidden")
	if n.DataAtom == atom.Style || n.DataAtom == atom.Script || style.hidden || hiddenAttr {
		n.Parent.RemoveChild(n)
		return subtreeContent{}, false, false
	}
	switch n.DataAtom {
	case atom.Br:
		return subtreeContent{hasBreak: true}, false, true
	case atom.Img:
		return subtreeContent{visible: true, hasImage: true}, false, true
	case atom.Hr:
		return subtreeContent{visible: true}, false, true
	}
	sum = t.tidyChildren(n)
	// A zero font size hides text only where nothing inside sets a size
	// again; column layouts use it on containers to remove the gaps between
	// inline blocks. Images show whatever the font size.
	if style.zeroFont && !sum.setsFont && !sum.hasImage {
		n.Parent.RemoveChild(n)
		return subtreeContent{}, false, false
	}
	sum.setsFont = sum.setsFont || style.setsFont

	switch {
	case n.DataAtom == atom.Td || n.DataAtom == atom.Th:
		t.cellHasContent[n] = sum.visible
	case n.DataAtom == atom.Table:
		// A break never outlives the table as a blank line: rows that show
		// nothing are dropped, and a kept table is itself visible.
		sum.hasBreak = false
		return sum, false, !t.flattenTable(n)
	case n.DataAtom == atom.A && !sum.visible:
		n.Parent.RemoveChild(n)
		return sum, false, false
	case blankableBlocks[n.DataAtom] && !sum.visible:
		if !sum.hasBreak {
			n.Parent.RemoveChild(n)
			return sum, false, false
		}
		return sum, true, true
	}
	return sum, false, true
}

// flattenTable drops the table's rows that show nothing. When no row has
// more than one cell showing something the table only positions its content,
// so it is replaced by that content in reading order; it reports whether it
// did so. A row with two cells of content marks a table of data, kept as is.
func (t *htmlTidier) flattenTable(table *html.Node) bool {
	var filled []*html.Node
	isData := false
	for sec := table.FirstChild; sec != nil; sec = sec.NextSibling {
		switch sec.DataAtom {
		case atom.Caption:
			isData = true
		case atom.Thead, atom.Tbody, atom.Tfoot:
			for row := sec.FirstChild; row != nil; {
				next := row.NextSibling
				if row.DataAtom == atom.Tr {
					var rowFilled []*html.Node
					for cell := row.FirstChild; cell != nil; cell = cell.NextSibling {
						if t.cellHasContent[cell] {
							rowFilled = append(rowFilled, cell)
						}
					}
					switch len(rowFilled) {
					case 0:
						sec.RemoveChild(row)
					case 1:
						filled = append(filled, rowFilled[0])
					default:
						isData = true
					}
				}
				row = next
			}
		}
	}
	if isData {
		return false
	}
	for _, cell := range filled {
		hoistCell(cell, table)
	}
	table.Parent.RemoveChild(table)
	return true
}

// hoistCell moves a layout cell to just before table as a plain div, so text
// from different cells stays on separate lines. A line break in the source
// keeps the cells apart in text derived by stripping tags, too.
func hoistCell(cell, table *html.Node) {
	cell.Parent.RemoveChild(cell)
	cell.Data, cell.DataAtom, cell.Attr = "div", atom.Div, nil
	table.Parent.InsertBefore(cell, table)
	table.Parent.InsertBefore(&html.Node{Type: html.TextNode, Data: "\n"}, table)
}

// blankableBlocks are the block elements removed when they show nothing.
// Lists, quotes and preformatted text are left alone even when empty.
var blankableBlocks = map[atom.Atom]bool{
	atom.Div: true, atom.P: true, atom.Center: true, atom.Section: true,
	atom.Article: true, atom.Header: true, atom.Footer: true, atom.Main: true,
	atom.Aside: true, atom.Nav: true, atom.Address: true,
	atom.H1: true, atom.H2: true, atom.H3: true, atom.H4: true, atom.H5: true, atom.H6: true,
}

// hasVisibleText reports whether s has a character other than whitespace
// (non-breaking and figure spaces included) and preview padding.
func hasVisibleText(s string) bool {
	for _, r := range s {
		if !unicode.IsSpace(r) && !isPaddingOnly(r) && !isJoiner(r) {
			return true
		}
	}
	return false
}

type inlineStyle struct {
	hidden   bool // hidden in every client that applies inline styles
	zeroFont bool
	setsFont bool
}

// parseInlineStyle reads the declarations of a style attribute that hide an
// element. mso-hide:all is not one: it hides only from Outlook, and mail
// uses it to give Outlook an alternative to content every other client
// shows. Overflow hidden alone is not either; with a zero height or width
// it is how Gmail-compatible preview text is hidden.
func parseInlineStyle(style string) inlineStyle {
	var s inlineStyle
	var zeroBox, overflowHidden bool
	for _, decl := range strings.Split(style, ";") {
		prop, val, ok := strings.Cut(decl, ":")
		if !ok {
			continue
		}
		prop = strings.ToLower(strings.TrimSpace(prop))
		val = strings.ToLower(strings.TrimSpace(val))
		val = strings.TrimSpace(strings.TrimSuffix(val, "!important"))
		switch prop {
		case "display":
			s.hidden = s.hidden || val == "none"
		case "visibility":
			s.hidden = s.hidden || val == "hidden"
		case "opacity":
			s.hidden = s.hidden || isZeroValue(val)
		case "overflow":
			overflowHidden = val == "hidden"
		case "height", "max-height", "width", "max-width":
			zeroBox = zeroBox || isZeroValue(val)
		case "font-size":
			if isZeroValue(val) {
				s.zeroFont = true
			} else if val != "inherit" {
				s.setsFont = true
			}
		}
	}
	s.hidden = s.hidden || zeroBox && overflowHidden
	return s
}

// isZeroValue reports whether a CSS number or length is zero, with or without
// a unit.
func isZeroValue(val string) bool {
	num := strings.TrimRightFunc(val, func(r rune) bool { return unicode.IsLetter(r) || r == '%' })
	f, err := strconv.ParseFloat(num, 64)
	return err == nil && f == 0
}

// attr returns the value of n's attribute key and whether n has it.
func attr(n *html.Node, key string) (string, bool) {
	for _, a := range n.Attr {
		if a.Namespace == "" && a.Key == key {
			return a.Val, true
		}
	}
	return "", false
}
