package email

import (
	"regexp"
	"strings"
)

// DecorativeImageMaxBytes is the size below which an inline image is
// considered decorative (tracking pixel, signature logo, separator graphic,
// social-icon button) and not sent to the room: an image event for each of
// these tiny assets is noisier than it is useful. Real photos and
// screenshots are typically well above this size.
const DecorativeImageMaxBytes = 16 * 1024

// isLikelyDecorativeImage returns true when an inline image is too small
// to be content-bearing or its file name matches a tracking-pixel or spacer
// pattern. Alt text is not consulted: it describes content ("package
// tracking screenshot") as often as decoration.
func isLikelyDecorativeImage(filename string, size int64) bool {
	if size > 0 && size < DecorativeImageMaxBytes {
		return true
	}
	filename = strings.ToLower(filename)
	for _, marker := range decorativeLabelMarkers {
		if strings.Contains(filename, marker) {
			return true
		}
	}
	return false
}

// decorativeLabelMarkers are substrings commonly found in the file names of
// tracking pixels and structural graphics that we don't want to
// expose as standalone Matrix events.
var decorativeLabelMarkers = []string{
	"spacer",
	"pixel",
	"tracking",
	"transparent",
	"divider",
	"separator",
	"clear.gif",
	"clear.png",
	"1x1",
}

// IsLikelyMarketingHTML returns true when the HTML looks like a typical
// marketing-email layout that won't survive Matrix's restricted HTML subset
// (heavy nested tables, CSS background images, lots of cid-referenced
// graphics) and has little real text content. Callers should ship the
// plain-text body and attach the original HTML as a file rather than try to
// render the HTML in Matrix, which would produce empty bordered boxes.
//
// The heuristic intentionally errs on the side of preserving HTML: small or
// text-rich emails always render as-is.
func IsLikelyMarketingHTML(html, plainText string) bool {
	if len(html) < 16*1024 {
		return false
	}
	lowered := strings.ToLower(html)
	tableCount := strings.Count(lowered, "<table")
	bgCSSCount := strings.Count(lowered, "background-image")
	bgAttrCount := strings.Count(lowered, " background=\"") + strings.Count(lowered, " background='")
	imgCount := strings.Count(lowered, "<img")

	// Sum of structural-layout signals.
	signals := tableCount + bgCSSCount + bgAttrCount

	// "Mostly tables and backgrounds": at least 8 layout signals total, and
	// at least 2 of them are background images (pure transactional emails
	// with simple tables don't usually use backgrounds at all).
	if signals < 8 || (bgCSSCount+bgAttrCount) < 2 {
		// Image-heavy newsletters with few backgrounds: catch those too —
		// 12+ images and almost no plain text is unmistakably a newsletter.
		if imgCount < 12 {
			return false
		}
	}

	textLen := len(strings.TrimSpace(plainText))
	// "Little real text" — < 800 chars of actual content. A personal email
	// thread with embedded images stays well above this; marketing digests
	// with their "tagline + button + footer" structure stay below.
	if textLen > 800 {
		return false
	}
	return true
}

// reBackgroundImageCSS matches an opening tag carrying an inline
// `style="...background-image: url(cid:XXX)..."` declaration; the cid is
// group 3.
var reBackgroundImageCSS = regexp.MustCompile(
	`(?is)<(td|table|div|tr|th|p|a|span|section|article)\b([^>]*?\bstyle\s*=\s*['"][^'"]*background-image\s*:\s*url\(\s*['"]?cid:([^)\s'"]+)['"]?\s*\)[^'"]*['"][^>]*)>`,
)

// reBackgroundAttr matches Outlook-style `<table background="cid:XXX">` or
// `<td background="cid:XXX">`; the cid is group 3.
var reBackgroundAttr = regexp.MustCompile(
	`(?is)<(td|table|tr|th|div)\b([^>]*?\bbackground\s*=\s*['"]cid:([^'"\s>]+)['"][^>]*)>`,
)

// backgroundImageCIDs returns, normalised, the cids of
// inline images an element shows only through CSS `background-image:
// url(cid:...)` or the legacy `background="cid:..."` attribute. The client's
// sanitizer strips both, so without sending these images separately a
// reader would never see them.
func backgroundImageCIDs(htmlIn string) []string {
	var cids []string
	for _, re := range []*regexp.Regexp{reBackgroundImageCSS, reBackgroundAttr} {
		for _, m := range re.FindAllStringSubmatch(htmlIn, -1) {
			cids = append(cids, normalizeCIDRef(m[3]))
		}
	}
	return cids
}

// reEmptyProtonSignature matches one Proton Mail signature element marked
// empty (class protonmail_signature_block-empty) whose content is only
// whitespace and <br>. Proton composes every message with a signature block
// for the user's and Proton's signatures and keeps the block when both are
// unset, so without this a room shows a stack of blank wrappers.
var reEmptyProtonSignature = regexp.MustCompile(
	`(?is)<div\b[^>]*\bclass="[^"]*\bprotonmail_signature_block-empty\b[^"]*"[^>]*>(?:\s|<br\s*/?>)*</div>`,
)

// stripEmptyProtonSignature removes Proton Mail's empty signature elements.
// Proton nests them two deep, a container around the user and Proton
// signatures, and the container only matches once its empty children are
// gone: so children first, then the container they emptied. A signature with
// content keeps its container and loses only the empty sibling.
func stripEmptyProtonSignature(htmlBody string) string {
	htmlBody = reEmptyProtonSignature.ReplaceAllString(htmlBody, "")
	return reEmptyProtonSignature.ReplaceAllString(htmlBody, "")
}
