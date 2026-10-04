package connector

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Leicas/matrimail/pkg/imap"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
)

func TestLoginFolderDefaultsIncludeSent(t *testing.T) {
	a := &EmailAccount{}
	a.SetMonitoredFoldersFromJSON("")
	if !reflect.DeepEqual(a.MonitoredFolders, []string{"INBOX", "SENT"}) {
		t.Fatalf("folders=%v", a.MonitoredFolders)
	}
}

func TestEnsureAccountPreservesFolders(t *testing.T) {
	t.Setenv("MATRIMAIL_PASSPHRASE", "test-only-passphrase")
	t.Chdir(t.TempDir())
	ctx := context.Background()
	q := &EmailAccountQuery{DB: newTestDB(t)}
	if err := q.CreateTable(ctx); err != nil {
		t.Fatal(err)
	}
	a := &EmailAccount{UserMXID: "@me:example.com", Email: "me@example.com", Username: "me", Host: "imap.example.com", Port: 993, TLS: true, CreatedAt: time.Now(), LastSyncTime: time.Now()}
	if err := q.EnsureAccountRow(ctx, a); err != nil {
		t.Fatal(err)
	}
	fresh, err := q.GetAccount(ctx, a.UserMXID, a.Email)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fresh.MonitoredFolders, []string{"INBOX", "SENT"}) {
		t.Fatalf("fresh folders=%v", fresh.MonitoredFolders)
	}
	if err := q.UpdateMonitoredFolders(ctx, a.UserMXID, a.Email, []string{"INBOX", "SENT", "custom"}); err != nil {
		t.Fatal(err)
	}
	if err := q.EnsureAccountRow(ctx, a); err != nil {
		t.Fatal(err)
	}
	got, err := q.GetAccount(ctx, a.UserMXID, a.Email)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.MonitoredFolders, []string{"INBOX", "SENT", "custom"}) {
		t.Fatalf("folders=%v", got.MonitoredFolders)
	}
}

func TestDefaultLoginConfirmationListsSavedFolders(t *testing.T) {
	t.Setenv("MATRIMAIL_PASSPHRASE", "test-only-passphrase")
	t.Chdir(t.TempDir())
	ctx := context.Background()
	q := &EmailAccountQuery{DB: newTestDB(t)}
	if err := q.CreateTable(ctx); err != nil {
		t.Fatal(err)
	}
	a := &EmailAccount{UserMXID: "@me:example.com", Email: "me@example.com", CreatedAt: time.Now()}
	if err := q.EnsureAccountRow(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := q.UpdateMonitoredFolders(ctx, a.UserMXID, a.Email, []string{"INBOX", "SENT", "custom"}); err != nil {
		t.Fatal(err)
	}
	login := &EmailLoginProcess{connector: &EmailConnector{DB: q}, user: &bridgev2.User{User: &database.User{MXID: "@me:example.com"}}, email: a.Email,
		availableFolders: []imap.FolderInfo{{Name: "INBOX", Display: "INBOX", Type: imap.FolderTypeStandard}, {Name: "SENT", Display: "SENT", Type: imap.FolderTypeStandard}, {Name: "custom", Display: "custom"}}}
	step, err := login.handleFolderSelection(ctx, map[string]string{"folder_selection": "default"})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"INBOX", "SENT", "custom"} {
		if !strings.Contains(step.Instructions, name) {
			t.Errorf("confirmation omitted %s", name)
		}
	}
}
