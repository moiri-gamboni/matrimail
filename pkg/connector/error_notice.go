package connector

import (
	"context"
	"fmt"
	netmail "net/mail"
	"strings"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/format"
	"maunium.net/go/mautrix/id"
)

// loginIDFromReceiver converts a receiver string (the form stored on the
// processor's threading scope) back into a networkid.UserLoginID for
// bridgev2 cache lookups.
func loginIDFromReceiver(receiver string) networkid.UserLoginID {
	return networkid.UserLoginID(receiver)
}

// postErrorToRoom posts a user-visible notice into a specific Matrix room.
// title is a short headline; details may include err.Error() (callers should
// sanitize first if it could contain PII/credentials).
func postErrorToRoom(ctx context.Context, bridge *bridgev2.Bridge, roomID id.RoomID, title, details string) error {
	if bridge == nil {
		return fmt.Errorf("postErrorToRoom: nil bridge")
	}
	if roomID == "" {
		return fmt.Errorf("postErrorToRoom: empty roomID")
	}
	intent := bridge.Bot
	if intent == nil {
		return fmt.Errorf("postErrorToRoom: bridge bot intent unavailable")
	}
	md := fmt.Sprintf("⚠️ **%s**", title)
	if details != "" {
		md += "\n\n" + details
	}
	content := format.RenderMarkdown(md, true, false)
	content.MsgType = event.MsgNotice
	if _, err := intent.SendMessage(ctx, roomID, event.EventMessage, &event.Content{Parsed: &content}, nil); err != nil {
		return fmt.Errorf("send notice: %w", err)
	}
	return nil
}

// postErrorToPortal looks up the Matrix room for the portal and forwards to
// postErrorToRoom. No-op (returns nil) if portal has no MXID yet.
func postErrorToPortal(ctx context.Context, bridge *bridgev2.Bridge, portal *bridgev2.Portal, title, details string) error {
	if portal == nil {
		return fmt.Errorf("postErrorToPortal: nil portal")
	}
	if portal.MXID == "" {
		// No room provisioned yet — nothing we can post into.
		return nil
	}
	return postErrorToRoom(ctx, bridge, portal.MXID, title, details)
}

// processorErrorNotifier adapts the email.Processor's ErrorNotifier hook to
// the connector's portal/room machinery. Best-effort: failures to find the
// portal or post the notice are logged and swallowed.
type processorErrorNotifier struct {
	ec *EmailConnector
}

// NotifyProcessingError surfaces a processing error to the user. We don't
// have direct access to the affected portal here — the processor calls this
// before threading finishes — so we fall back to the user's management room.
// TODO: thread the portal through once available so notices land in the
// affected room rather than the management room.
func (n *processorErrorNotifier) NotifyProcessingError(ctx context.Context, receiver, messageID, subject, kind string, err error) {
	if n == nil || n.ec == nil || n.ec.Bridge == nil {
		return
	}
	login := n.ec.Bridge.GetCachedUserLoginByID(loginIDFromReceiver(receiver))
	if login == nil || login.User == nil {
		return
	}
	md := fmt.Sprintf("Failed to process inbound email (%s): %s", kind, err.Error())
	if subject != "" {
		md += fmt.Sprintf("\nSubject: %s", subject)
	}
	if msgErr := sendBridgeNotice(ctx, login.User, md); msgErr != nil {
		n.ec.Bridge.Log.Warn().Err(msgErr).Str("kind", kind).Msg("Failed to deliver processing-error notice")
	}
}

// postSendReceiptToPortal posts a one-line record of who an outbound email
// actually went to, into the room it was sent from.
//
// Reply-all is the default and the recipient set is computed from thread
// state the user never sees, so a Matrix message box that looks like a chat
// reply silently becomes a reply-all. Without this line the first indication
// that a third party was on the Cc is their answer. Best-effort: a failure to
// post must never fail a send that already happened.
func postSendReceiptToPortal(ctx context.Context, bridge *bridgev2.Bridge, portal *bridgev2.Portal, from string, to, cc []netmail.Address, dropped []string) error {
	if portal == nil || portal.MXID == "" || bridge == nil {
		return nil
	}
	intent := bridge.Bot
	if intent == nil {
		return nil
	}
	// "Addressed to", not "Sent to": this reports the recipient set handed to
	// the server, which is what the reader needs to check. Delivery is a later
	// and separate event.
	md := "📤 Addressed to " + joinAddrs(to)
	if len(cc) > 0 {
		md += " · cc " + joinAddrs(cc)
	}
	if from != "" {
		md += " · from " + from
	}
	if len(dropped) > 0 {
		md += fmt.Sprintf("\n\n⚠️ %d address(es) on this thread could not be parsed and are **not** on this reply: %s",
			len(dropped), strings.Join(dropped, ", "))
	}
	content := format.RenderMarkdown(md, true, false)
	content.MsgType = event.MsgNotice
	if _, err := intent.SendMessage(ctx, portal.MXID, event.EventMessage, &event.Content{Parsed: &content}, nil); err != nil {
		return fmt.Errorf("send receipt: %w", err)
	}
	return nil
}

func joinAddrs(addrs []netmail.Address) string {
	if len(addrs) == 0 {
		// Reachable: a reply-all can resolve to Cc-only. Saying "nobody" about
		// a message that did go somewhere is worse than saying nothing.
		return "(no To: recipients — Cc only)"
	}
	parts := make([]string, 0, len(addrs))
	for _, a := range addrs {
		parts = append(parts, a.Address)
	}
	return strings.Join(parts, ", ")
}
