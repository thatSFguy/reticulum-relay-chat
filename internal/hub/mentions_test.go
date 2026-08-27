package hub

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thatSFguy/reticulum-relay-chat/internal/config"
	"github.com/thatSFguy/reticulum-relay-chat/internal/peerreg"
	"github.com/thatSFguy/reticulum-relay-chat/internal/rrc"
)

// keyFor builds a public key and the identity hash it derives, so tests
// can register peers the way a real LINKIDENTIFY would.
func keyFor(seed byte) (pubKey, idHash []byte) {
	pubKey = bytes.Repeat([]byte{seed}, 64)
	h := sha256.Sum256(pubKey)
	return pubKey, append([]byte(nil), h[:16]...)
}

func mentionHub(t *testing.T, tune func(*config.HubConfig)) *Hub {
	t.Helper()
	dir := t.TempDir()
	cfg := config.HubConfig{
		MentionNotify:       true,
		PeerRegistryPath:    filepath.Join(dir, "peers.toml"),
		MaxKnownPeers:       128,
		MaxPendingMentions:  20,
		MentionSnippetBytes: 140,
		RoomRegistryPath:    filepath.Join(dir, "rooms.toml"),
	}
	if tune != nil {
		tune(&cfg)
	}
	return quietHubCfg(cfg)
}

// connectKeyed brings up a session whose link carries a real key/identity
// pair, so the hub can file it in the peer directory.
func connectKeyed(t *testing.T, h *Hub, seed byte, nick string) (*Session, *fakeLink, []byte) {
	t.Helper()
	pub, id := keyFor(seed)
	link := &fakeLink{id: id, pubKey: pub}
	s := h.NewSession(link)
	hello := clientEnvelope(rrc.THello, id, "", nil)
	if nick != "" {
		hello.Nick = &nick
	}
	s.OnInbound(encode(t, hello))
	return s, link, id
}

// visitAndLeave connects a peer long enough for the hub to file it,
// then disconnects — which is what "the hub knows them but they are not
// here" means.
func visitAndLeave(t *testing.T, h *Hub, seed byte, nick string) []byte {
	t.Helper()
	s, _, id := connectKeyed(t, h, seed, nick)
	s.Close()
	return id
}

func pendingFor(h *Hub, id []byte) []peerreg.Mention {
	h.mu.Lock()
	defer h.mu.Unlock()
	p, ok := h.peers[hex.EncodeToString(id)]
	if !ok {
		return nil
	}
	return append([]peerreg.Mention(nil), p.Mentions...)
}

func TestMentionTokens(t *testing.T) {
	cases := []struct {
		body string
		want []string
	}{
		{"@alice hello", []string{"alice"}},
		{"hey @alice, you there?", []string{"alice"}},
		{"@alice and @bob", []string{"alice", "bob"}},
		{"@a1b2c3d4 look", []string{"a1b2c3d4"}},
		{"no mention here", nil},
		{"@", nil},
		{"mail me at bob@example.com", nil},
		{"@under_score and @with-dash", []string{"under_score", "with-dash"}},
		{"punctuation @alice!", []string{"alice"}},
	}
	for _, tc := range cases {
		got := mentionTokens(tc.body)
		if !equalStrings(got, tc.want) {
			t.Errorf("mentionTokens(%q) = %v, want %v", tc.body, got, tc.want)
		}
	}
}

// The case the feature exists for: someone is named while they are not
// connected, and hears about it when they come back.
func TestAMentionWaitsForAnAbsentPeer(t *testing.T) {
	h := mentionHub(t, nil)

	// Bob visits once so the hub knows him, then leaves.
	idB := visitAndLeave(t, h, 0xB2, "bob")

	sa, _, idA := connectKeyed(t, h, 0xA1, "alice")
	join(t, sa, idA, "#lobby", "")
	say(t, sa, idA, "#lobby", "@bob are you around?")

	held := pendingFor(h, idB)
	if len(held) != 1 {
		t.Fatalf("got %d pending mentions, want 1", len(held))
	}
	if held[0].Room != "#lobby" || held[0].ByNick != "alice" {
		t.Errorf("pending mention = %+v, want one in #lobby from alice", held[0])
	}
	if !strings.Contains(held[0].Text, "are you around") {
		t.Errorf("mention text %q does not quote the message", held[0].Text)
	}

	// Bob returns.
	sb, linkB, _ := connectKeyed(t, h, 0xB2, "bob")
	_ = sb
	notices := noticesOn(t, linkB)
	if !hasPrefixIn(notices, "--- 1 mention(s) while you were away ---") {
		t.Errorf("no mention header among %v", notices)
	}
	var found bool
	for _, n := range notices {
		if strings.Contains(n, "#lobby") && strings.Contains(n, "alice") {
			found = true
		}
	}
	if !found {
		t.Errorf("the mention was not delivered on return: %v", notices)
	}
	if len(pendingFor(h, idB)) != 0 {
		t.Error("delivered mentions were not cleared")
	}
}

