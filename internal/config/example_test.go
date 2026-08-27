package config

import (
	"path/filepath"
	"testing"
	"time"
)

// The shipped example is the first thing an operator edits, so it has to
// parse and to mean what its comments say. Nothing else checks that the
// two stay in step as keys are added.
func TestShippedExampleConfigLoads(t *testing.T) {
	path := filepath.Join("..", "..", "configs", "rrc-hub.example.toml")
	c, err := Load(path)
	if err != nil {
		t.Fatalf("the shipped example config does not load: %v", err)
	}

	if c.Hub.HistoryEnabled {
		t.Error("the example enables history; it must ship off so an upgrade never starts retaining conversation on its own")
	}
	if got, want := c.Hub.HistoryRetention.Duration, 7*24*time.Hour; got != want {
		t.Errorf("example history_retention = %s, want %s to match the documented default", got, want)
	}
	if c.Hub.HistoryReplayCount <= 0 {
		t.Error("example history_replay_count must leave the join replay on")
	}
	if c.Hub.HistoryPath == "" || c.Hub.HistoryMaxTotalBytes <= 0 {
		t.Error("example history storage settings are incomplete")
	}
}

// An operator who enables history without tuning it must still get
// bounded, working behavior rather than an unbounded store.
func TestHistoryDefaultsFillInEveryUnsetKnob(t *testing.T) {
	h := HubConfig{HistoryEnabled: true}
	applyHistoryDefaults(&h)

	if h.HistoryPath == "" {
		t.Error("HistoryPath left empty")
	}
	if h.HistoryRetention.Duration != 7*24*time.Hour {
		t.Errorf("HistoryRetention = %s, want seven days", h.HistoryRetention.Duration)
	}
	if h.HistoryMaxBytesPerRoom <= 0 || h.HistoryMaxTotalBytes <= 0 {
		t.Error("storage caps left unbounded")
	}
	if h.HistoryReplayCount <= 0 || h.HistoryReplayBytes <= 0 {
		t.Error("join replay bounds left unset")
	}
	if h.HistoryPullCount <= 0 || h.HistoryPullBytes <= 0 {
		t.Error("/history bounds left unset")
	}
}

// A negative replay count is an operator saying "no automatic replay",
// not an invitation to substitute the default.
func TestANegativeReplayCountDisablesTheJoinReplay(t *testing.T) {
	h := HubConfig{HistoryEnabled: true, HistoryReplayCount: -1}
	applyHistoryDefaults(&h)

	if h.HistoryReplayCount != 0 {
		t.Errorf("HistoryReplayCount = %d, want 0 (replay off)", h.HistoryReplayCount)
	}
}
