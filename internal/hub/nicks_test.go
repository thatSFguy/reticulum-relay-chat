package hub

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/thatSFguy/reticulum-relay-chat/internal/config"
	"github.com/thatSFguy/reticulum-relay-chat/internal/rrc"
)

// nickHub is a hub with unique nicks AND the peer directory on, which
// is what makes nick ownership outlive a disconnection.
func nickHub(t *testing.T, tune func(*config.HubConfig)) *Hub {
	t.Helper()
	return mentionHub(t, func(c *config.HubConfig) {
		c.UniqueNicks = true
		if tune != nil {
			tune(c)
		}
	})
}

func nickOf(s *Session) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.nick
}

// --- the core rule ------------------------------------------------------

func TestSecondClaimantGetsASuffix(t *testing.T) {
	h := nickHub(t, nil)
	a, _, _ := connectKeyed(t, h, 0xA1, "sam")
	b, _, _ := connectKeyed(t, h, 0xB2, "sam")
	c, _, _ := connectKeyed(t, h, 0xC3, "sam")

	if got := nickOf(a); got != "sam" {
		t.Errorf("first claimant = %q, want %q", got, "sam")
	}
	if got := nickOf(b); got != "sam1" {
		t.Errorf("second claimant = %q, want %q", got, "sam1")
	}
	if got := nickOf(c); got != "sam2" {
		t.Errorf("third claimant = %q, want %q", got, "sam2")
	}
}

// Nothing in RRC can carry "you asked for sam and you are sam1", so it
// is said in a NOTICE — the one thing every deployed client renders.
func TestARenamedPeerIsTold(t *testing.T) {
	h := nickHub(t, nil)
	connectKeyed(t, h, 0xA1, "sam")
	_, linkB, _ := connectKeyed(t, h, 0xB2, "sam")

	got := allNoticeText(t, linkB)
	if !strings.Contains(got, "already taken") || !strings.Contains(got, "sam1") {
		t.Errorf("the renamed peer was not told:\n%s", got)
	}
}

func TestTheFirstClaimantIsNotTold(t *testing.T) {
	h := nickHub(t, nil)
	_, linkA, _ := connectKeyed(t, h, 0xA1, "sam")
	if got := allNoticeText(t, linkA); strings.Contains(got, "already taken") {
		t.Errorf("the peer who got the name it asked for was told otherwise:\n%s", got)
	}
}

// Ownership belongs to an IDENTITY, not a session. Somebody who has
// been "sam" here for a year must not come back to find a stranger
// holding it.
func TestOwnershipSurvivesDisconnection(t *testing.T) {
	h := nickHub(t, nil)
	a, _, _ := connectKeyed(t, h, 0xA1, "sam")
	a.Close()

	// A different identity asking for it while sam is away is refused
	// the name.
	b, _, _ := connectKeyed(t, h, 0xB2, "sam")
	if got := nickOf(b); got == "sam" {
		t.Error("a stranger took an absent peer's nick")
	}

	// And sam gets it back.
	a2, _, _ := connectKeyed(t, h, 0xA1, "sam")
	if got := nickOf(a2); got != "sam" {
		t.Errorf("the owner reconnected and got %q, want %q", got, "sam")
	}
}

// --- the second entry point ---------------------------------------------

// A nick also arrives on any MSG carrying K_NICK, which the hub adopts.
// Enforcing only at HELLO would be theatre.
func TestAMessageCannotStealATakenNick(t *testing.T) {
	h := nickHub(t, nil)
	connectKeyed(t, h, 0xA1, "sam")
	b, linkB, idB := connectKeyed(t, h, 0xB2, "bob")
	join(t, b, idB, "lobby", "")

	// bob asserts "sam" on a message rather than in HELLO.
	env := clientEnvelope(rrc.TMsg, idB, "lobby", "hello")
	want := "sam"
	env.Nick = &want
	b.OnInbound(encode(t, env))

	if got := nickOf(b); got == "sam" {
		t.Error("a per-message K_NICK stole a nick that HELLO would have refused")
	}
	if got := allNoticeText(t, linkB); !strings.Contains(got, "already taken") {
		t.Errorf("the peer was not told its message-asserted nick was refused:\n%s", got)
	}
}

// A client that re-sends the same taken nick on every message must not
// be told every time.
func TestTheRenameNoticeIsNotRepeatedPerMessage(t *testing.T) {
	h := nickHub(t, nil)
	connectKeyed(t, h, 0xA1, "sam")
	b, linkB, idB := connectKeyed(t, h, 0xB2, "bob")
	join(t, b, idB, "lobby", "")

	for i := 0; i < 5; i++ {
		env := clientEnvelope(rrc.TMsg, idB, "lobby", "hello")
		want := "sam"
		env.Nick = &want
		b.OnInbound(encode(t, env))
	}

	n := 0
	for _, notice := range noticesOn(t, linkB) {
		if strings.Contains(notice, "already taken") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("the rename notice was sent %d times across 5 messages, want 1", n)
	}
}

