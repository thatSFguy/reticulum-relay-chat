package config

import (
	"strings"
	"testing"
)

// --- the LXMF display name --------------------------------------------

// The hub announces on two aspects. rrc.hub is a room to join;
// lxmf.delivery is a sender that never reads a reply. A messaging
// client lists the second beside real people, so under the hub's own
// name it is indistinguishable from a correspondent — and the hub shows
// up twice in an announce list, only one of which is true.
func TestLXMFNameIsDistinctFromTheHubName(t *testing.T) {
	c := HubConfig{Name: "thatSFguy"}
	if got := c.LXMFName(); got == c.Name {
		t.Errorf("LXMFName() = %q, same as the hub name — the two announces are indistinguishable", got)
	}
	if got := c.LXMFName(); !strings.Contains(got, "thatSFguy") {
		t.Errorf("LXMFName() = %q, want it to still identify the hub", got)
	}
	if got := c.LXMFName(); !strings.Contains(got, "notification") {
		t.Errorf("LXMFName() = %q, want it to say what the address is for", got)
	}
}

func TestLXMFNameIsConfigurable(t *testing.T) {
	c := HubConfig{Name: "thatSFguy", LXMFDisplayName: "hub alerts"}
	if got := c.LXMFName(); got != "hub alerts" {
		t.Errorf("LXMFName() = %q, want the configured value", got)
	}
	// Whitespace-only is not a choice.
	c.LXMFDisplayName = "   "
	if got := c.LXMFName(); got != "thatSFguy"+LXMFNotifySuffix {
		t.Errorf("LXMFName() = %q, want the derived default", got)
	}
}

// An unnamed hub must still announce something meaningful rather than a
// bare suffix.
func TestLXMFNameHandlesAnUnnamedHub(t *testing.T) {
	c := HubConfig{}
	got := c.LXMFName()
	if got == "" || strings.HasPrefix(got, LXMFNotifySuffix) {
		t.Errorf("LXMFName() = %q for an unnamed hub", got)
	}
}

// --- mention defaults --------------------------------------------------

// Both were off, which meant "@nick" silently did nothing on every hub
// whose operator had not read the config closely enough to find them.
// A feature nobody can discover is a feature nobody has.
func TestMentionsAreOnByDefault(t *testing.T) {
	d := DefaultsForTest()
	if !d.MentionNotify {
		t.Error("mention_notify defaults off; @nick does nothing on an untuned hub")
	}
	if !d.MentionLXMF {
		t.Error("mention_lxmf defaults off; a mention is held but nothing tells the person to look")
	}
}

// The directory is what makes an absent peer addressable, so a hub that
// notifies must have somewhere to keep it.
func TestTheMentionDefaultsAreInternallyConsistent(t *testing.T) {
	d := DefaultsForTest()
	if d.MentionNotify && d.PeerRegistryPath == "" {
		t.Error("mention_notify is on but there is nowhere to persist the peer directory")
	}
	if d.MentionLXMF && !d.MentionNotify {
		t.Error("mention_lxmf is on without mention_notify, which does nothing")
	}
	// Keepalive is what lets the hub tell present from absent. With it
	// at zero, mention_notify is inert — the reason those defaults
	// changed in v0.2.0.
	if d.MentionNotify && (d.PingInterval.Duration <= 0 || d.PingTimeout.Duration <= 0) {
		t.Error("mention_notify is on while keepalive is disabled; the hub cannot tell who left")
	}
	if d.MentionLXMF && d.LXMFName() == "" {
		t.Error("the hub would announce an lxmf.delivery destination with no name")
	}
}
