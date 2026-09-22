package email

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"golang.org/x/oauth2"
)

func TestGeneratePKCE(t *testing.T) {
	t.Parallel()
	v, c, err := GeneratePKCE()
	if err != nil {
		t.Fatalf("GeneratePKCE: %v", err)
	}
	if len(v) < 43 || len(v) > 128 {
		t.Errorf("verifier length %d outside RFC 7636 range [43,128]", len(v))
	}
	// challenge must be SHA-256(verifier) base64url-no-padding
	want := sha256.Sum256([]byte(v))
	wantB64 := base64.RawURLEncoding.EncodeToString(want[:])
	if c != wantB64 {
		t.Errorf("challenge mismatch:\n got  %q\n want %q", c, wantB64)
	}
	// Two calls must produce distinct verifiers
	v2, _, err := GeneratePKCE()
	if err != nil {
		t.Fatal(err)
	}
	if v == v2 {
		t.Errorf("two GeneratePKCE calls returned the same verifier (random source broken)")
	}
}

func TestGenerateState(t *testing.T) {
	t.Parallel()
	s1, err := GenerateState()
	if err != nil {
		t.Fatal(err)
	}
	s2, err := GenerateState()
	if err != nil {
		t.Fatal(err)
	}
	if s1 == s2 {
		t.Errorf("two GenerateState calls returned the same value")
	}
	// must be base64url-safe (no '=', '+', '/')
	if strings.ContainsAny(s1, "=+/") {
		t.Errorf("state %q contains non-url-safe characters", s1)
	}
}

func TestScopesForMode(t *testing.T) {
	t.Parallel()
	cases := []struct {
		mode string
		want []string
	}{
		{"modify", []string{GmailModifyScope, GmailSendScope}},
		{"full", []string{GmailFullScope}},
		{"", []string{GmailModifyScope, GmailSendScope}},      // safe default
		{"bogus", []string{GmailModifyScope, GmailSendScope}}, // unknown → safe default
	}
	for _, tc := range cases {
		got := ScopesForMode(tc.mode)
		if len(got) != len(tc.want) {
			t.Errorf("ScopesForMode(%q): len=%d, want %d", tc.mode, len(got), len(tc.want))
			continue
		}
		for i, s := range got {
			if s != tc.want[i] {
				t.Errorf("ScopesForMode(%q)[%d]=%q, want %q", tc.mode, i, s, tc.want[i])
			}
		}
	}
}

func TestBuildAuthURL(t *testing.T) {
	t.Parallel()
	cfg := GmailOAuthConfig{ClientID: "id", ClientSecret: "sec"}
	u, err := BuildAuthURL(cfg, "http://127.0.0.1:8888/callback", "STATE", "CHALLENGE", "user@example.com",
		[]string{GmailModifyScope, GmailSendScope})
	if err != nil {
		t.Fatalf("BuildAuthURL: %v", err)
	}
	parsed, err := url.Parse(u)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	q := parsed.Query()
	checks := map[string]string{
		"client_id":             "id",
		"redirect_uri":          "http://127.0.0.1:8888/callback",
		"response_type":         "code",
		"access_type":           "offline",
		"prompt":                "consent",
		"state":                 "STATE",
		"code_challenge":        "CHALLENGE",
		"code_challenge_method": "S256",
		"login_hint":            "user@example.com",
	}
	for k, want := range checks {
		if got := q.Get(k); got != want {
			t.Errorf("auth URL %s = %q, want %q", k, got, want)
		}
	}
	wantScope := GmailModifyScope + " " + GmailSendScope
	if got := q.Get("scope"); got != wantScope {
		t.Errorf("scope = %q, want %q", got, wantScope)
	}
}

func TestBuildAuthURL_MissingFields(t *testing.T) {
	t.Parallel()
	for _, missing := range []string{"client_id", "redirect_uri", "code_challenge", "state"} {
		cfg := GmailOAuthConfig{ClientID: "id", ClientSecret: "sec"}
		args := struct{ ru, st, ch string }{"r", "s", "c"}
		switch missing {
		case "client_id":
			cfg.ClientID = ""
		case "redirect_uri":
			args.ru = ""
		case "state":
			args.st = ""
		case "code_challenge":
			args.ch = ""
		}
		if _, err := BuildAuthURL(cfg, args.ru, args.st, args.ch, "", nil); err == nil {
			t.Errorf("BuildAuthURL with missing %s: expected error", missing)
		}
	}
}

