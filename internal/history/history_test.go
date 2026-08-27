package history

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// clock is a settable stand-in for time.Now.
type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func testStore(t *testing.T, opts Options) (*Store, *clock) {
	t.Helper()
	c := &clock{t: time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)}
	opts.Now = c.now
	s, err := Open(t.TempDir(), opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s, c
}

func msg(body string) Record {
	return Record{
		TimestampMs: 1_700_000_000_000,
		MsgID:       []byte{1, 2, 3, 4, 5, 6, 7, 8},
		Src:         bytes.Repeat([]byte{0xA1}, 16),
		Nick:        "alice",
		Type:        20,
		Body:        body,
	}
}

func appendAll(t *testing.T, s *Store, room string, bodies ...string) {
	t.Helper()
	for _, b := range bodies {
		if err := s.Append(room, msg(b)); err != nil {
			t.Fatalf("Append(%q): %v", b, err)
		}
	}
}

func bodies(recs []Record) []string {
	out := make([]string, len(recs))
	for i, r := range recs {
		out[i] = r.Body
	}
	return out
}

func TestRecentReturnsRecordsOldestFirst(t *testing.T) {
	s, _ := testStore(t, Options{})
	appendAll(t, s, "#lobby", "one", "two", "three")

	got, err := s.Recent("#lobby", Query{})
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if want := []string{"one", "two", "three"}; !equalStrings(bodies(got), want) {
		t.Errorf("got %v, want %v", bodies(got), want)
	}
}

// A round trip must preserve every field a replay reconstructs the
// envelope from — MsgID above all, since that is what lets a client
// recognise a replayed message it already rendered.
func TestRoundTripPreservesTheEnvelopeFields(t *testing.T) {
	s, c := testStore(t, Options{})
	in := msg("hello")
	if err := s.Append("#lobby", in); err != nil {
		t.Fatalf("Append: %v", err)
	}

	got, err := s.Recent("#lobby", Query{})
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d records, want 1", len(got))
	}
	out := got[0]
	if !bytes.Equal(out.MsgID, in.MsgID) {
		t.Errorf("MsgID = %x, want %x", out.MsgID, in.MsgID)
	}
	if !bytes.Equal(out.Src, in.Src) {
		t.Errorf("Src = %x, want %x", out.Src, in.Src)
	}
	if out.TimestampMs != in.TimestampMs || out.Nick != in.Nick ||
		out.Type != in.Type || out.Body != in.Body {
		t.Errorf("record round-tripped as %+v, want %+v", out, in)
	}
	if out.StoredAtMs != c.now().UnixMilli() {
		t.Errorf("StoredAtMs = %d, want the hub clock %d", out.StoredAtMs, c.now().UnixMilli())
	}
}

func TestRecentLimitTakesTheNewestRecords(t *testing.T) {
	s, _ := testStore(t, Options{})
	appendAll(t, s, "#lobby", "one", "two", "three", "four")

	got, err := s.Recent("#lobby", Query{Limit: 2})
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if want := []string{"three", "four"}; !equalStrings(bodies(got), want) {
		t.Errorf("got %v, want %v", bodies(got), want)
	}
}

// The byte budget is what actually protects a slow radio link, so it
// must bind before the count does.
func TestRecentStopsAtTheByteBudget(t *testing.T) {
	s, _ := testStore(t, Options{})
	appendAll(t, s, "#lobby", "aaaa", "bbbb", "cccc")

	got, err := s.Recent("#lobby", Query{MaxBytes: 9})
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if want := []string{"bbbb", "cccc"}; !equalStrings(bodies(got), want) {
		t.Errorf("got %v, want %v", bodies(got), want)
	}
}

// A budget smaller than the newest message must still yield that
// message: an empty result would read as "nothing was said".
func TestRecentAlwaysYieldsAtLeastOneRecord(t *testing.T) {
	s, _ := testStore(t, Options{})
	appendAll(t, s, "#lobby", "a very long message indeed")

	got, err := s.Recent("#lobby", Query{MaxBytes: 1})
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d records, want 1", len(got))
	}
}

func TestRecentSpansDayFiles(t *testing.T) {
	s, c := testStore(t, Options{})
	appendAll(t, s, "#lobby", "monday")
	c.advance(24 * time.Hour)
	appendAll(t, s, "#lobby", "tuesday")
	c.advance(24 * time.Hour)
	appendAll(t, s, "#lobby", "wednesday")

	got, err := s.Recent("#lobby", Query{})
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if want := []string{"monday", "tuesday", "wednesday"}; !equalStrings(bodies(got), want) {
		t.Errorf("got %v, want %v", bodies(got), want)
	}
}

