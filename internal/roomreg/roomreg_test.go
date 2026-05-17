package roomreg

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

func TestRegistryRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rooms.toml")
	const now = 1000.0

	want := map[string]*RoomRecord{
		"#lobby": {
			Name:          "#lobby",
			Founder:       "abc123",
			Topic:         "welcome to the lobby",
			Moderated:     true,
			InviteOnly:    true,
			TopicOpsOnly:  true,
			NoOutsideMsgs: true,
			Private:       true,
			Key:           "s3cret",
			Operators:     []string{"abc123", "def456"},
			Voiced:        []string{"111aaa"},
			Bans:          []string{"badbad"},
			Invited:       map[string]float64{"future01": 5000.0},
			LastUsedTS:    1234.5,
		},
	}

	if err := SaveRegistry(path, want, now); err != nil {
		t.Fatalf("SaveRegistry: %v", err)
	}

	got, err := LoadRegistry(path, now)
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round-trip mismatch:\n got = %#v\nwant = %#v", got["#lobby"], want["#lobby"])
	}
}

func TestLoadRegistryMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nope.toml")
	got, err := LoadRegistry(path, 0)
	if err != nil {
		t.Fatalf("LoadRegistry missing: unexpected error %v", err)
	}
	if got == nil {
		t.Fatal("LoadRegistry missing: want non-nil empty map, got nil")
	}
	if len(got) != 0 {
		t.Fatalf("LoadRegistry missing: want empty map, got %d entries", len(got))
	}
}

func TestLoadKlinesMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nope.toml")
	got, err := LoadKlines(path)
	if err != nil {
		t.Fatalf("LoadKlines missing: unexpected error %v", err)
	}
	if got != nil {
		t.Fatalf("LoadKlines missing: want nil slice, got %#v", got)
	}
}

func TestMalformedRegistryIsError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rooms.toml")
	if err := os.WriteFile(path, []byte("this is = = not toml"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := LoadRegistry(path, 0); err == nil {
		t.Fatal("LoadRegistry: want error for malformed file, got nil")
	}
}

func TestExpiredInvitesDroppedOnLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rooms.toml")
	const now = 2000.0

	// Save with both an expired and a valid invite, using a past
	// "now" so SaveRegistry keeps both entries on disk.
	in := map[string]*RoomRecord{
		"#room": {
			Name:    "#room",
			Founder: "aaa",
			Invited: map[string]float64{
				"expired": 1500.0, // <= now
				"valid":   9000.0, // > now
			},
		},
	}
	if err := SaveRegistry(path, in, 0); err != nil {
		t.Fatalf("SaveRegistry: %v", err)
	}

	got, err := LoadRegistry(path, now)
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	inv := got["#room"].Invited
	if _, ok := inv["expired"]; ok {
		t.Error("expired invite was not dropped on load")
	}
	if _, ok := inv["valid"]; !ok {
		t.Error("valid invite was dropped on load")
	}
}

func TestExpiredInvitesDroppedOnSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rooms.toml")
	const now = 2000.0

	in := map[string]*RoomRecord{
		"#room": {
			Name:    "#room",
			Founder: "aaa",
			Invited: map[string]float64{
				"expired": 1500.0,
				"valid":   9000.0,
			},
		},
	}
	// Save with current "now" -> expired entry must not reach disk.
	if err := SaveRegistry(path, in, now); err != nil {
		t.Fatalf("SaveRegistry: %v", err)
	}

	// Load with a "now" earlier than the expired entry's expiry so
	// load-time pruning cannot mask a save-time bug.
	got, err := LoadRegistry(path, 0)
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	inv := got["#room"].Invited
	if _, ok := inv["expired"]; ok {
		t.Error("expired invite was persisted to disk by SaveRegistry")
	}
	if _, ok := inv["valid"]; !ok {
		t.Error("valid invite missing after save/load")
	}
}

func TestKlinesDedupAndSort(t *testing.T) {
	path := filepath.Join(t.TempDir(), "klines.toml")

	in := []string{"ccc", "aaa", "BBB", "aaa", "0xCCC"}
	if err := SaveKlines(path, in); err != nil {
		t.Fatalf("SaveKlines: %v", err)
	}

	got, err := LoadKlines(path)
	if err != nil {
		t.Fatalf("LoadKlines: %v", err)
	}

	want := []string{"aaa", "bbb", "ccc"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("klines dedup/sort: got %#v, want %#v", got, want)
	}
	if !sort.StringsAreSorted(got) {
		t.Error("klines not sorted")
	}
}

func TestHexNormalizedOnLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rooms.toml")

	doc := `[rooms."#hex"]
founder = "0xABC123"
operators = ["0xDEF456", "FFF000"]
voiced = ["0xAaBb"]
bans = ["0xDEAD"]

[rooms."#hex".invited]
"0xFEED99" = 9999.0
`
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, err := LoadRegistry(path, 0)
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	rec := got["#hex"]
	if rec.Founder != "abc123" {
		t.Errorf("founder not normalized: %q", rec.Founder)
	}
	if !reflect.DeepEqual(rec.Operators, []string{"def456", "fff000"}) {
		t.Errorf("operators not normalized: %#v", rec.Operators)
	}
	if !reflect.DeepEqual(rec.Voiced, []string{"aabb"}) {
		t.Errorf("voiced not normalized: %#v", rec.Voiced)
	}
	if !reflect.DeepEqual(rec.Bans, []string{"dead"}) {
		t.Errorf("bans not normalized: %#v", rec.Bans)
	}
	if _, ok := rec.Invited["feed99"]; !ok {
		t.Errorf("invited key not normalized: %#v", rec.Invited)
	}
}

func TestKlinesHexNormalizedOnLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "klines.toml")
	doc := `banned_identities = ["0xABC", "DEF"]` + "\n"
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, err := LoadKlines(path)
	if err != nil {
		t.Fatalf("LoadKlines: %v", err)
	}
	want := []string{"abc", "def"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("klines normalize: got %#v, want %#v", got, want)
	}
}

func TestSaveCreatesParentDirs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "deep", "rooms.toml")
	if err := SaveRegistry(path, map[string]*RoomRecord{}, 0); err != nil {
		t.Fatalf("SaveRegistry into missing dirs: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("registry file not created: %v", err)
	}
}
