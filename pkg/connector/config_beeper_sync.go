package connector

import (
	"fmt"
	"os"
	"time"
)

// BeeperSyncConfig is the on-disk shape of the beeper_sync: block.
type BeeperSyncConfig struct {
	// APIURL is the Beeper Desktop API, as served by Beeper Server or Beeper
	// Desktop on the bridge's host. Empty turns the sync off.
	APIURL string `yaml:"api_url"`
	// TokenFile holds the API's bearer token. Read on every call, so a
	// replaced token needs no restart.
	TokenFile string `yaml:"token_file"`
	// IntervalSeconds is how often Beeper is checked for changes. Default 60.
	IntervalSeconds int `yaml:"interval_seconds"`
}

// Interval returns how often the sync runs, a minute when unset.
func (b BeeperSyncConfig) Interval() time.Duration {
	if b.IntervalSeconds <= 0 {
		return time.Minute
	}
	return time.Duration(b.IntervalSeconds) * time.Second
}

// validate checks, when the sync is on, that its token can be read, so a
// wrong path stops startup instead of failing every sync quietly.
func (b BeeperSyncConfig) validate() error {
	if b.APIURL == "" {
		return nil
	}
	if b.TokenFile == "" {
		return fmt.Errorf("network.beeper_sync.token_file is required when network.beeper_sync.api_url is set")
	}
	if _, err := os.ReadFile(b.TokenFile); err != nil {
		return fmt.Errorf("network.beeper_sync.token_file: %w", err)
	}
	return nil
}
