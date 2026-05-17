package hub

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/thatSFguy/reticulum-relay-chat/internal/config"
)

// --- A7: invalid-UTF-8 room names ------------------------------------

func TestValidRoomRejectsInvalidUTF8(t *testing.T) {
	h := quietHub()
	s := &Session{hub: h}

	if err := s.validRoom(string([]byte{0xff, 0xfe})); err == nil {
		t.Error("validRoom must reject an invalid-UTF-8 room name (audit A7)")
	}
	if err := s.validRoom("lobby"); err != nil {
		t.Errorf("validRoom rejected a valid room name: %v", err)
	}
}

// --- A10: debounced registry writes ----------------------------------

func TestRegistryWriteIsDebounced(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rooms.toml")
	h := quietHubCfg(config.HubConfig{RoomRegistryPath: path})

	// Mutate a registered room and mark the registry dirty, as any of
	// the ~12 mutation sites do.
	h.mu.Lock()
	r := newRoom("lobby")
	r.registered = true
	r.founder = "aabbccddaabbccddaabbccddaabbccdd"
	h.rooms["lobby"] = r
	h.markRegistryDirtyLocked()
	h.mu.Unlock()

	// Debounced (audit A10): the mutation must not have written to disk.
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("a registry mutation must be debounced, not written immediately")
	}

	// flushRegistry performs the actual deferred write.
	h.flushRegistry()
	if _, err := os.Stat(path); err != nil {
		t.Errorf("flushRegistry must write the registry to disk: %v", err)
	}
}
