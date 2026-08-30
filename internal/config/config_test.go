package config

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// --- the LXMF notification name ---------------------------------------

// A messaging client lists the hub's lxmf.delivery destination beside
// real people. Under the hub's own name it is indistinguishable from a
// correspondent — and the hub appears twice in an announce list, only
// one of which is true.
func TestLXMFNameMarksTheAddressAsOneWay(t *testing.T) {
	c := HubConfig{Name: "MichMesh RRC Hub"}
	got := c.LXMFName()
	if got != "MichMesh RRC Hub"+LXMFNoReplySuffix {
		t.Errorf("LXMFName() = %q, want the hub name plus %q", got, LXMFNoReplySuffix)
	}
	if got == c.Name {
		t.Error("the notification address announces under the hub's own name")
	}
}

// The suffix is the part that carries the meaning. A name truncated to
// "MichMesh RRC Hu" is odd; one that silently loses "(noreply)" is a
// hub posing as somebody you can talk to.
func TestTheSuffixIsNeverWhatGetsDropped(t *testing.T) {
	for _, name := range []string{
		strings.Repeat("A", 200),
		strings.Repeat("word ", 40),
		strings.Repeat("é", 100), // 2 bytes each
		strings.Repeat("🎉", 60),  // 4 bytes each
	} {
		got := HubConfig{Name: name}.LXMFName()
		if !strings.HasSuffix(got, LXMFNoReplySuffix) {
			t.Errorf("name of %d bytes lost the suffix: %q", len(name), got)
		}
		if len(got) > lxmfNameMaxBytes {
			t.Errorf("name of %d bytes produced %d bytes, over the %d budget: %q",
				len(name), len(got), lxmfNameMaxBytes, got)
		}
		// Upstream does dn.decode("utf-8") on this (SPEC §9.3): invalid
		// UTF-8 makes the name vanish entirely rather than look odd.
		if !utf8.ValidString(got) {
			t.Errorf("truncation produced invalid UTF-8 for a %d-byte name: %q", len(name), got)
		}
	}
}

// Truncating mid-word can leave a trailing space, which reads as a
// mistake rather than a truncation.
func TestTruncationDoesNotLeaveDanglingWhitespace(t *testing.T) {
	got := HubConfig{Name: strings.Repeat("ab ", 40)}.LXMFName()
	if strings.Contains(got, " "+LXMFNoReplySuffix) {
		t.Errorf("truncated name has a dangling space before the suffix: %q", got)
	}
}

// A short name is left exactly alone.
func TestAShortNameIsNotTruncated(t *testing.T) {
	c := HubConfig{Name: "thatSFguy"}
	if got := c.LXMFName(); got != "thatSFguy"+LXMFNoReplySuffix {
		t.Errorf("LXMFName() = %q", got)
	}
}

// An unnamed hub must still announce something, not a bare suffix.
func TestLXMFNameHandlesAnUnnamedHub(t *testing.T) {
	for _, name := range []string{"", "   ", "\t"} {
		got := HubConfig{Name: name}.LXMFName()
		if got == LXMFNoReplySuffix || strings.HasPrefix(got, LXMFNoReplySuffix) {
			t.Errorf("unnamed hub announces as %q", got)
		}
		if !strings.HasSuffix(got, LXMFNoReplySuffix) {
			t.Errorf("unnamed hub lost the suffix: %q", got)
		}
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