// --- the point of the whole thing ---------------------------------------

// Two people called sam is why "@sam are you there" reached neither of
// them and said nothing about having failed.
func TestAMentionNowResolvesWhereItUsedToDecline(t *testing.T) {
	h := nickHub(t, nil)
	sa, _, idA := connectKeyed(t, h, 0xA1, "alice")
	sb, _, idB := connectKeyed(t, h, 0xB2, "sam")
	connectKeyed(t, h, 0xC3, "sam") // becomes sam1
	join(t, sa, idA, "lobby", "")
	join(t, sb, idB, "lobby", "")

	h.mu.Lock()
	targets := h.resolveMentionsLocked("@sam are you there", "lobby", hexOfID(idA))
	h.mu.Unlock()

	if len(targets) != 1 {
		t.Fatalf("@sam resolved to %d peers, want exactly 1", len(targets))
	}
	if targets[0].idHex != hexOfID(idB) {
		t.Errorf("@sam resolved to %s, want the first claimant %s", targets[0].idHex, hexOfID(idB))
	}
}

// --- bounds and the off switch ------------------------------------------

// The suffix is what makes the name correct, so the base is trimmed to
// make room for it rather than the suffix being dropped.
func TestASuffixedNickStillFitsTheLimit(t *testing.T) {
	h := nickHub(t, func(c *config.HubConfig) {
		c.Limits = config.LimitsConfig{
			MaxNickBytes: 4, MaxRoomNameBytes: 64, MaxMsgBodyBytes: 4096,
			MaxRoomsPerSession: 16, RateLimitMsgsPerMin: 240,
		}
	})
	connectKeyed(t, h, 0xA1, "samm")
	b, _, _ := connectKeyed(t, h, 0xB2, "samm")

	got := nickOf(b)
	if len(got) > 4 {
		t.Errorf("granted nick %q is %d bytes, over max_nick_bytes=4", got, len(got))
	}
	if got == "samm" {
		t.Error("the second claimant kept the taken name")
	}
	if !strings.HasSuffix(got, "1") {
		t.Errorf("granted nick %q dropped the suffix that makes it unique", got)
	}
}

func TestNickWithSuffixTrimsOnARuneBoundary(t *testing.T) {
	// "café" is 5 bytes; trimming to 4 must not split the é.
	got := nickWithSuffix("café", "1", 5)
	if !strings.HasSuffix(got, "1") {
		t.Fatalf("got %q, want it to end in the suffix", got)
	}
	if !utf8.ValidString(got) {
		t.Errorf("got %q, which is not valid UTF-8", got)
	}
}

func TestUniqueNicksCanBeSwitchedOff(t *testing.T) {
	h := mentionHub(t, func(c *config.HubConfig) { c.UniqueNicks = false })
	connectKeyed(t, h, 0xA1, "sam")
	b, _, _ := connectKeyed(t, h, 0xB2, "sam")

	if got := nickOf(b); got != "sam" {
		t.Errorf("with unique_nicks off, second claimant = %q, want %q", got, "sam")
	}
}

// The default has to be on: the alternative is not "names are flexible"
// but "@mentions silently reach nobody".
func TestUniqueNicksIsOnByDefault(t *testing.T) {
	if !config.DefaultsForTest().UniqueNicks {
		t.Error("unique_nicks defaults off, so mention ambiguity ships as the default behaviour")
	}
}

// The directory is where ownership lives once the session is gone, and
// what a mention resolves against for somebody who is away. A rename on
// the message path has to reach it, or the granted name looks unclaimed
// the moment its owner disconnects.
func TestARenameOnTheMessagePathReachesTheDirectory(t *testing.T) {
	h := nickHub(t, nil)
	connectKeyed(t, h, 0xA1, "sam")
	b, _, idB := connectKeyed(t, h, 0xB2, "mallory")
	join(t, b, idB, "lobby", "")

	env := clientEnvelope(rrc.TMsg, idB, "lobby", "hello")
	want := "sam"
	env.Nick = &want
	b.OnInbound(encode(t, env))

	granted := nickOf(b)
	if granted == "mallory" || granted == "sam" {
		t.Fatalf("unexpected granted nick %q", granted)
	}

	h.mu.Lock()
	filed := h.peers[hexOfID(idB)].Nick
	h.mu.Unlock()
	if filed != granted {
		t.Errorf("directory holds %q while the hub broadcasts %q", filed, granted)
	}

	// And the granted name is still owned once its holder is gone.
	b.Close()
	h.mu.Lock()
	owner := h.nickOwnerLocked(granted)
	h.mu.Unlock()
	if owner != hexOfID(idB) {
		t.Errorf("%q looked unclaimed after its owner disconnected (owner=%q)", granted, owner)
	}
}
