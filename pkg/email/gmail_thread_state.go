package email

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/rs/zerolog"
	"golang.org/x/oauth2"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"

	gmail "google.golang.org/api/gmail/v1"
)

// ThreadState is the part of a mail thread's state that is mirrored between
// Gmail and a chat client: whether it is out of the inbox, and whether it is
// waiting to be read.
type ThreadState struct {
	Archived bool
	Unread   bool
}

// GmailThread is a Gmail thread's state as last read, with what Apply needs
// to change it.
type GmailThread struct {
	ID    string
	State ThreadState
	// newestMessageID is the thread's newest message that is not a draft,
	// where "mark as unread" goes.
	newestMessageID string
}

// GmailThreads reads and changes the archived and unread state of Gmail
// threads for one account. It needs the gmail.modify scope.
type GmailThreads struct {
	TokenSource oauth2.TokenSource
	Log         *zerolog.Logger

	clientOptions []option.ClientOption
}

// NewGmailThreads builds the client; opts are added to the Gmail service's
// options, after the token source.
func NewGmailThreads(ts oauth2.TokenSource, log *zerolog.Logger, opts ...option.ClientOption) *GmailThreads {
	return &GmailThreads{TokenSource: ts, Log: log, clientOptions: opts}
}

func (g *GmailThreads) service(ctx context.Context) (*gmail.Service, error) {
	opts := append([]option.ClientOption{option.WithTokenSource(g.TokenSource)}, g.clientOptions...)
	svc, err := gmail.NewService(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("gmail.NewService: %w", err)
	}
	return svc, nil
}

// Get reads a thread's labels. The thread is archived when none of its
// messages carries INBOX, which is when Gmail stops listing it in the inbox,
// and unread when any of them carries UNREAD.
func (g *GmailThreads) Get(ctx context.Context, threadID string) (*GmailThread, error) {
	svc, err := g.service(ctx)
	if err != nil {
		return nil, err
	}
	resp, err := svc.Users.Threads.Get("me", threadID).Format("minimal").Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("threads.get %s: %w", threadID, err)
	}
	labels := make(map[string][]string, len(resp.Messages))
	for _, m := range resp.Messages {
		labels[m.Id] = m.LabelIds
	}
	g.Log.Debug().Str("gmail_thread_id", threadID).Interface("labels", labels).Msg("Gmail threads.get response")

	th := &GmailThread{ID: threadID, State: ThreadState{Archived: true}}
	for _, m := range resp.Messages {
		if hasLabel(m.LabelIds, "INBOX") {
			th.State.Archived = false
		}
		if hasLabel(m.LabelIds, "UNREAD") {
			th.State.Unread = true
		}
		// Gmail returns a thread's messages oldest first (observed; the
		// reference states no order).
		if !hasLabel(m.LabelIds, "DRAFT") {
			th.newestMessageID = m.Id
		}
	}
	return th, nil
}

// Apply changes the thread, as last read by Get, to the target state, making
// only the changes that differ. Archive and read act on the whole thread, as
// Gmail's own buttons do; unread marks the newest message only, as Gmail's
// "mark as unread" does.
func (g *GmailThreads) Apply(ctx context.Context, th *GmailThread, target ThreadState) error {
	var add, remove []string
	if target.Archived != th.State.Archived {
		if target.Archived {
			remove = append(remove, "INBOX")
		} else {
			add = append(add, "INBOX")
		}
	}
	if th.State.Unread && !target.Unread {
		remove = append(remove, "UNREAD")
	}
	markUnread := !th.State.Unread && target.Unread
	if len(add) == 0 && len(remove) == 0 && !markUnread {
		return nil
	}

	svc, err := g.service(ctx)
	if err != nil {
		return err
	}
	if len(add) > 0 || len(remove) > 0 {
		g.Log.Debug().Str("gmail_thread_id", th.ID).Strs("add", add).Strs("remove", remove).Msg("Gmail threads.modify request")
		req := &gmail.ModifyThreadRequest{AddLabelIds: add, RemoveLabelIds: remove}
		if _, err := svc.Users.Threads.Modify("me", th.ID, req).Context(ctx).Do(); err != nil {
			return fmt.Errorf("threads.modify %s: %w", th.ID, err)
		}
	}
	if markUnread {
		if th.newestMessageID == "" {
			return fmt.Errorf("mark thread %s unread: it has no message besides drafts", th.ID)
		}
		g.Log.Debug().Str("gmail_thread_id", th.ID).Str("gmail_msg_id", th.newestMessageID).Msg("Gmail messages.modify request: add UNREAD")
		req := &gmail.ModifyMessageRequest{AddLabelIds: []string{"UNREAD"}}
		if _, err := svc.Users.Messages.Modify("me", th.newestMessageID, req).Context(ctx).Do(); err != nil {
			return fmt.Errorf("messages.modify %s: %w", th.newestMessageID, err)
		}
	}
	return nil
}

// IsGmailNotFound reports whether err is the Gmail API's 404, which for a
// thread means Gmail no longer has it.
func IsGmailNotFound(err error) bool {
	var gerr *googleapi.Error
	return errors.As(err, &gerr) && gerr.Code == http.StatusNotFound
}
