package email

import (
	"fmt"
	"regexp"
	"strings"
)

// DecorativeImageMaxBytes is the size below which an inline image is
// considered decorative (tracking pixel, signature logo, separator graphic,
// social-icon button). Anything below this threshold is skipped when
// emitting m.image sidecars — broken-image cards from these tiny assets are
// noisier than they are useful. Real photos and screenshots are typically
// well above this size.
const DecorativeImageMaxBytes = 16 * 1024

// isLikelyDecorativeImage returns true when the inline image is too small
// to be content-bearing or matches a label pattern (tracking-pixel,
// transparent-spacer) we want to suppress from sidecar emission.
func isLikelyDecorativeImage(im *InlineImageMeta) bool {
	if im == nil {
		return true
	}
	if im.Size > 0 && im.Size < DecorativeImageMaxBytes {
		return true
	}
	label := strings.ToLower(im.Label)
	for _, marker := range decorativeLabelMarkers {
		if strings.Contains(label, marker) {
			return true
		}
	}
	return false
}

// decorativeLabelMarkers are substrings commonly found in filenames /
// alt-text of tracking pixels and structural graphics that we don't want to
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
// `style="...background-image: url(cid:XXX)..."` declaration. The cid is
// captured (group 3) so the caller can map it to an mxc URL.
//
// Tag name match list intentionally narrow — Matrix sanitizes <script>,
// <iframe>, etc., and arbitrary background-image on those wouldn't survive
// anyway. Adding tags here is cheap if a real email needs it.
var reBackgroundImageCSS = regexp.MustCompile(
	`(?is)<(td|table|div|tr|th|p|a|span|section|article)\b([^>]*?\bstyle\s*=\s*['"][^'"]*background-image\s*:\s*url\(\s*['"]?cid:([^)\s'"]+)['"]?\s*\)[^'"]*['"][^>]*)>`,
)

// reBackgroundAttr matches Outlook-style `<table background="cid:XXX">` or
// `<td background="cid:XXX">`. cid captured at group 3.
var reBackgroundAttr = regexp.MustCompile(
	`(?is)<(td|table|tr|th|div)\b([^>]*?\bbackground\s*=\s*['"]cid:([^'"\s>]+)['"][^>]*)>`,
)

// MaterializeBackgroundImages finds elements that reference an inline image
// only through CSS `background-image: url(cid:...)` or the legacy
// `background="cid:..."` attribute, and injects an `<img src="mxc://...">`
// immediately after the opening tag so Matrix's HTML sanitizer (which
// strips style and unknown attributes) still renders the image.
//
// uploadByCID is invoked for any cid that is not already present in
// cidToMXC; implementations should locate the attachment, upload it via
// `intent.UploadMedia`, mark it as inline-used, and return the mxc URL and
// the resolved mime type. Returning ("", "") from uploadByCID is treated as
// "image unavailable" — the tag is then left alone (Matrix will simply not
// show a background, which is what would have happened anyway).
//
// The original `style` / `background` attribute is left in place. Matrix
// will strip it on rendering, but downstream consumers (mail client
// archives, the saved-HTML attachment) keep it for fidelity.
func MaterializeBackgroundImages(
	htmlIn string,
	cidToMXC map[string]string,
	uploadByCID func(cid string) (mxc string, mime string),
) string {
	if htmlIn == "" {
		return htmlIn
	}

	inject := func(tagWithBracket, cidRef string) string {
		cid := normalizeCIDRef(cidRef)
		mxc := cidToMXC[cid]
		if mxc == "" && uploadByCID != nil {
			if got, _ := uploadByCID(cid); got != "" {
				cidToMXC[cid] = got
				mxc = got
			}
		}
		if mxc == "" {
			return tagWithBracket
		}
		// Inject the <img> right after the matched opening tag. The alt
		// text gives Matrix something to render in the text-only fallback
		// when the mxc fails.
		return tagWithBracket + fmt.Sprintf(`<img src="%s" alt="" style="max-width:100%%">`, mxc)
	}

	out := reBackgroundImageCSS.ReplaceAllStringFunc(htmlIn, func(m string) string {
		subs := reBackgroundImageCSS.FindStringSubmatch(m)
		if len(subs) < 4 {
			return m
		}
		return inject(m, subs[3])
	})
	out = reBackgroundAttr.ReplaceAllStringFunc(out, func(m string) string {
		subs := reBackgroundAttr.FindStringSubmatch(m)
		if len(subs) < 4 {
			return m
		}
		return inject(m, subs[3])
	})
	return out
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
