package email

import (
	"fmt"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

// The Processor dereferences its logger unguarded, so tests need a real one.
func testProcessor() *Processor {
	nop := zerolog.Nop()
	return NewProcessor(&nop, NewThreadManager(nil), true, "")
}

// nestedMultipart builds a message body nested `depth` containers deep, with a
// text/plain leaf. Roughly fifty bytes per level, which is what makes the
// unbounded version cheap to attack: a few thousand levels fits inside any
// ordinary message size.
func nestedMultipart(depth int, leaf string) (body, boundary string) {
	cur := leaf
	for i := depth; i >= 1; i-- {
		b := fmt.Sprintf("b%d", i)
		var sb strings.Builder
		fmt.Fprintf(&sb, "--%s\r\n", b)
		if i == depth {
			sb.WriteString("Content-Type: text/plain\r\n\r\n")
			sb.WriteString(cur)
			sb.WriteString("\r\n")
		} else {
			fmt.Fprintf(&sb, "Content-Type: multipart/mixed; boundary=\"b%d\"\r\n\r\n", i+1)
			sb.WriteString(cur)
		}
		fmt.Fprintf(&sb, "--%s--\r\n", b)
		cur = sb.String()
	}
	return cur, "b1"
}

func TestParseMultipartContent_NestingIsBounded(t *testing.T) {
	t.Parallel()
	p := testProcessor()

	// Ordinary nesting still parses.
	body, boundary := nestedMultipart(3, "hello from the leaf")
	text, _ := p.parseMultipartContent(strings.NewReader(body), boundary)
	if !strings.Contains(text, "hello from the leaf") {
		t.Fatalf("shallow nesting should still parse; got %q", text)
	}

	// Hostile nesting returns rather than recursing until the stack dies.
	// A stack overflow is not recoverable in Go, so "it returned at all" is
	// the whole assertion.
	deep, deepBoundary := nestedMultipart(maxMultipartDepth+500, "unreachable")
	done := make(chan struct{})
	var deepText string
	go func() {
		defer close(done)
		deepText, _ = p.parseMultipartContent(strings.NewReader(deep), deepBoundary)
	}()
	<-done
	if !strings.Contains(deepText, "parsed in full") {
		t.Errorf("a message we refused to parse must say so; got %q", deepText)
	}
}

func TestParseMultipartAttachments_NestingIsBounded(t *testing.T) {
	t.Parallel()
	p := testProcessor()

	// An attachment nested one level past the cap must not come back; one just
	// inside it must. Without both halves this test passed with the limit
	// removed entirely, which is how it was found.
	inside := nestedMultipartWithAttachment(maxMultipartDepth-2, "inside.txt")
	if got := p.parseMultipartAttachments(strings.NewReader(inside.body), inside.boundary); len(got) != 1 {
		t.Fatalf("attachment within the depth limit was not extracted: got %d", len(got))
	}

	beyond := nestedMultipartWithAttachment(maxMultipartDepth+5, "beyond.txt")
	if got := p.parseMultipartAttachments(strings.NewReader(beyond.body), beyond.boundary); len(got) != 0 {
		t.Fatalf("recursed past the depth limit: extracted %d attachment(s)", len(got))
	}
}

type nestedFixture struct{ body, boundary string }

// nestedMultipartWithAttachment puts a single attachment at the deepest level,
// so its presence or absence reports exactly whether the parser descended.
func nestedMultipartWithAttachment(depth int, filename string) nestedFixture {
	leaf := "Content-Type: text/plain\r\n" +
		"Content-Disposition: attachment; filename=\"" + filename + "\"\r\n\r\n" +
		"payload\r\n"
	cur := leaf
	for i := depth; i >= 1; i-- {
		b := fmt.Sprintf("b%d", i)
		var sb strings.Builder
		fmt.Fprintf(&sb, "--%s\r\n", b)
		if i == depth {
			sb.WriteString(cur)
		} else {
			fmt.Fprintf(&sb, "Content-Type: multipart/mixed; boundary=\"b%d\"\r\n\r\n", i+1)
			sb.WriteString(cur)
		}
		fmt.Fprintf(&sb, "--%s--\r\n", b)
		cur = sb.String()
	}
	return nestedFixture{body: cur, boundary: "b1"}
}

// Breadth is bounded too: the per-part size limit says nothing about how many
// parts a container holds.
func TestParseMultipartContent_PartCountIsBounded(t *testing.T) {
	t.Parallel()
	p := testProcessor()
	var sb strings.Builder
	for i := 0; i < maxPartsPerLevel+50; i++ {
		sb.WriteString("--b\r\nContent-Type: text/plain\r\n\r\nx\r\n")
	}
	sb.WriteString("--b--\r\n")
	text, _ := p.parseMultipartContent(strings.NewReader(sb.String()), "b")
	if !strings.HasPrefix(text, "x") {
		t.Fatalf("expected the first text part, got %q", text)
	}
	// A truncated message that says nothing reads as a short one. The reader
	// has to be told, or the limit trades a crash for a silent data loss.
	if !strings.Contains(text, "parsed in full") {
		t.Errorf("truncation must be visible to the reader; got %q", text)
	}
}
