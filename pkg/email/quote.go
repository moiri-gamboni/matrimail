package email

import (
	"fmt"
	netmail "net/mail"
	"regexp"
	"strings"
	"time"
)

// StripQuotedReply removes quoted history from the body of a reply email,
// returning only the "new" portion the sender wrote on top. Mirrors what a
// modern mail client shows above the quote-fold.
//
// The heuristic looks (in order) for the FIRST occurrence of:
//
//  1. A Gmail / Apple-Mail attribution line: `On <date>, <name> <addr> wrote:`
//     (possibly soft-wrapped across two lines).
//  2. An Outlook divider: `-----Original Message-----` or `_____________`.
//  3. An Outlook header block: a line starting with `From:` immediately
//     followed by `Sent:` or `Date:` / `To:` / `Subject:`.
//  4. A run of `>`-quoted lines at the tail of the message.
//
// Everything from that point to EOF is dropped. If none of the heuristics
// match, the original text is returned unchanged — better to show too much
// than to silently truncate the user's message.
//
// The result is right-trimmed (trailing blank lines removed) but otherwise
// preserves the sender's formatting. A signature delimiter (`-- ` on its own
// line) is NOT stripped — signatures belong to the new message.
func StripQuotedReply(text string) string {
	if text == "" {
		return ""
	}
	lines := strings.Split(text, "\n")
	cut := len(lines)

	for i := 0; i < len(lines); i++ {
		l := strings.TrimRight(lines[i], " \t\r")
		ll := strings.TrimSpace(l)

		// (1) Gmail / Apple Mail attribution: locale-specific "On ... wrote:" lines.
		if matchesGmailAttribution(ll) {
			cut = i
			break
		}
		// Multi-line variant: attribution split across 2-3 lines (Gmail soft-wraps
		// long display names). We try joining up to two trailing lines whenever
		// the current line starts with any known locale prefix.
		if hasGmailAttributionPrefix(ll) && i+1 < len(lines) {
			joined := ll
			for j := 1; j <= 2 && i+j < len(lines); j++ {
				joined += " " + strings.TrimSpace(lines[i+j])
				if matchesGmailAttribution(joined) {
					cut = i
					break
				}
			}
			if cut == i {
				break
			}
		}

		// (2) Outlook dividers
		if ll == "-----Original Message-----" ||
			ll == "________________________________" ||
			outlookDividerRE.MatchString(ll) {
			cut = i
			break
		}

		// (3) Outlook header block: `From: ...` followed by `Sent:`/`Date:`/`To:`/`Subject:`
		if strings.HasPrefix(ll, "From: ") && i+1 < len(lines) {
			next := strings.TrimSpace(lines[i+1])
			if strings.HasPrefix(next, "Sent: ") ||
				strings.HasPrefix(next, "Date: ") ||
				strings.HasPrefix(next, "To: ") ||
				strings.HasPrefix(next, "Subject: ") {
				cut = i
				break
			}
		}
	}

	// (4) Trailing run of `>`-quoted lines (only if nothing else matched).
	if cut == len(lines) {
		j := len(lines) - 1
		// Skip trailing blanks.
		for j >= 0 && strings.TrimSpace(lines[j]) == "" {
			j--
		}
		// Walk backward while we see quoted lines (or blanks between quote blocks).
		end := j + 1
		sawQuote := false
		for j >= 0 {
			trimmed := strings.TrimSpace(lines[j])
			if trimmed == "" {
				j--
				continue
			}
			if strings.HasPrefix(trimmed, ">") {
				sawQuote = true
				j--
				continue
			}
			break
		}
		if sawQuote {
			cut = j + 1
			_ = end
		}
	}

	out := strings.Join(lines[:cut], "\n")
	// Right-trim trailing blank lines and whitespace.
	out = strings.TrimRight(out, " \t\r\n")
	return out
}

// gmailAttrRegexes matches Gmail / Apple Mail / Thunderbird attribution lines
// across the locales Google ships by default. Examples:
//
//	English:    On Mon, May 19, 2026 at 10:30 AM John Doe <john@example.com> wrote:
//	French:     Le mar. 19 mai 2026, à 09 h 19, Antoine Weill-Duflos <antoine@…> a écrit :
//	German:     Am Mo., 19. Mai 2026 um 10:30 schrieb John Doe <john@example.com>:
//	Spanish:    El lun, 19 may 2026 a las 10:30, John Doe <john@example.com> escribió:
//	Italian:    Il lun 19 mag 2026, 10:30 John Doe <john@example.com> ha scritto:
//	Portuguese: Em seg., 19 de mai. de 2026 às 10:30, John Doe <john@…> escreveu:
//	Dutch:      Op ma 19 mei 2026 om 10:30 schreef John Doe <john@example.com>:
//
// Each pattern is anchored — the line must start with the locale-specific
// connective ("On ", "Le ", "Am ", "El ", "Il ", "Em ", "Op ") and end with
// the locale-specific verb plus a colon (French uses a space before the
// colon per typographic convention, so `\s*:` is lenient).
var gmailAttrRegexes = []*regexp.Regexp{
	regexp.MustCompile(`^On .{3,400} wrote\s*:\s*$`),
	regexp.MustCompile(`^Le .{3,400} a écrit\s*:\s*$`),
	regexp.MustCompile(`^Am .{3,400} schrieb .{1,200}\s*:\s*$`),
	regexp.MustCompile(`^El .{3,400} escribió\s*:\s*$`),
	regexp.MustCompile(`^Il .{3,400} ha scritto\s*:\s*$`),
	regexp.MustCompile(`^Em .{3,400} escreveu\s*:\s*$`),
	regexp.MustCompile(`^Op .{3,400} schreef .{1,200}\s*:\s*$`),
}