// Someone sitting in the room already received the message. A second
// "you were mentioned" for a line on their screen is noise.
func TestNoNotificationForSomeoneAlreadyInTheRoom(t *testing.T) {
	h := mentionHub(t, nil)
	sa, _, idA := connectKeyed(t, h, 0xA1, "alice")
	sb, linkB, idB := connectKeyed(t, h, 0xB2, "bob")
	join(t, sa, idA, "#lobby", "")
	join(t, sb, idB, "#lobby", "")

	before := len(noticesOn(t, linkB))
	say(t, sa, idA, "#lobby", "@bob hello")

	if got := pendingFor(h, idB); len(got) != 0 {
		t.Errorf("queued %d mentions for a present member", len(got))
	}
	for _, n := range noticesOn(t, linkB)[before:] {
		if strings.Contains(n, "mentioned") {
			t.Errorf("a present member was told they were mentioned: %q", n)
		}
	}
}

// Connected but elsewhere: they did not receive the message, so tell
// them now rather than holding it.
func TestAConnectedPeerOutsideTheRoomIsToldImmediately(t *testing.T) {
	h := mentionHub(t, nil)
	sa, _, idA := connectKeyed(t, h, 0xA1, "alice")
	_, linkB, idB := connectKeyed(t, h, 0xB2, "bob")
	join(t, sa, idA, "#lobby", "")

	say(t, sa, idA, "#lobby", "@bob come join us")

	if got := pendingFor(h, idB); len(got) != 0 {
		t.Errorf("queued %d mentions for a connected peer", len(got))
	}
	if !hasSubstringIn(noticesOn(t, linkB), "you were mentioned in #lobby") {
		t.Errorf("connected peer was not notified: %v", noticesOn(t, linkB))
	}
}

// A nickname that matches two people names neither: guessing would send
// one person's conversation to a stranger.
func TestAnAmbiguousNicknameNotifiesNobody(t *testing.T) {
	h := mentionHub(t, nil)
	idB := visitAndLeave(t, h, 0xB2, "sam")
	idC := visitAndLeave(t, h, 0xC3, "sam")

	sa, _, idA := connectKeyed(t, h, 0xA1, "alice")
	join(t, sa, idA, "#lobby", "")
	say(t, sa, idA, "#lobby", "@sam which one of you")

	if len(pendingFor(h, idB))+len(pendingFor(h, idC)) != 0 {
		t.Error("an ambiguous nickname produced a notification")
	}
}

// A hash prefix is exact, so it resolves where a nickname cannot.
func TestAHashPrefixResolvesUnambiguously(t *testing.T) {
	h := mentionHub(t, nil)
	idB := visitAndLeave(t, h, 0xB2, "sam")
	idC := visitAndLeave(t, h, 0xC3, "sam")

	sa, _, idA := connectKeyed(t, h, 0xA1, "alice")
	join(t, sa, idA, "#lobby", "")
	say(t, sa, idA, "#lobby", "@"+hex.EncodeToString(idB)[:8]+" this one")

	if len(pendingFor(h, idB)) != 1 {
		t.Errorf("hash-prefix mention did not reach its target")
	}
	if len(pendingFor(h, idC)) != 0 {
		t.Error("hash-prefix mention reached the wrong peer")
	}
}

// Naming yourself is not a notification.
func TestSelfMentionsAreIgnored(t *testing.T) {
	h := mentionHub(t, nil)
	sa, _, idA := connectKeyed(t, h, 0xA1, "alice")
	join(t, sa, idA, "#lobby", "")

	// Leave the room so a mention would otherwise queue.
	sa.OnInbound(encode(t, clientEnvelope(rrc.TPart, idA, "#lobby", nil)))
	join(t, sa, idA, "#lobby", "")
	say(t, sa, idA, "#lobby", "@alice talking to myself")

	if got := pendingFor(h, idA); len(got) != 0 {
		t.Errorf("self-mention queued %d notifications", len(got))
	}
}

