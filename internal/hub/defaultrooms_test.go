package hub

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thatSFguy/reticulum-relay-chat/internal/config"
	"github.com/thatSFguy/reticulum-relay-chat/internal/roomreg"
)

func roomsHub(t *testing.T, tune func(*config.HubConfig)) *Hub {
	t.Helper()
	cfg := config.HubConfig{
		RoomRegistryPath: filepath.Join(t.TempDir(), "rooms.toml"),
		DefaultRooms:     []string{"lobby"},
	}
	if tune != nil {
		tune(&cfg)
	}
	return quietHubCfg(cfg)
}

func roomOf(h *Hub, name string) *Room {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.rooms[name]
}

// A brand new hub should be somewhere to arrive, not an empty prompt.
func TestANewHubHasALobby(t *testing.T) {
	h := roomsHub(t, nil)
	r := roomOf(h, "lobby")
	if r == nil {
		t.Fatal("no #lobby on a fresh hub")
	}
	// Registered is the part that makes it real when nobody is in it:
	// /list shows only registered rooms, and an unregistered one is
	// dropped the moment its last member parts.
	if !r.registered {
		t.Error("#lobby must be registered or it evaporates between visitors")
	}
	if r.private {
		t.Error("#lobby must not be private — /list would hide it")
	}
}

// It has to survive being empty, which is the state it spends most of
// its life in.
func TestTheDefaultLobbySurvivesItsLastMemberLeaving(t *testing.T) {
	h := roomsHub(t, nil)
	s, _, id := connectKeyed(t, h, 0xA1, "alice")
	join(t, s, id, "lobby", "")
	if r := roomOf(h, "lobby"); r == nil || !r.hasMember(s) {
		t.Fatal("setup: alice did not join #lobby")
	}
	s.Close()
	if roomOf(h, "lobby") == nil {
		t.Error("#lobby disappeared when its last member left")
	}
}

// The hub owns it, not whoever arrives first — otherwise the first
// visitor to a public hub silently acquires the keys to the lobby.
func TestTheDefaultLobbyHasNoFounderOrOps(t *testing.T) {
	h := roomsHub(t, nil)
	r := roomOf(h, "lobby")
	if r.founder != "" {
		t.Errorf("founder = %q, want none", r.founder)
	}
	if len(r.ops) != 0 {
		t.Errorf("ops = %v, want none", r.ops)
	}

	s, _, id := connectKeyed(t, h, 0xA1, "alice")
	join(t, s, id, "lobby", "")
	if r := roomOf(h, "lobby"); len(r.ops) != 0 {
		t.Errorf("joining granted ops %v; the first arrival must not inherit the room", r.ops)
	}
}

// A room that already exists carries a topic, operators, modes and bans
// somebody set deliberately. Startup must not reset any of it.
func TestAnExistingRoomIsNotClobbered(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rooms.toml")
	recs := map[string]*roomreg.RoomRecord{
		"lobby": {
			Founder:   "aabbccddeeff00112233445566778899",
			Topic:     "house rules apply",
			Operators: []string{"aabbccddeeff00112233445566778899"},
			Moderated: true,
		},
	}
	if err := roomreg.SaveRegistry(path, recs, 0); err != nil {
		t.Fatal(err)
	}

	h := quietHubCfg(config.HubConfig{
		RoomRegistryPath: path,
		DefaultRooms:     []string{"lobby"},
	})
	r := roomOf(h, "lobby")
	if r == nil {
		t.Fatal("#lobby missing")
	}
	if r.topic != "house rules apply" {
		t.Errorf("topic = %q — startup overwrote a room somebody configured", r.topic)
	}
	if r.founder == "" || len(r.ops) != 1 {
		t.Errorf("founder=%q ops=%v — existing ownership was dropped", r.founder, r.ops)
	}
	if !r.moderated {
		t.Error("+m was reset on restart")
	}
}

// An operator who does not want one must be able to say so, and an
// absent key must not be read as that.
func TestDefaultRoomsCanBeTurnedOff(t *testing.T) {
	h := roomsHub(t, func(c *config.HubConfig) { c.DefaultRooms = []string{} })
	if roomOf(h, "lobby") != nil {
		t.Error("default_rooms = [] still created #lobby")
	}
	h.mu.Lock()
	n := len(h.rooms)
	h.mu.Unlock()
	if n != 0 {
		t.Errorf("created %d rooms, want none", n)
	}
}

// "#lobby" is what an operator will write, and it means lobby.
func TestAConfiguredNameMayCarryAHash(t *testing.T) {
	h := roomsHub(t, func(c *config.HubConfig) { c.DefaultRooms = []string{"#lobby", " #town ", "help"} })
	for _, want := range []string{"lobby", "town", "help"} {
		if roomOf(h, want) == nil {
			t.Errorf("no room %q", want)
		}
	}
	if roomOf(h, "#lobby") != nil {
		t.Error(`created a room literally named "#lobby"`)
	}
}

