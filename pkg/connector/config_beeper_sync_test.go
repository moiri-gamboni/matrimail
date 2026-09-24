package connector

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	up "go.mau.fi/util/configupgrade"
	"gopkg.in/yaml.v3"
	"maunium.net/go/mautrix/bridgev2"
)

// The Beeper sync is off unless api_url is set, and once it is set, a token
// the bridge cannot read stops startup with the key to fix, rather than
// leaving the sync failing quietly on every tick.
func TestStart_BeeperSyncConfig(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	readable := filepath.Join(dir, "token")
	if err := os.WriteFile(readable, []byte("tok"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		cfg     BeeperSyncConfig
		wantErr string
	}{
		{"off", BeeperSyncConfig{}, ""},
		{"off, other keys ignored", BeeperSyncConfig{TokenFile: filepath.Join(dir, "missing")}, ""},
		{"on", BeeperSyncConfig{APIURL: "http://127.0.0.1:23373", TokenFile: readable}, ""},
		{"no token file", BeeperSyncConfig{APIURL: "http://127.0.0.1:23373"}, "network.beeper_sync.token_file"},
		{"unreadable token file", BeeperSyncConfig{APIURL: "http://127.0.0.1:23373", TokenFile: filepath.Join(dir, "missing")}, "network.beeper_sync.token_file"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ec := &EmailConnector{Bridge: &bridgev2.Bridge{Log: zerolog.Nop()}}
			ec.Config.BeeperSync = tc.cfg
			err := ec.Start(context.Background())
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("Start: %v; want no error", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Errorf("Start: %v; want an error naming %s", err, tc.wantErr)
			}
		})
	}
}

// interval_seconds defaults to a minute when unset.
func TestBeeperSyncInterval(t *testing.T) {
	t.Parallel()
	if got := (BeeperSyncConfig{}).Interval(); got.Seconds() != 60 {
		t.Errorf("default interval = %v; want 60s", got)
	}
	if got := (BeeperSyncConfig{IntervalSeconds: 15}).Interval(); got.Seconds() != 15 {
		t.Errorf("interval = %v; want 15s", got)
	}
}

// An existing config's beeper_sync block survives the upgrade the bridge
// runs on its config at every start.
func TestUpgradeConfig_KeepsBeeperSync(t *testing.T) {
	t.Parallel()
	var base, old yaml.Node
	if err := yaml.Unmarshal([]byte(ExampleConfig), &base); err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal([]byte("beeper_sync:\n  api_url: http://127.0.0.1:23373\n  token_file: /run/beeper-token\n  interval_seconds: 15\n"), &old); err != nil {
		t.Fatal(err)
	}
	upgradeConfig(up.NewHelper(&base, &old))

	out, err := yaml.Marshal(&base)
	if err != nil {
		t.Fatal(err)
	}
	var cfg Config
	if err := yaml.Unmarshal(out, &cfg); err != nil {
		t.Fatal(err)
	}
	want := BeeperSyncConfig{APIURL: "http://127.0.0.1:23373", TokenFile: "/run/beeper-token", IntervalSeconds: 15}
	if cfg.BeeperSync != want {
		t.Errorf("upgraded beeper_sync = %+v; want %+v", cfg.BeeperSync, want)
	}
}
