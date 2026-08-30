package hub

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/thatSFguy/reticulum-relay-chat/internal/config"
	"github.com/thatSFguy/reticulum-relay-chat/internal/roomreg"
)

// Room names carry no leading "#".
//
// The sigil is decoration a client adds when it renders a room, exactly
// as this hub's own log format string does. But people type it, and a
// client that passes the typed line straight through creates a room
// whose real name is "#lobby" — thereafter rendered "##lobby", a second
// room distinct from the one everybody else is in, with a name nobody
// can type consistently.

func TestNormalizeRoomName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"lobby", "lobby"},
		// Case-insensitive, as IRC channels are: the intro room was
		// once invisible to anyone who typed its name in lower case.
		{"Lobby", "lobby"},
		{"LOBBY", "lobby"},
		{"#WELCOME-INTRO", "welcome-intro"},
		{"CafÉ", "café"},
		{"#lobby", "lobby"},
		{"##lobby", "lobby"},
		{"  #lobby  ", "lobby"},
		{"# lobby", "lobby"},
		{"#", ""},
		{"###", ""},
		{"", ""},
		// Only LEADING sigils: a "#" inside a name is part of it.
		{"c#", "c#"},
		{"#c#", "c#"},
	}
	for _, c := range cases {
		if got := normalizeRoomName(c.in); got != c.want {
			t.Errorf("normalizeRoomName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// Normalization must not LAUNDER: strings.ToLower replaces malformed
// bytes with U+FFFD, which would turn a name audit A7 requires be
// rejected into one that passes. The name becomes a rooms.toml table
// key, and an invalid one makes the registry unloadable on next boot.
func TestNormalizationDoesNotLaunderInvalidUTF8(t *testing.T) {
	bad := string([]byte{0xff, 0xfe})
	if got := normalizeRoomName(bad); utf8ValidString(got) {
		t.Errorf("normalizeRoomName made invalid UTF-8 valid: %q -> %q", bad, got)
	}
}

func utf8ValidString(s string) bool { return utf8.ValidString(s) }

// Two people typing the same name in different cases must land in one
// room, not two.
func TestDifferentCasingIsOneRoom(t *testing.T) {
	h := quietHub()
	idA := bytes.Repeat([]byte{0xA1}, 16)
	idB := bytes.Repeat([]byte{0xB2}, 16)
	sa, _ := connect(t, h, idA)
	sb, _ := connect(t, h, idB)

	join(t, sa, idA, "Lobby", "")
	join(t, sb, idB, "#LOBBY", "")

	if got := h.RoomCount(); got != 1 {
		t.Fatalf("%d rooms exist; casing split them", got)
	}
	r := roomOf(h, "lobby")
	if r == nil {
		t.Fatal("no room named lobby")
	}
	h.mu.Lock()
	n := len(r.members)
	h.mu.Unlock()
	if n != 2 {
		t.Errorf("lobby has %d members, want 2", n)
	}
}

// The bug as a user meets it: joining "#lobby" must put you in the same
// room as joining "lobby", not in a second one.
func TestJoiningWithASigilLandsInTheSameRoom(t *testing.T) {
	h := quietHub()
	idA := bytes.Repeat([]byte{0xA1}, 16)
	idB := bytes.Repeat([]byte{0xB2}, 16)
	sa, _ := connect(t, h, idA)
	sb, linkB := connect(t, h, idB)

	join(t, sa, idA, "lobby", "")
	join(t, sb, idB, "#lobby", "")

	if got := h.RoomCount(); got != 1 {
		t.Fatalf("%d rooms exist; joining \"#lobby\" created a second one", got)
	}
	if roomOf(h, "#lobby") != nil {
		t.Error(`a room literally named "#lobby" was created`)
	}
	r := roomOf(h, "lobby")
	if r == nil {
		t.Fatal("no room named lobby")
	}
	h.mu.Lock()
	n := len(r.members)
	h.mu.Unlock()
	if n != 2 {
		t.Errorf("lobby has %d members, want 2 — the sigilled join went elsewhere", n)
	}

	// And the message path agrees: a MSG addressed to "#lobby" from a
	// client that prefixes must reach the room it joined. Normalizing
	// only at JOIN would give it a membership it could not use.
	say(t, sb, idB, "#lobby", "hello from a prefixing client")
	if h.RoomCount() != 1 {
		t.Error("a sigilled MSG created a second room")
	}
	h.mu.Lock()
	still := len(r.members)
	h.mu.Unlock()
	if still != 2 {
		t.Errorf("membership changed after a sigilled MSG: %d", still)
	}
	_ = linkB
}

// A command argument is the other way a room name is typed, and it is
// typed by a person rather than a client — so it carries the sigil more
// often, not less.
func TestCommandArgumentsAcceptTheSigil(t *testing.T) {
	h := quietHub()
	id := bytes.Repeat([]byte{0xA1}, 16)
	s, link := connect(t, h, id)
	join(t, s, id, "lobby", "")

	cmd(t, s, id, "lobby", "/who #lobby")
	got := allNoticeText(t, link)
	if !strings.Contains(got, "members in lobby") {
		t.Errorf("/who #lobby did not resolve to lobby:\n%s", got)
	}
	if strings.Contains(got, "members in #lobby") {
		t.Errorf("/who echoed the sigil back as part of the name:\n%s", got)
	}
}

func TestSlashJoinAcceptsTheSigil(t *testing.T) {
	h := quietHub()
	id := bytes.Repeat([]byte{0xA1}, 16)
	s, _ := connect(t, h, id)

	// The hub's own default greeting says "/join #lobby to start", so a
	// client that does not intercept the line must still land somewhere.
	cmd(t, s, id, "", "/join #lobby")
	if roomOf(h, "lobby") == nil {
		t.Fatal("/join #lobby did not create lobby")
	}
	if roomOf(h, "#lobby") != nil {
		t.Error(`/join #lobby created a room literally named "#lobby"`)
	}
}

// A rooms.toml written before this fix can hold a "#lobby" no client
// can now reach, because every JOIN for it resolves to "lobby".
func TestARegistryEntryWithASigilIsRetiredOnLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rooms.toml")
	if err := roomreg.SaveRegistry(path, map[string]*roomreg.RoomRecord{
		"#lobby": {Name: "#lobby", Topic: "stale", LastUsedTS: 1},
	}, 1); err != nil {
		t.Fatalf("seed registry: %v", err)
	}

	h := quietHubCfg(config.HubConfig{RoomRegistryPath: path})
	if roomOf(h, "#lobby") != nil {
		t.Error(`the hub loaded a room literally named "#lobby"`)
	}
	r := roomOf(h, "lobby")
	if r == nil {
		t.Fatal("the stale entry was dropped instead of normalized")
	}
	if r.topic != "stale" {
		t.Errorf("topic = %q, want the normalized entry to keep it", r.topic)
	}
}

// When both forms are present, the already-normalized entry is the one
// clients have been reaching, so it is the one that survives.
func TestAnAlreadyNormalizedRegistryEntryWins(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rooms.toml")
	if err := roomreg.SaveRegistry(path, map[string]*roomreg.RoomRecord{
		"#lobby": {Name: "#lobby", Topic: "stale", LastUsedTS: 1},
		"lobby":  {Name: "lobby", Topic: "live", LastUsedTS: 2},
	}, 2); err != nil {
		t.Fatalf("seed registry: %v", err)
	}

	h := quietHubCfg(config.HubConfig{RoomRegistryPath: path})
	r := roomOf(h, "lobby")
	if r == nil {
		t.Fatal("no room named lobby")
	}
	if r.topic != "live" {
		t.Errorf("topic = %q, want %q — the stale sigilled entry overwrote the live room", r.topic, "live")
	}
}