// gmailAttrPrefixes lists the leading connectives we consider for the
// multi-line soft-wrap path. Keep this in sync with gmailAttrRegexes.
var gmailAttrPrefixes = []string{"On ", "Le ", "Am ", "El ", "Il ", "Em ", "Op "}

func matchesGmailAttribution(s string) bool {
	for _, re := range gmailAttrRegexes {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}

func hasGmailAttributionPrefix(s string) bool {
	for _, p := range gmailAttrPrefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// outlookDividerRE matches the long underscore divider Outlook injects above
// quoted blocks (12+ underscores on their own line).
var outlookDividerRE = regexp.MustCompile(`^_{12,}$`)

// htmlQuoteMarkers lists case-insensitive substrings (already lower-cased) that
// open the quoted-history block in the HTML half of a reply. The first such
// marker found in the body is the cut point — everything from there to EOF is
// dropped. Markers cover Gmail, Apple Mail, Proton Mail, and Outlook's web
// and desktop quote layouts. Each is a client's own quote marker, so a
// blockquote the sender wrote into their message is never mistaken for one.
var htmlQuoteMarkers = []string{
	`<div class="gmail_quote`,                            // Gmail (with optional gmail_quote_container suffix)
	`<blockquote class="gmail_quote`,                     // Gmail standalone blockquote variant
	`<blockquote type="cite"`,                            // Apple Mail
	`<div class="protonmail_quote`,                       // Proton Mail (wraps the attribution line and the blockquote)
	`<blockquote class="protonmail_quote`,                // Proton Mail blockquote without the wrapper
	`<div id="appendonsend"`,                             // Outlook web (above the quote)
	`<div id="mail-editor-reference-message-container"`, // New Outlook
	`<hr id="stopspelling"`,                              // Outlook quirk divider
	`<div style="border:none;border-top:solid`,           // Outlook desktop header block above quoted message
	`<div style="border-top:solid`,                       // Same, alternative spacing
}

// StripQuotedReplyHTML removes the quoted history from the HTML body of a
// reply, returning only the new content the sender wrote above the
// quote-fold. Mirrors StripQuotedReply for the plain-text body so Matrix
// rooms don't render the whole thread chain twice (once per inbound message).
//
// We cut at the first occurrence of a known quote-container marker. Trailing
// `<br>` runs and empty `<div>` spacers immediately preceding the cut are
// trimmed so the visible reply doesn't end on a hanging blank line. We do NOT
// attempt to balance unclosed tags — Matrix clients run the HTML through a
// sanitizer that handles truncated fragments gracefully, and the alternative
// (full DOM parse + walk) is significantly more code for marginal gain.
//
// If no marker is found, the input is returned unchanged.
func StripQuotedReplyHTML(htmlBody string) string {
	if htmlBody == "" {
		return ""
	}
	lower := strings.ToLower(htmlBody)
	cut := len(htmlBody)
	for _, m := range htmlQuoteMarkers {
		if idx := strings.Index(lower, m); idx >= 0 && idx < cut {
			cut = idx
		}
	}
	if cut == len(htmlBody) {
		return htmlBody
	}
	return reTrailingFiller.ReplaceAllString(htmlBody[:cut], "")
}

// reTrailingFiller matches the run of whitespace, <br> tags and empty <div>
// spacers at the end of the input: the blank lines a client leaves between
// the new text and its quote (a bare <br> from Gmail, a <div><br></div> from
// Proton). One anchored match keeps the cost linear in the body size.
var reTrailingFiller = regexp.MustCompile(`(?is)(?:\s|<br\b[^>]*>|<div\b[^>]*>(?:\s|<br\b[^>]*>)*</div>)+$`)

// FormatGmailQuoteText returns a Gmail-style plain-text quote block to be
// appended to a reply body. Layout:
//
//	On <Date>, <Sender> wrote:
//	> <line 1>
//	> <line 2>
//	> ...
//
// The Date is formatted in loc as `Mon, Jan 2, 2006 at 3:04 PM` (Gmail's
// canonical English form). When parentFrom parses as a "Name <addr>" pair the display
// name is used; otherwise the raw From is preserved.
//
// parentText should be the full text body of the parent message (already
// containing its own quote chain if any) — Gmail only quotes one level deep
// at the producer side; the chain accumulates through repeated replies.
func FormatGmailQuoteText(parentDate time.Time, loc *time.Location, parentFrom, parentText string) string {
	attr := buildAttributionLine(parentDate, loc, parentFrom)
	if parentText == "" {
		return attr + "\n"
	}
	// Prefix every line with "> " (or ">" for empty lines, matching Gmail).
	var b strings.Builder
	b.Grow(len(parentText) + 64)
	b.WriteString(attr)
	b.WriteString("\n")
	for _, line := range strings.Split(parentText, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			b.WriteString(">\n")
		} else if strings.HasPrefix(line, ">") {
			// Already quoted — increment quote depth, no space between markers.
			b.WriteString(">")
			b.WriteString(line)
			b.WriteString("\n")
		} else {
			b.WriteString("> ")
			b.WriteString(line)
			b.WriteString("\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// FormatGmailQuoteHTML returns a Gmail-style HTML quote block. It mirrors the
// exact DOM Gmail emits so threaded clients (Gmail, Apple Mail, Outlook
// modern) collapse it into a "show trimmed content" fold.
//
//	<div class="gmail_quote gmail_quote_container">
//	  <div dir="ltr" class="gmail_attr">On ..., ... wrote:<br></div>
//	  <blockquote class="gmail_quote" style="...">
//	    <parentHTML>
//	  </blockquote>
//	</div>
//
// If parentHTML is empty, no quote block is produced and the caller should
// fall back to omitting the HTML alternative entirely (a text-only quote is
// fine on its own).
func FormatGmailQuoteHTML(parentDate time.Time, loc *time.Location, parentFrom, parentHTML string) string {
	if parentHTML == "" {
		return ""
	}
	attr := htmlEscape(buildAttributionLine(parentDate, loc, parentFrom))
	return `<div class="gmail_quote gmail_quote_container"><div dir="ltr" class="gmail_attr">` +
		attr + `<br></div><blockquote class="gmail_quote" style="margin:0px 0px 0px 0.8ex;border-left:1px solid rgb(204,204,204);padding-left:1ex">` +
		parentHTML +
		`</blockquote></div>`
}

// buildAttributionLine produces the "On <date>, <sender> wrote:" header used
// by both text and HTML quote builders. The date is shown in loc, the user's
// zone: the parent's Date header carries the sender's zone (often UTC), which
// would show a time the user's clock never read.
func buildAttributionLine(parentDate time.Time, loc *time.Location, parentFrom string) string {
	when := parentDate
	if when.IsZero() {
		when = time.Now()
	}
	// Gmail's English format: "Mon, Jan 2, 2006 at 3:04 PM"
	dateStr := when.In(loc).Format("Mon, Jan 2, 2006 at 3:04 PM")

	sender := strings.TrimSpace(parentFrom)
	if addr, err := netmail.ParseAddress(sender); err == nil {
		name := strings.TrimSpace(addr.Name)
		if name != "" {
			sender = fmt.Sprintf("%s <%s>", name, addr.Address)
		} else {
			sender = addr.Address
		}
	}
	if sender == "" {
		sender = "Unknown"
	}
	return fmt.Sprintf("On %s, %s wrote:", dateStr, sender)
}

// htmlEscape escapes the four HTML metacharacters. We avoid pulling in
// html/template just for this — the attribution line is plain text.
func htmlEscape(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&quot;",
	)
	return r.Replace(s)
}

// NormalizeReplySubject collapses a chain of "Re:" / "RE:" / "re:" prefixes
// into a single canonical "Re: " and trims surrounding whitespace. Matches
// Gmail's web-client behavior, which never emits "Re: Re: Foo".
//
// If the subject has no existing Re: prefix, one is added (caller is
// responsible for choosing whether to call this — first sends on a new
// thread should NOT pass through here).
func NormalizeReplySubject(subject string) string {
	s := strings.TrimSpace(subject)
	// Strip any leading Re:/RE:/re: (with optional "[N]") prefixes repeatedly.
	for {
		lower := strings.ToLower(s)
		switch {
		case strings.HasPrefix(lower, "re:"):
			s = strings.TrimSpace(s[3:])
		case strings.HasPrefix(lower, "re["):
			if idx := strings.Index(lower, "]:"); idx > 0 {
				s = strings.TrimSpace(s[idx+2:])
			} else {
				return "Re: " + s
			}
		default:
			return "Re: " + s
		}
	}
}