// A name the hub would have refused over JOIN must not reach
// rooms.toml, where it would make the registry unloadable next boot.
func TestAnUnusableDefaultNameIsSkipped(t *testing.T) {
	h := roomsHub(t, func(c *config.HubConfig) {
		c.Limits.MaxRoomNameBytes = 8
		c.DefaultRooms = []string{"lobby", strings.Repeat("x", 40), "\xff\xfe"}
	})
	if roomOf(h, "lobby") == nil {
		t.Error("a usable name was skipped alongside the bad ones")
	}
	h.mu.Lock()
	n := len(h.rooms)
	h.mu.Unlock()
	if n != 1 {
		t.Errorf("created %d rooms, want only the usable one", n)
	}
}

// The rooms must be in rooms.toml, or the next start recreates them and
// anything set on them in between is lost.
func TestTheDefaultLobbyIsPersisted(t *testing.T) {
	h := roomsHub(t, nil)
	h.flushRegistry()

	recs, err := roomreg.LoadRegistry(h.cfg.RoomRegistryPath, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := recs["lobby"]; !ok {
		t.Errorf("#lobby was not written to rooms.toml (got %v)", recs)
	}
}

// --- pruning -----------------------------------------------------------

// ensureDefaultRooms runs only at startup, so pruning a default room
// deletes something the operator declared should always exist and
// leaves it gone until the next restart. /list then advertises nothing
// to the next visitor — the empty-prompt problem default_rooms exists
// to solve.
func TestADefaultRoomIsNotPruned(t *testing.T) {
	h := roomsHub(t, func(c *config.HubConfig) {
		c.DefaultRooms = []string{"lobby"}
		c.RoomRegistryPruneAfter = config.Duration{Duration: time.Hour}
		c.RoomRegistryPruneInterval = config.Duration{Duration: time.Minute}
	})
	ageEveryRoom(h)

	h.doPrune()

	if roomOf(h, "lobby") == nil {
		t.Error("the default room was pruned; it is gone until the hub restarts")
	}
}

// The exemption must not become "registered rooms are never pruned".
func TestANonDefaultRoomIsStillPruned(t *testing.T) {
	h := roomsHub(t, func(c *config.HubConfig) {
		c.DefaultRooms = []string{"lobby"}
		c.RoomRegistryPruneAfter = config.Duration{Duration: time.Hour}
		c.RoomRegistryPruneInterval = config.Duration{Duration: time.Minute}
	})
	h.mu.Lock()
	r := newRoom("ephemeral")
	r.registered = true
	h.rooms["ephemeral"] = r
	h.mu.Unlock()
	ageEveryRoom(h)

	h.doPrune()

	if roomOf(h, "ephemeral") != nil {
		t.Error("a stale registered room that is not a default was kept")
	}
	if roomOf(h, "lobby") == nil {
		t.Error("the default room was pruned")
	}
}

// The exemption reads the live config, so what an operator writes is
// matched the same way a JOIN would be.
func TestTheExemptionNormalizesTheConfiguredName(t *testing.T) {
	h := roomsHub(t, func(c *config.HubConfig) {
		c.DefaultRooms = []string{"#Lobby"}
		c.RoomRegistryPruneAfter = config.Duration{Duration: time.Hour}
		c.RoomRegistryPruneInterval = config.Duration{Duration: time.Minute}
	})
	ageEveryRoom(h)

	h.doPrune()

	if roomOf(h, "lobby") == nil {
		t.Error(`default_rooms = ["#Lobby"] did not exempt the room it created`)
	}
}

// Reading the live config rather than flagging the Room at creation is
// what makes this true: an operator who drops a room from default_rooms
// is saying the hub need not keep it, and it becomes prunable again
// without a restart.
func TestARoomDroppedFromDefaultsBecomesPrunableAgain(t *testing.T) {
	h := roomsHub(t, func(c *config.HubConfig) {
		c.DefaultRooms = []string{"lobby"}
		c.RoomRegistryPruneAfter = config.Duration{Duration: time.Hour}
		c.RoomRegistryPruneInterval = config.Duration{Duration: time.Minute}
	})
	ageEveryRoom(h)

	h.cfg.DefaultRooms = nil // as a config reload would leave it
	h.doPrune()

	if roomOf(h, "lobby") != nil {
		t.Error("a room no longer listed in default_rooms was still exempt")
	}
}

// ageEveryRoom makes every room look long-idle and empty.
func ageEveryRoom(h *Hub) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.rooms {
		r.lastUsedTS = h.nowUnix() - 48*3600
	}
}