// One message must not become a notification storm.
func TestOneMessageCannotNotifyEveryone(t *testing.T) {
	h := mentionHub(t, nil)
	var ids [][]byte
	names := []string{"n1", "n2", "n3", "n4", "n5", "n6", "n7", "n8"}
	for i, n := range names {
		ids = append(ids, visitAndLeave(t, h, byte(0x10+i), n))
	}

	sa, _, idA := connectKeyed(t, h, 0xA1, "alice")
	join(t, sa, idA, "#lobby", "")
	say(t, sa, idA, "#lobby", "@n1 @n2 @n3 @n4 @n5 @n6 @n7 @n8 everyone")

	total := 0
	for _, id := range ids {
		total += len(pendingFor(h, id))
	}
	if total > maxMentionsPerMessage {
		t.Errorf("one message produced %d notifications, cap is %d", total, maxMentionsPerMessage)
	}
	if total == 0 {
		t.Error("no notifications at all — the cap swallowed everything")
	}
}

// A burst while someone is away must not grow without bound, and what
// survives should be the newest.
func TestThePendingQueueIsBoundedAndKeepsTheNewest(t *testing.T) {
	h := mentionHub(t, func(c *config.HubConfig) { c.MaxPendingMentions = 3 })
	idB := visitAndLeave(t, h, 0xB2, "bob")

	sa, _, idA := connectKeyed(t, h, 0xA1, "alice")
	join(t, sa, idA, "#lobby", "")
	for _, text := range []string{"one", "two", "three", "four", "five"} {
		say(t, sa, idA, "#lobby", "@bob "+text)
	}

	held := pendingFor(h, idB)
	if len(held) != 3 {
		t.Fatalf("got %d pending mentions, want the cap of 3", len(held))
	}
	if !strings.Contains(held[len(held)-1].Text, "five") {
		t.Errorf("newest pending mention is %q, want the last one sent", held[len(held)-1].Text)
	}
	if strings.Contains(held[0].Text, "one") {
		t.Error("oldest mention survived eviction")
	}
}

func TestNotifyOffStopsFurtherMentions(t *testing.T) {
	h := mentionHub(t, nil)
	sb, linkB, idB := connectKeyed(t, h, 0xB2, "bob")
	sb.OnInbound(encode(t, clientEnvelope(rrc.TMsg, idB, "", "/notify off")))
	if !hasSubstringIn(noticesOn(t, linkB), "mention notifications off") {
		t.Fatalf("no confirmation for /notify off: %v", noticesOn(t, linkB))
	}
	sb.Close()

	sa, _, idA := connectKeyed(t, h, 0xA1, "alice")
	join(t, sa, idA, "#lobby", "")
	say(t, sa, idA, "#lobby", "@bob still there?")

	if got := pendingFor(h, idB); len(got) != 0 {
		t.Errorf("a mention queued for a peer who opted out: %+v", got)
	}

	// And opting back in resumes them.
	sb2, _, _ := connectKeyed(t, h, 0xB2, "bob")
	sb2.OnInbound(encode(t, clientEnvelope(rrc.TMsg, idB, "", "/notify on")))
	sb2.Close()
	say(t, sa, idA, "#lobby", "@bob back on?")
	if got := pendingFor(h, idB); len(got) != 1 {
		t.Errorf("opting back in did not resume notifications (%d pending)", len(got))
	}
}

// Opting out drops what is already waiting: delivering it on the next
// connection would hand over exactly what was just declined.
func TestNotifyOffDiscardsWhatIsAlreadyWaiting(t *testing.T) {
	h := mentionHub(t, nil)
	idB := visitAndLeave(t, h, 0xB2, "bob")

	sa, _, idA := connectKeyed(t, h, 0xA1, "alice")
	join(t, sa, idA, "#lobby", "")
	say(t, sa, idA, "#lobby", "@bob one")
	say(t, sa, idA, "#lobby", "@bob two")
	if len(pendingFor(h, idB)) != 2 {
		t.Fatal("setup: mentions did not queue")
	}

	// Bob reconnects — which delivers and clears them — so re-seed the
	// queue to model someone who opts out with mail still waiting.
	sb, _, _ := connectKeyed(t, h, 0xB2, "bob")
	h.mu.Lock()
	h.peers[hex.EncodeToString(idB)].Mentions = []peerreg.Mention{{Room: "#lobby", Text: "still here"}}
	h.mu.Unlock()

	sb.OnInbound(encode(t, clientEnvelope(rrc.TMsg, idB, "", "/notify off")))

	if got := pendingFor(h, idB); len(got) != 0 {
		t.Errorf("opting out left %d mentions waiting", len(got))
	}
}

