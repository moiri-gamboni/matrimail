// Package beeper is a client for the few calls of the Beeper Desktop API
// (served by Beeper Desktop, or headless by Beeper Server) that mirror a
// chat's archived and unread state.
package beeper

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/rs/zerolog"
)

// Chat is the part of the API's chat object that the state sync reads.
type Chat struct {
	// ID is the chat's Matrix room ID.
	ID             string `json:"id"`
	IsArchived     bool   `json:"isArchived"`
	UnreadCount    int    `json:"unreadCount"`
	IsMarkedUnread bool   `json:"isMarkedUnread"`
}

// Unread reports whether the chat waits to be read: it holds unread messages,
// or it was marked unread by hand.
func (c Chat) Unread() bool {
	return c.UnreadCount > 0 || c.IsMarkedUnread
}

// APIError is a response outside 2xx, with the body the server sent.
type APIError struct {
	Method string
	Path   string
	Status int
	Body   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("beeper API %s %s: HTTP %d: %s", e.Method, e.Path, e.Status, e.Body)
}

// Client calls the Beeper Desktop API at BaseURL with the bearer token held
// in TokenFile. The file is read on every call, so a replaced token takes
// effect without a restart.
type Client struct {
	BaseURL   string
	TokenFile string
	HTTP      *http.Client
	Log       *zerolog.Logger
}

// AccountID returns the ID of the account a bridge login appears under
// (for a self-hosted bridge, "<bridge>_<login ID>").
func (c *Client) AccountID(ctx context.Context, loginID string) (string, error) {
	var accounts []struct {
		AccountID string `json:"accountID"`
		LoginID   string `json:"loginID"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/accounts", nil, nil, &accounts); err != nil {
		return "", err
	}
	for _, a := range accounts {
		if a.LoginID == loginID {
			return a.AccountID, nil
		}
	}
	return "", fmt.Errorf("no Beeper account has login ID %q", loginID)
}

// ListChats returns every chat of the account, archived ones included.
func (c *Client) ListChats(ctx context.Context, accountID string) ([]Chat, error) {
	var chats []Chat
	q := url.Values{"accountIDs": {accountID}, "limit": {"200"}}
	for {
		var page struct {
			Items        []Chat `json:"items"`
			HasMore      bool   `json:"hasMore"`
			OldestCursor string `json:"oldestCursor"`
		}
		if err := c.do(ctx, http.MethodGet, "/v1/chats", q, nil, &page); err != nil {
			return nil, err
		}
		c.Log.Debug().Interface("chats", page.Items).Bool("has_more", page.HasMore).Msg("Beeper API chat page")
		chats = append(chats, page.Items...)
		if !page.HasMore {
			return chats, nil
		}
		q.Set("cursor", page.OldestCursor)
		q.Set("direction", "before")
	}
}

// GetChat reads one chat.
func (c *Client) GetChat(ctx context.Context, chatID string) (Chat, error) {
	var chat Chat
	err := c.do(ctx, http.MethodGet, chatPath(chatID), nil, nil, &chat)
	c.Log.Debug().Interface("chat", chat).Msg("Beeper API chat")
	return chat, err
}

// SetArchived archives or unarchives a chat through the archive endpoint.
// PATCH /v1/chats/{id} also accepts isArchived and answers 200, but for a
// bridged chat Beeper Server's updateThread has no branch for it, so the
// chat stays as it was. The response is not read; GetChat confirms.
func (c *Client) SetArchived(ctx context.Context, chatID string, archived bool) error {
	return c.do(ctx, http.MethodPost, chatPath(chatID)+"/archive", nil, map[string]bool{"archived": archived}, nil)
}

// SetUnread marks a chat unread, or read. As with SetArchived, GetChat
// confirms.
func (c *Client) SetUnread(ctx context.Context, chatID string, unread bool) error {
	path := chatPath(chatID) + "/read"
	if unread {
		path = chatPath(chatID) + "/unread"
	}
	return c.do(ctx, http.MethodPost, path, nil, struct{}{}, nil)
}

func chatPath(chatID string) string {
	return "/v1/chats/" + url.PathEscape(chatID)
}

// do sends one request, logging it and the raw response at debug level, and
// decodes a 2xx body into out when out is non-nil.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, in, out any) error {
	token, err := os.ReadFile(c.TokenFile)
	if err != nil {
		return fmt.Errorf("read Beeper API token: %w", err)
	}
	u := strings.TrimRight(c.BaseURL, "/") + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var reqBody []byte
	if in != nil {
		if reqBody, err = json.Marshal(in); err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(reqBody))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	c.Log.Debug().Str("method", method).Str("url", u).RawJSON("body", jsonOrNull(reqBody)).Msg("Beeper API request")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("beeper API %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("beeper API %s %s: read response: %w", method, path, err)
	}
	// A successful body is not logged: chats carry mail subjects, addresses
	// and message previews. What was read from it is logged by the callers;
	// error bodies are the API's own messages and go into the error.
	c.Log.Debug().Str("method", method).Str("url", u).Int("status", resp.StatusCode).
		Int("bytes", len(respBody)).Msg("Beeper API response")

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &APIError{Method: method, Path: path, Status: resp.StatusCode, Body: string(respBody)}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(respBody, out); err != nil {
		return fmt.Errorf("beeper API %s %s: decode %d-byte response: %w", method, path, len(respBody), err)
	}
	return nil
}

func jsonOrNull(b []byte) []byte {
	if len(b) == 0 {
		return []byte("null")
	}
	return b
}
