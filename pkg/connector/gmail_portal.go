package connector

import (
	"context"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
)

// primaryGmailPortal keeps historical split rooms from competing over mailbox state.
func primaryGmailPortal(ctx context.Context, db *database.Database, receiver networkid.UserLoginID, gmailID string) (*database.Portal, error) {
	byThread, err := gmailPortals(ctx, db, receiver)
	if err != nil {
		return nil, err
	}
	return oldestPortal(ctx, db, byThread[gmailID])
}

// primaryGmailPortals returns the primary room of each of receiver's Gmail
// threads. Every portal's metadata is decoded to find its thread, so a caller
// that needs many threads asks here once rather than per thread.
func primaryGmailPortals(ctx context.Context, db *database.Database, receiver networkid.UserLoginID) (map[string]*database.Portal, error) {
	byThread, err := gmailPortals(ctx, db, receiver)
	if err != nil {
		return nil, err
	}
	primaries := make(map[string]*database.Portal, len(byThread))
	for gmailID, portals := range byThread {
		if primaries[gmailID], err = oldestPortal(ctx, db, portals); err != nil {
			return nil, err
		}
	}
	return primaries, nil
}

// gmailPortals groups receiver's rooms by the Gmail thread they hold.
func gmailPortals(ctx context.Context, db *database.Database, receiver networkid.UserLoginID) (map[string][]*database.Portal, error) {
	portals, err := db.Portal.GetAllWithMXID(ctx)
	if err != nil {
		return nil, err
	}
	byThread := make(map[string][]*database.Portal)
	for _, portal := range portals {
		pm, _ := portal.Metadata.(*PortalMetadata)
		if portal.Receiver != receiver || pm == nil || pm.GmailThreadID == "" {
			continue
		}
		byThread[pm.GmailThreadID] = append(byThread[pm.GmailThreadID], portal)
	}
	return byThread, nil
}

// oldestPortal picks the room whose first message came first, the lower
// portal ID on a tie.
func oldestPortal(ctx context.Context, db *database.Database, portals []*database.Portal) (*database.Portal, error) {
	if len(portals) == 1 {
		return portals[0], nil
	}
	var primary *database.Portal
	var first *database.Message
	for _, portal := range portals {
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
