package config

import (
	"os"
	"path/filepath"
	"strings"
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

// The example config explains each feature in prose and then sets it.
// When a default flips, the setting is easy to remember and the prose
// above it is easy to forget — and a comment saying "Off by default"
// over a setting that is on does not merely age, it misleads somebody
// reading the file to decide whether to turn something on.
//
// This caught exactly that: mention_notify was flipped on while the
// section header above it still said "Off by default".
func TestTheExampleAgreesWithTheDefaultsItDocuments(t *testing.T) {
	path := filepath.Join("..", "..", "configs", "rrc-hub.example.toml")
	c, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	d := defaults().Hub

	// Flags whose comments in that file make a claim about the default.
	for _, f := range []struct {
		key            string
		example, deflt bool
	}{
		{"mention_notify", c.Hub.MentionNotify, d.MentionNotify},
		{"mention_lxmf", c.Hub.MentionLXMF, d.MentionLXMF},
		{"unique_nicks", c.Hub.UniqueNicks, d.UniqueNicks},
		{"history_enabled", c.Hub.HistoryEnabled, d.HistoryEnabled},
	} {
		if f.example != f.deflt {
			t.Errorf("example sets %s = %v but the default is %v — one of the two is "+
				"lying to whoever reads the file", f.key, f.example, f.deflt)
		}
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)

	// A prose claim about being off, in a file where it is on.
	for _, claim := range []string{"Off by default", "off by default"} {
		if !strings.Contains(text, claim) {
			continue
		}
		// history_enabled is genuinely off and may say so; nothing else
		// in this file is entitled to that sentence.
		if strings.Count(text, claim) > 1 {
			t.Errorf("%q appears %d times; only history_enabled is off by default, so "+
				"one of those is stale", claim, strings.Count(text, claim))
		}
	}
}
