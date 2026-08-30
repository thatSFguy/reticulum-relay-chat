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
