package connector

import (
	"context"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
)

// primaryGmailPortal keeps historical split rooms from competing over mailbox state.
func primaryGmailPortal(ctx context.Context, db *database.Database, receiver networkid.UserLoginID, gmailID string) (*database.Portal, error) {
	portals, err := db.Portal.GetAllWithMXID(ctx)
	if err != nil {
		return nil, err
	}
	var primary *database.Portal
	var first *database.Message
	for _, portal := range portals {
		pm, _ := portal.Metadata.(*PortalMetadata)
		if portal.Receiver != receiver || pm == nil || pm.GmailThreadID != gmailID {
			continue
		}
		message, err := db.Message.GetFirstPortalMessage(ctx, portal.PortalKey)
		if err != nil {
			return nil, err
		}
		earlier := primary == nil || (message != nil && (first == nil || message.Timestamp.Before(first.Timestamp)))
		sameTime := (message == nil && first == nil) || (message != nil && first != nil && message.Timestamp.Equal(first.Timestamp))
		if earlier || (sameTime && primary != nil && portal.ID < primary.ID) {
			primary, first = portal, message
		}
	}
	return primary, nil
}
