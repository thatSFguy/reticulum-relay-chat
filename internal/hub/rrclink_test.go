package hub

import (
	"strings"
	"testing"

	"github.com/thatSFguy/reticulum-relay-chat/internal/config"
)

const testDest = "43c8adb1172377a76b8f9ba41bb85e5c"

// --- the format ---------------------------------------------------------

func TestLinkRendersTheNomadNetTargetSyntax(t *testing.T) {
	got := RRCLink{DestHash: testDest, Room: "ops"}.String()
	want := "rrc@" + testDest + ":/room/ops"
	if got != want {
		t.Errorf("link = %q, want %q", got, want)
	}
	if hub := (RRCLink{DestHash: testDest}).String(); hub != "rrc@"+testDest {
		t.Errorf("hub link = %q, want %q", hub, "rrc@"+testDest)
	}
}

func TestParseAcceptsEveryFormSpec1163Defines(t *testing.T) {
	cases := []struct {
		in   string
		room string
	}{
		// The shorthand, which is the canonical form we emit.
		{"rrc@" + testDest + ":/room/ops", "ops"},
		{"rrc@" + testDest, ""},
		// The expanded aspect, as lxmf.delivery@ is accepted for lxmf@.
		{"rrc.hub@" + testDest + ":/room/ops", "ops"},
		// The bare-hash form, which NomadNet also accepts.
		{testDest, ""},
		{testDest + ":/room/ops", "ops"},
		// §11.6.3: normalize hash hex to lower case.
		{"RRC@" + strings.ToUpper(testDest) + ":/room/ops", "ops"},
	}
	for _, c := range cases {
		got, err := ParseRRCLink(c.in)
		if err != nil {
			t.Errorf("ParseRRCLink(%q): %v", c.in, err)
			continue
		}
		if got.DestHash != testDest {
			t.Errorf("ParseRRCLink(%q).DestHash = %q, want %q", c.in, got.DestHash, testDest)
		}
		if got.Room != c.room {
			t.Errorf("ParseRRCLink(%q).Room = %q, want %q", c.in, got.Room, c.room)
		}
	}
}

// §11.6.3 is explicit that forgiving hash parsing creates aliases for
// one destination and invites cache poisoning.
func TestParseIsStrictAboutTheHash(t *testing.T) {
	bad := []string{
		"",
		"rrc@",
		"rrc@deadbeef",                           // too short
		"rrc@" + testDest + "ff",                 // too long
		"rrc@43c8:adb1:1723:77a7:6b8f:9ba4:1bb8", // embedded separators
		"rrc@zzc8adb1172377a76b8f9ba41bb85e5c",   // not hex
		"lxmf@" + testDest,                       // a different aspect
		"nnn@" + testDest,                        // a different aspect
		"rrc@" + testDest + ":/page/index.mu",    // a path we do not define
		"rrc@" + testDest + ":/room/",            // names no room
		"rrc@" + testDest + ":/room/%",           // truncated escape
		"rrc@" + testDest + ":/room/%zz",         // bad escape
	}
	for _, in := range bad {
		if got, err := ParseRRCLink(in); err == nil {
			t.Errorf("ParseRRCLink(%q) accepted it as %+v", in, got)
		}
	}
}

// Room names are arbitrary UTF-8 and a link is a whitespace-delimited
// token somebody pastes out of a message, so anything structural or
// splittable has to survive a round trip.
func TestLinksRoundTripAwkwardRoomNames(t *testing.T) {
	for _, room := range []string{
		"ops",
		"two words",
		"a/b",
		"at@sign",
		"colon:name",
		"percent%name",
		"café",
		"日本語",
		"tab\tinside",
	} {
		link := RRCLink{DestHash: testDest, Room: room}.String()
		if strings.ContainsAny(link, " \t\n") {
			t.Errorf("link for %q contains whitespace and cannot be pasted: %q", room, link)
		}
		got, err := ParseRRCLink(link)
		if err != nil {
			t.Errorf("round trip of %q (%q): %v", room, link, err)
			continue
		}
		if got.Room != room {
			t.Errorf("round trip of %q gave %q", room, got.Room)
		}
	}
}

// A link carrying a stray sigil must not address a room JOIN cannot.
func TestLinkRoomIsNormalizedLikeEveryOtherRoomName(t *testing.T) {
	got, err := ParseRRCLink("rrc@" + testDest + ":/room/%23lobby")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.Room != "lobby" {
		t.Errorf("room = %q, want %q", got.Room, "lobby")
	}
}

// --- the hub emits them -------------------------------------------------

func linkHub(t *testing.T) *Hub {
	t.Helper()
	h := quietHubCfg(config.HubConfig{Name: "Test Hub"})
	h.SetDestHash(mustHexBytes(t, testDest))
	return h
}

func mustHexBytes(t *testing.T, s string) []byte {
	t.Helper()
	b := make([]byte, len(s)/2)
	for i := range b {
		hi, ok1 := unhexByte(s[2*i])
		lo, ok2 := unhexByte(s[2*i+1])
		if !ok1 || !ok2 {
			t.Fatalf("bad hex %q", s)
		}
		b[i] = hi<<4 | lo
	}
	return b
}

func TestLinkCommandPrintsAPastableLink(t *testing.T) {
	h := linkHub(t)
	s, link, id := connectKeyed(t, h, 0xA1, "alice")
	join(t, s, id, "ops", "")
	cmd(t, s, id, "ops", "/link")

	got := allNoticeText(t, link)
	want := "rrc@" + testDest + ":/room/ops"
	if !strings.Contains(got, want) {
		t.Errorf("/link did not print %q:\n%s", want, got)
	}
}

func TestLinkCommandTakesARoomArgumentAndTheSigil(t *testing.T) {
	h := linkHub(t)
	s, link, id := connectKeyed(t, h, 0xA1, "alice")
	cmd(t, s, id, "", "/link #ops")

	if got := allNoticeText(t, link); !strings.Contains(got, ":/room/ops") {
		t.Errorf("/link #ops did not resolve to ops:\n%s", got)
	}
}

func TestLinkWithNoRoomGivesTheHub(t *testing.T) {
	h := linkHub(t)
	s, link, id := connectKeyed(t, h, 0xA1, "alice")
	cmd(t, s, id, "", "/link")

	got := allNoticeText(t, link)
	if !strings.Contains(got, "rrc@"+testDest) {
		t.Errorf("/link outside a room did not give the hub link:\n%s", got)
	}
	if strings.Contains(got, ":/room/") {
		t.Errorf("/link outside a room invented a room:\n%s", got)
	}
}

// A link missing the hash is not a shorter link, it is a wrong one, and
// it would be pasted around as though it worked.
func TestNoLinkIsEmittedWhenTheHubDoesNotKnowItsAddress(t *testing.T) {
	h := quietHubCfg(config.HubConfig{Name: "Test Hub"})
	s, link, id := connectKeyed(t, h, 0xA1, "alice")
	cmd(t, s, id, "", "/link")

	got := allNoticeText(t, link)
	if strings.Contains(got, "rrc@") {
		t.Errorf("a hub with no destination hash emitted a link anyway:\n%s", got)
	}
	if !strings.Contains(got, "does not know its own address") {
		t.Errorf("/link did not explain itself:\n%s", got)
	}
}
