package roomreg

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSweepStaleTempOnLoad verifies the audit-A18 cleanup: an orphaned
// .roomreg-*.tmp file (left by a crash between CreateTemp and Rename) is
// removed when the registry is next loaded.
func TestSweepStaleTempOnLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rooms.toml")
	orphan := filepath.Join(dir, ".roomreg-stale.tmp")
	if err := os.WriteFile(orphan, []byte("junk"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadRegistry(path, 0); err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Error("LoadRegistry must sweep orphaned .roomreg-*.tmp files (audit A18)")
	}
}

// TestSaveRegistryRoundTrip exercises the rewritten marshalAtomic
// (in-memory encode + re-parse + atomic rename) end to end.
func TestSaveRegistryRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "rooms.toml") // sub/ created by marshalAtomic
	in := map[string]*RoomRecord{
		"lobby": {
			Name:       "lobby",
			Founder:    "aabbccddaabbccddaabbccddaabbccdd",
			Topic:      "welcome",
			LastUsedTS: 1,
		},
	}
	if err := SaveRegistry(path, in, 0); err != nil {
		t.Fatalf("SaveRegistry: %v", err)
	}
	out, err := LoadRegistry(path, 0)
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	if out["lobby"] == nil || out["lobby"].Topic != "welcome" {
		t.Errorf("registry did not round-trip: %+v", out)
	}
}