// Retention is the headline promise: a week of history, and nothing
// older leaks into a replay even before Prune has run.
func TestRecentExcludesRecordsPastRetention(t *testing.T) {
	s, c := testStore(t, Options{Retention: 7 * 24 * time.Hour})
	appendAll(t, s, "#lobby", "ancient")
	c.advance(8 * 24 * time.Hour)
	appendAll(t, s, "#lobby", "fresh")

	got, err := s.Recent("#lobby", Query{})
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if want := []string{"fresh"}; !equalStrings(bodies(got), want) {
		t.Errorf("got %v, want %v", bodies(got), want)
	}
}

func TestPruneDeletesExpiredDayFiles(t *testing.T) {
	s, c := testStore(t, Options{Retention: 7 * 24 * time.Hour})
	appendAll(t, s, "#lobby", "ancient")
	c.advance(30 * 24 * time.Hour)
	appendAll(t, s, "#lobby", "fresh")

	if err := s.Prune(); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	days, err := s.dayFilesLocked(s.roomDir("#lobby"))
	if err != nil {
		t.Fatalf("dayFiles: %v", err)
	}
	if len(days) != 1 {
		t.Fatalf("got %d day files after prune, want 1", len(days))
	}
	if days[0].day != "2026-09-26" {
		t.Errorf("surviving day file is %s, want the recent one", days[0].day)
	}
}

// A day file is a whole UTC day, so it may only be deleted once its
// entire day has aged out — otherwise a message could vanish hours
// before its retention actually elapsed.
func TestPruneKeepsADayFileThatIsStillPartlyInsideTheWindow(t *testing.T) {
	s, c := testStore(t, Options{Retention: 7 * 24 * time.Hour})
	appendAll(t, s, "#lobby", "borderline")
	c.advance(7*24*time.Hour - time.Hour)

	if err := s.Prune(); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	days, _ := s.dayFilesLocked(s.roomDir("#lobby"))
	if len(days) != 1 {
		t.Errorf("day file deleted while still inside the retention window")
	}
}

func TestPruneRemovesEmptiedRoomDirectories(t *testing.T) {
	s, c := testStore(t, Options{Retention: 24 * time.Hour})
	appendAll(t, s, "#gone", "old")
	c.advance(30 * 24 * time.Hour)

	if err := s.Prune(); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	rooms, err := s.Rooms()
	if err != nil {
		t.Fatalf("Rooms: %v", err)
	}
	if len(rooms) != 0 {
		t.Errorf("Rooms = %v, want none after everything expired", rooms)
	}
}

// The per-room cap must evict oldest-first and never take the current
// day's file, or a chatty room would lose the conversation in progress.
func TestRoomCapEvictsOldestButKeepsTheCurrentDay(t *testing.T) {
	s, c := testStore(t, Options{MaxBytesPerRoom: 200})
	for i := 0; i < 4; i++ {
		appendAll(t, s, "#loud", strings.Repeat("x", 100))
		c.advance(24 * time.Hour)
	}
	appendAll(t, s, "#loud", "today")

	days, err := s.dayFilesLocked(s.roomDir("#loud"))
	if err != nil {
		t.Fatalf("dayFiles: %v", err)
	}
	if len(days) == 0 {
		t.Fatal("room cap deleted every day file")
	}
	var total int64
	for _, d := range days {
		total += d.size
	}
	if total > 200 {
		t.Errorf("room holds %d bytes, cap is 200", total)
	}
	got, err := s.Recent("#loud", Query{})
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(got) == 0 || got[len(got)-1].Body != "today" {
		t.Error("the current day's messages were evicted")
	}
}

func TestTotalCapEvictsOldestAcrossRooms(t *testing.T) {
	s, c := testStore(t, Options{MaxTotalBytes: 300})
	appendAll(t, s, "#old", strings.Repeat("a", 200))
	c.advance(24 * time.Hour)
	appendAll(t, s, "#new", strings.Repeat("b", 200))

	if err := s.Prune(); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	oldRecs, _ := s.Recent("#old", Query{})
	newRecs, _ := s.Recent("#new", Query{})
	if len(oldRecs) != 0 {
		t.Error("the older room's history survived the store-wide cap")
	}
	if len(newRecs) == 0 {
		t.Error("the newer room's history was evicted before the older one")
	}
}

func TestOversizedRecordsAreRejected(t *testing.T) {
	s, _ := testStore(t, Options{MaxRecordBytes: 64})
	err := s.Append("#lobby", msg(strings.Repeat("x", 500)))
	if err == nil {
		t.Fatal("an oversized record was accepted")
	}
	if got, _ := s.Recent("#lobby", Query{}); len(got) != 0 {
		t.Error("a rejected record was still written")
	}
}

