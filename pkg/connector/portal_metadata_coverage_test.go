package connector

import (
	"reflect"
	"testing"
	"time"

	"github.com/Leicas/matrimail/pkg/email"
)

// Every field carrying the thread's reply context must reach the persisted
// snapshot, because after a restart that snapshot is the only thing a reply is
// addressed from. The failure this guards is mundane and likely: someone adds
// a Last* field to EmailThread, uses it in the send path, and forgets the
// snapshot -- reintroducing exactly the staleness that persisting on receive
// was added to remove, and doing it silently.
//
// Reflection rather than a hand-written list, so the test fails when the struct
// grows rather than when someone remembers to update the test.
func TestPortalMetadataFromThread_CoversEveryReplyContextField(t *testing.T) {
	t.Parallel()

	// Populated with distinctive, non-zero values so "copied" is detectable.
	thread := &email.EmailThread{}
	tv := reflect.ValueOf(thread).Elem()
	tt := tv.Type()
	for i := 0; i < tt.NumField(); i++ {
		f := tv.Field(i)
		if !f.CanSet() {
			continue
		}
		switch f.Kind() {
		case reflect.String:
			f.SetString("value-" + tt.Field(i).Name)
		case reflect.Slice:
			if f.Type().Elem().Kind() == reflect.String {
				f.Set(reflect.ValueOf([]string{"value-" + tt.Field(i).Name}))
			}
		case reflect.Bool:
			f.SetBool(true)
		case reflect.Struct:
			if f.Type() == reflect.TypeOf(time.Time{}) {
				f.Set(reflect.ValueOf(time.Unix(1750000000, 0).UTC()))
			}
		}
	}

	pm := PortalMetadataFromThread(thread)
	if pm == nil {
		t.Fatal("nil metadata")
	}
	pv := reflect.ValueOf(pm).Elem()

	// Deliberately not persisted, with the reason, so the exclusion is a
	// decision rather than an omission.
	notReplyContext := map[string]string{
		// Reset to now() on every cache access and read only by TTL eviction.
		// Persisting it would store a timestamp that is meaningless on restore.
		"LastAccessed": "in-memory cache bookkeeping, not reply context",
	}

	for i := 0; i < tt.NumField(); i++ {
		name := tt.Field(i).Name
		// Only the reply context matters here. The participant-delta fields are
		// per-email churn for room membership and are deliberately not stored;
		// InReplyTo and Cc belong to the compose path, not to addressing a reply.
		if len(name) < 4 || name[:4] != "Last" {
			continue
		}
		if why, skip := notReplyContext[name]; skip {
			t.Logf("skipping %s: %s", name, why)
			continue
		}
		target := pv.FieldByName(name)
		if !target.IsValid() {
			t.Errorf("EmailThread.%s has no counterpart in PortalMetadata; a reply after a restart will not see it", name)
			continue
		}
		if target.IsZero() {
			t.Errorf("EmailThread.%s is not copied into the snapshot; after a restart a reply is addressed without it", name)
		}
	}
}