func TestExchangeCode_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.FormValue("grant_type"); got != "authorization_code" {
			t.Errorf("grant_type = %q", got)
		}
		if r.FormValue("code") != "CODE" || r.FormValue("code_verifier") != "VERIFIER" || r.FormValue("redirect_uri") != "http://127.0.0.1:1234/callback" {
			t.Errorf("unexpected form: code=%q verifier=%q redirect=%q",
				r.FormValue("code"), r.FormValue("code_verifier"), r.FormValue("redirect_uri"))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(authCodeTokenResp{
			AccessToken:  "AT",
			RefreshToken: "RT",
			TokenType:    "Bearer",
			ExpiresIn:    3600,
			Scope:        "https://mail.google.com/",
		})
	}))
	defer srv.Close()
	withFakeGoogle(t, srv)

	cfg := GmailOAuthConfig{ClientID: "id", ClientSecret: "sec"}
	tok, err := ExchangeCode(context.Background(), cfg, "CODE", "VERIFIER", "http://127.0.0.1:1234/callback")
	if err != nil {
		t.Fatalf("ExchangeCode: %v", err)
	}
	if tok.AccessToken != "AT" || tok.RefreshToken != "RT" {
		t.Errorf("unexpected token: %+v", tok)
	}
}

func TestExchangeCode_NoRefreshToken(t *testing.T) {
	// Google sometimes returns no refresh_token (e.g. when the user has
	// already consented and didn't get re-prompted). We refuse to persist
	// because that would set up a 1h-then-die account.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(authCodeTokenResp{
			AccessToken: "AT",
			TokenType:   "Bearer",
			ExpiresIn:   3600,
		})
	}))
	defer srv.Close()
	withFakeGoogle(t, srv)

	cfg := GmailOAuthConfig{ClientID: "id", ClientSecret: "sec"}
	if _, err := ExchangeCode(context.Background(), cfg, "C", "V", "http://127.0.0.1:1/callback"); err == nil {
		t.Errorf("ExchangeCode without refresh_token: expected error, got nil")
	}
}

func TestExchangeCode_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"bad code"}`))
	}))
	defer srv.Close()
	withFakeGoogle(t, srv)

	cfg := GmailOAuthConfig{ClientID: "id", ClientSecret: "sec"}
	_, err := ExchangeCode(context.Background(), cfg, "C", "V", "http://127.0.0.1:1/callback")
	if err == nil {
		t.Fatal("expected error from bad-code response")
	}
	if !strings.Contains(err.Error(), "invalid_grant") {
		t.Errorf("error %q does not mention invalid_grant", err.Error())
	}
}

func TestExchangeRefreshToken_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.FormValue("grant_type"); got != "refresh_token" {
			t.Errorf("grant_type = %q", got)
		}
		if r.FormValue("refresh_token") != "RT" {
			t.Errorf("refresh_token = %q", r.FormValue("refresh_token"))
		}
		_, _ = w.Write([]byte(`{"access_token":"AT","token_type":"Bearer","expires_in":3600}`))
	}))
	defer srv.Close()
	withFakeGoogle(t, srv)

	cfg := GmailOAuthConfig{ClientID: "id", ClientSecret: "sec"}
	tok, err := ExchangeRefreshToken(context.Background(), cfg, "RT")
	if err != nil {
		t.Fatalf("ExchangeRefreshToken: %v", err)
	}
	if tok.AccessToken != "AT" || tok.RefreshToken != "RT" {
		t.Errorf("unexpected token: %+v", tok)
	}
}

func TestRevokeToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.FormValue("token") != "TOK" {
			t.Errorf("token = %q", r.FormValue("token"))
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	withFakeGoogle(t, srv)

	if err := RevokeToken(context.Background(), "TOK"); err != nil {
		t.Errorf("RevokeToken: %v", err)
	}
}

func TestRevokeToken_AlreadyInvalid(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_token"}`))
	}))
	defer srv.Close()
	withFakeGoogle(t, srv)

	// Already-revoked tokens are fine — desired end state is reached.
	if err := RevokeToken(context.Background(), "TOK"); err != nil {
		t.Errorf("RevokeToken on already-invalid token: %v", err)
	}
}

func TestIsRefreshError(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"plain", errors.New("something else"), false},
		{"invalid_grant string", errors.New("oauth2: cannot fetch token: invalid_grant"), true},
		{"revoked string", errors.New("Token has been expired or revoked."), true},
		{"unauthorized_client string", errors.New("server returned unauthorized_client"), true},
		{"RetrieveError invalid_grant", &oauth2.RetrieveError{ErrorCode: "invalid_grant"}, true},
		{"RetrieveError invalid_token", &oauth2.RetrieveError{ErrorCode: "invalid_token"}, true},
		{"RetrieveError other", &oauth2.RetrieveError{ErrorCode: "temporarily_unavailable"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsRefreshError(tc.err); got != tc.want {
				t.Errorf("IsRefreshError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