// Room names are arbitrary client-supplied text. None of it may reach
// the filesystem as a path component.
func TestHostileRoomNamesStayInsideTheStore(t *testing.T) {
	s, _ := testStore(t, Options{})
	hostile := []string{
		"../../etc/passwd",
		"/absolute",
		"..",
		".",
		"with/separator",
		"with\x00nul",
		"CON",
	}
	for _, room := range hostile {
		if err := s.Append(room, msg("payload")); err != nil {
			t.Fatalf("Append(%q): %v", room, err)
		}
		got, err := s.Recent(room, Query{})
		if err != nil {
			t.Fatalf("Recent(%q): %v", room, err)
		}
		if len(got) != 1 || got[0].Body != "payload" {
			t.Errorf("room %q did not round-trip", room)
		}
	}

	// Everything written must live under the store root, and every
	// directory in it must decode back to a room name.
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	seen := map[string]bool{}
	for _, e := range entries {
		room, ok := RoomFromDir(e.Name())
		if !ok {
			t.Errorf("unexpected entry %q in the store root", e.Name())
			continue
		}
		seen[room] = true
	}
	for _, room := range hostile {
		if !seen[room] {
			t.Errorf("room %q is missing from the store root — it was written elsewhere", room)
		}
	}
}

// Rooms differing only in case must not share a transcript, which they
// would if names were used as path components on a case-insensitive
// filesystem.
func TestRoomsDifferingOnlyByCaseAreDistinct(t *testing.T) {
	s, _ := testStore(t, Options{})
	appendAll(t, s, "#Lobby", "upper")
	appendAll(t, s, "#lobby", "lower")

	upper, _ := s.Recent("#Lobby", Query{})
	lower, _ := s.Recent("#lobby", Query{})
	if len(upper) != 1 || upper[0].Body != "upper" {
		t.Errorf("#Lobby = %v", bodies(upper))
	}
	if len(lower) != 1 || lower[0].Body != "lower" {
		t.Errorf("#lobby = %v", bodies(lower))
	}
}

// Appends are not fsynced, so a partial trailing record is the expected
// state after an unclean shutdown, not a corruption to fail on.
func TestATruncatedTailDoesNotLoseTheRecordsBeforeIt(t *testing.T) {
	s, c := testStore(t, Options{})
	appendAll(t, s, "#lobby", "first", "second")

	path := filepath.Join(s.roomDir("#lobby"), c.now().UTC().Format(dayLayout)+".log")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	// Append a header promising more bytes than follow.
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], 4096)
	if err := os.WriteFile(path, append(append(data, hdr[:]...), 0x01, 0x02), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, err := s.Recent("#lobby", Query{})
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if want := []string{"first", "second"}; !equalStrings(bodies(got), want) {
		t.Errorf("got %v, want %v", bodies(got), want)
	}
}

// A corrupt length prefix must not drive a huge allocation.
func TestAnAbsurdLengthPrefixIsIgnored(t *testing.T) {
	s, c := testStore(t, Options{})
	appendAll(t, s, "#lobby", "good")

	path := filepath.Join(s.roomDir("#lobby"), c.now().UTC().Format(dayLayout)+".log")
	data, _ := os.ReadFile(path)
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], 0xFFFFFFFF)
	if err := os.WriteFile(path, append(data, hdr[:]...), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, err := s.Recent("#lobby", Query{})
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if want := []string{"good"}; !equalStrings(bodies(got), want) {
		t.Errorf("got %v, want %v", bodies(got), want)
	}
}

func TestDropRemovesARoomsTranscript(t *testing.T) {
	s, _ := testStore(t, Options{})
	appendAll(t, s, "#lobby", "one")
	appendAll(t, s, "#other", "two")

	if err := s.Drop("#lobby"); err != nil {
		t.Fatalf("Drop: %v", err)
	}
	if got, _ := s.Recent("#lobby", Query{}); len(got) != 0 {
		t.Error("dropped room still has history")
	}
	if got, _ := s.Recent("#other", Query{}); len(got) != 1 {
		t.Error("Drop removed an unrelated room")
	}
}

func TestRecentOnAnUnknownRoomIsEmptyNotAnError(t *testing.T) {
	s, _ := testStore(t, Options{})
	got, err := s.Recent("#never-used", Query{})
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d records for an unknown room", len(got))
	}
}

func TestConcurrentAppendsAreSerialized(t *testing.T) {
	s, _ := testStore(t, Options{})
	const writers, each = 8, 25

	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				if err := s.Append("#busy", msg("x")); err != nil {
					t.Errorf("Append: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	got, err := s.Recent("#busy", Query{})
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(got) != writers*each {
		t.Errorf("got %d records, want %d — concurrent appends interleaved", len(got), writers*each)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