// The directory is what makes someone addressable after they leave, so
// it has to survive a restart.
func TestThePeerDirectorySurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	cfg := config.HubConfig{
		MentionNotify:       true,
		PeerRegistryPath:    filepath.Join(dir, "peers.toml"),
		MaxKnownPeers:       128,
		MaxPendingMentions:  20,
		MentionSnippetBytes: 140,
		RoomRegistryPath:    filepath.Join(dir, "rooms.toml"),
	}

	h1 := quietHubCfg(cfg)
	idB := visitAndLeave(t, h1, 0xB2, "bob")
	sa, _, idA := connectKeyed(t, h1, 0xA1, "alice")
	join(t, sa, idA, "#lobby", "")
	say(t, sa, idA, "#lobby", "@bob catch you later")
	h1.Stop()

	h2 := quietHubCfg(cfg)
	held := pendingFor(h2, idB)
	if len(held) != 1 {
		t.Fatalf("after restart the hub holds %d mentions, want 1", len(held))
	}
	if held[0].Room != "#lobby" {
		t.Errorf("restored mention room = %q, want #lobby", held[0].Room)
	}

	// And the restored key must still address the same identity.
	h2.mu.Lock()
	p := h2.peers[hex.EncodeToString(idB)]
	h2.mu.Unlock()
	if p == nil || !peerreg.KeyMatchesIdentity(p.PublicKey, hex.EncodeToString(idB)) {
		t.Error("restored peer key does not derive its identity")
	}
}

// A row whose key does not derive its identity is an address pointed at
// someone else; it must not survive a load.
func TestALoadDropsAPeerWhoseKeyDoesNotMatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "peers.toml")

	pubB, idB := keyFor(0xB2)
	_, idC := keyFor(0xC3)
	// File bob's key under carol's identity.
	forged := map[string]*peerreg.Peer{
		hex.EncodeToString(idC): {
			IdentityHex: hex.EncodeToString(idC),
			PublicKey:   pubB,
			Nick:        "carol",
		},
	}
	if err := peerreg.Save(path, forged); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := peerreg.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := loaded[hex.EncodeToString(idC)]; ok {
		t.Error("a peer whose key does not derive its identity was loaded")
	}
	if _, ok := loaded[hex.EncodeToString(idB)]; ok {
		t.Error("the forged row was loaded under the key owner's identity")
	}
}

// With notifications off, nothing is resolved, queued or stored.
func TestMentionsOffChangesNothing(t *testing.T) {
	h := quietHub()
	sa, _, idA := connectKeyed(t, h, 0xA1, "alice")
	_, _, idB := connectKeyed(t, h, 0xB2, "bob")
	join(t, sa, idA, "#lobby", "")
	say(t, sa, idA, "#lobby", "@bob hello")

	if got := pendingFor(h, idB); len(got) != 0 {
		t.Errorf("a hub with mentions disabled queued %d notifications", len(got))
	}
	h.mu.Lock()
	n := len(h.peers)
	h.mu.Unlock()
	if n != 0 {
		t.Errorf("a hub with mentions disabled filed %d peers", n)
	}
}

func TestSnippetTruncatesOnARuneBoundary(t *testing.T) {
	body := strings.Repeat("é", 100) // two bytes per rune
	got := snippet(body, 11)
	if !strings.HasSuffix(got, "…") {
		t.Errorf("snippet %q is not marked as truncated", got)
	}
	trimmed := strings.TrimSuffix(got, "…")
	for _, r := range trimmed {
		if r != 'é' {
			t.Fatalf("snippet split a rune: %q", got)
		}
	}
}

func hasSubstringIn(list []string, want string) bool {
	for _, s := range list {
		if strings.Contains(s, want) {
			return true
		}
	}
	return false
}

// Plenty of ordinary nicknames are also valid hex. Treating a token as
// a hash prefix and giving up when it matches no identity would make
// those people silently unmentionable — no error, just nothing.
func TestAHexShapedNickIsStillMentionable(t *testing.T) {
	h := mentionHub(t, nil)
	for _, nick := range []string{"decade", "facade", "beaded"} {
		t.Run(nick, func(t *testing.T) {
			seed := byte(0xC0 + len(nick) + int(nick[0]))
			_, id := keyFor(seed)
			visitAndLeave(t, h, seed, nick)

			sa, _, idA := connectKeyed(t, h, 0xA1, "alice")
			join(t, sa, idA, "#lobby", "")
			say(t, sa, idA, "#lobby", "@"+nick+" are you there")
			sa.Close()

			if got := len(pendingFor(h, id)); got != 1 {
				t.Fatalf("%d mentions queued for @%s, want 1", got, nick)
			}
		})
	}
}
