package email

import (
	"bytes"
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
	go func() {
		defer close(done)
		_, _ = p.parseMultipartContent(strings.NewReader(deep), deepBoundary)
	}()
	<-done
}

func TestParseMultipartAttachments_NestingIsBounded(t *testing.T) {
	t.Parallel()
	p := testProcessor()
	deep, deepBoundary := nestedMultipart(maxMultipartDepth+500, "unreachable")
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = p.parseMultipartAttachments(bytes.NewReader([]byte(deep)), deepBoundary)
	}()
	<-done
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
	if text != "x" {
		t.Fatalf("expected the first text part, got %q", text)
	}
}
