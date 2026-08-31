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

// --- names with spaces in them ------------------------------------------

// sayAs sends a MSG that also asserts a nick, which is the SECOND way a
// name is claimed (handleMsg adopts K_NICK).
func sayAs(t *testing.T, s *Session, id []byte, room, text, nick string) {
	t.Helper()
	env := clientEnvelope(rrc.TMsg, id, room, text)
	env.Nick = &nick
	s.OnInbound(encode(t, env))
}

// Reported by a user: a nick can contain spaces, and a name with a space
// in it is a name nobody can mention. mentionTokens splits the body on
// whitespace, so "@sam jones" names "sam" — someone else, or nobody
// — and a mention that resolves to nobody is silent at both ends.
func TestASpacedNickIsGrantedWithUnderscores(t *testing.T) {
	h := nickHub(t, nil)
	s, _, _ := connectKeyed(t, h, 0xA1, "sam jones")

	if got := nickOf(s); got != "sam_jones" {
		t.Errorf("granted nick = %q, want %q", got, "sam_jones")
	}
}

// The point of the rename: the name can now be mentioned.
func TestASpacedNickBecomesMentionable(t *testing.T) {
	h := nickHub(t, nil)
	idB := visitAndLeave(t, h, 0xB2, "sam jones")

	sa, _, idA := connectKeyed(t, h, 0xA1, "alice")
	join(t, sa, idA, "lobby", "")
	say(t, sa, idA, "lobby", "@sam_jones are you around?")

	if held := pendingFor(h, idB); len(held) != 1 {
		t.Fatalf("got %d pending mentions, want 1 — the granted name is not mentionable", len(held))
	}
}

// And the form that could never work still cannot: "@sam jones"
// names "sam". That is not a regression, it is the reason the name was
// changed — a mention cannot contain a space, so a name must not either.
func TestASpacedMentionStillNamesNobody(t *testing.T) {
	h := nickHub(t, nil)
	idB := visitAndLeave(t, h, 0xB2, "sam jones")

	sa, _, idA := connectKeyed(t, h, 0xA1, "alice")
	join(t, sa, idA, "lobby", "")
	say(t, sa, idA, "lobby", "@sam jones are you around?")

	if held := pendingFor(h, idB); len(held) != 0 {
		t.Errorf("got %d pending mentions, want 0", len(held))
	}
}

// A name that comes back different has to say why, or the peer is left
// with a name they did not choose and no account of it.
func TestTheSpacedPeerIsToldWhy(t *testing.T) {
	h := nickHub(t, nil)
	_, link, _ := connectKeyed(t, h, 0xA1, "sam jones")

	got := allNoticeText(t, link)
	if !strings.Contains(got, "cannot address the nick") || !strings.Contains(got, "sam_jones") {
		t.Errorf("the renamed peer was not told why:\n%s", got)
	}
	// The cost is the part worth naming: what their mention would have done.
	if !strings.Contains(got, "reached nobody") {
		t.Errorf("the notice does not say what it cost:\n%s", got)
	}
}

// The trap nicks.go already documents for taken names applies here too:
// a client re-asserts its nick on every MSG, so a notice keyed off the
// REQUEST repeats forever. It must key off the granted name.
func TestTheSpacedNickNoticeIsNotRepeated(t *testing.T) {
	h := nickHub(t, nil)
	s, link, id := connectKeyed(t, h, 0xA1, "sam jones")
	join(t, s, id, "lobby", "")

	for i := 0; i < 3; i++ {
		sayAs(t, s, id, "lobby", "still here", "sam jones")
	}

	if n := strings.Count(allNoticeText(t, link), "cannot address the nick"); n != 1 {
		t.Errorf("the notice was sent %d times, want once", n)
	}
}

// Every kind of whitespace, since strings.Fields splits on all of them:
// a tab or a non-breaking space breaks a mention exactly as a plain
// space does.
func TestEveryKindOfWhitespaceCollapses(t *testing.T) {
	cases := []struct{ in, want string }{
		{"sam jones", "sam_jones"},
		{"sam\tjones", "sam_jones"},
		{"sam\u00a0jones", "sam_jones"}, // non-breaking space
		{"sam   jones", "sam_jones"},
		{"  sam jones  ", "sam_jones"},
		{"sam van der jones", "sam_van_der_jones"},
		{"sam", "sam"},
		{"under_score", "under_score"},
	}
	for _, tc := range cases {
		got, ok := normalizeNick(tc.in, 32)
		if !ok || got != tc.want {
			t.Errorf("normalizeNick(%q) = %q, %v; want %q, true", tc.in, got, ok, tc.want)
		}
	}
}

// The other user of the old shared helper. An away reason is prose, and
// prose keeps its spaces — "back in 10 minutes" must not be echoed to
// the room as "back_in_10_minutes".
func TestAnAwayReasonKeepsItsSpaces(t *testing.T) {
	h := nickHub(t, nil)
	s, _, id := connectKeyed(t, h, 0xA1, "sam")
	join(t, s, id, "lobby", "")
	cmd(t, s, id, "lobby", "/away back in 10 minutes")

	s.mu.Lock()
	reason := s.awayReason
	s.mu.Unlock()
	if reason != "back in 10 minutes" {
		t.Errorf("away reason = %q, want %q", reason, "back in 10 minutes")
	}
}

// A space was the reported case, not the whole bug. mentionTokens also
// trims punctuation off the ends of a token, so "@sam!" names "sam" and
// a peer called "sam!" was unmentionable in exactly the same silence.
func TestPunctuationAtTheEndsOfANickIsTrimmed(t *testing.T) {
	h := nickHub(t, nil)
	s, _, _ := connectKeyed(t, h, 0xA1, "sam!")
	if got := nickOf(s); got != "sam" {
		t.Errorf("granted nick = %q, want %q", got, "sam")
	}

	b, _, _ := connectKeyed(t, h, 0xB2, "...bob...")
	if got := nickOf(b); got != "bob" {
		t.Errorf("granted nick = %q, want %q", got, "bob")
	}
}

// And an "@" inside a name is the worst of the three: mentionTokens
// reads the whole token as an email address and skips it, so there is
// no mention at all rather than a mention of the wrong person.
func TestAnAtSignInANickCannotSurvive(t *testing.T) {
	h := nickHub(t, nil)
	s, _, _ := connectKeyed(t, h, 0xA1, "bob@host")
	if got := nickOf(s); got != "bob_host" {
		t.Errorf("granted nick = %q, want %q", got, "bob_host")
	}
}

// Only the ENDS of a token are trimmed, so a name with punctuation in
// the middle is left exactly as its owner typed it. Rewriting those
// would take away names people actually have.
func TestInteriorPunctuationIsLeftAlone(t *testing.T) {
	for _, want := range []string{"O'Brien", "sam.jones", "under_score", "with-dash", "Ana-María"} {
		got, ok := normalizeNick(want, 32)
		if !ok || got != want {
			t.Errorf("normalizeNick(%q) = %q, %v; want it untouched", want, got, ok)
		}
	}
}

// The invariant, stated directly: whatever the hub grants, the mention
// tokenizer must hand back that same name. This is the test that makes
// the rule a rule rather than a list of characters somebody remembered
// — a change to mentionTokens that breaks a granted name fails here,
// not in a user's silent mention six months later.
func TestEveryGrantedNickRoundTripsThroughTheTokenizer(t *testing.T) {
	claims := []string{
		"sam", "sam jones", "sam!", "!sam!", "bob@host", "a@b@c",
		"O'Brien", "sam.jones", "under_score", "with-dash", "Ana-María",
		"sam\tjones", "sam jones", "  padded  ", "sam...",
		"decade", "🐢turtle", "多字节名字", "\"quoted\"", "(bracketed)",
	}
	for _, claim := range claims {
		granted, ok := normalizeNick(claim, 64)
		if !ok {
			continue // dropped outright, which is its own answer
		}
		toks := mentionTokens("@" + granted)
		if len(toks) != 1 || toks[0] != granted {
			t.Errorf("granted %q from %q, but mentionTokens(%q) = %v — that name cannot be mentioned",
				granted, claim, "@"+granted, toks)
		}
	}
}

// When nothing addressable is left, the name is dropped rather than
// granted — and the peer is TOLD, because a peer with no name and no
// explanation is the silent failure again, just at the other end.
func TestANameWithNothingAddressableLeftIsDropped(t *testing.T) {
	if got, ok := normalizeNick("...", 32); ok {
		t.Errorf("normalizeNick(%q) = %q, true; want it dropped", "...", got)
	}

	h := nickHub(t, nil)
	s, link, _ := connectKeyed(t, h, 0xA1, "...")
	if got := nickOf(s); got != "" {
		t.Errorf("session nick = %q, want empty", got)
	}
	if got := allNoticeText(t, link); !strings.Contains(got, "cannot be used here") {
		t.Errorf("the peer was not told its name was dropped:\n%s", got)
	}
}

// --- uniqueness, which the rewrite must not quietly break --------------

// Rewriting a name for mentionability creates a NEW way for two claims
// to converge: "sam jones" and "sam_jones" are different strings
// that now normalize to the same one. Uniqueness has to hold on the
// granted form, or the rewrite hands two people one name and mentions go
// back to resolving ambiguously — the exact failure unique_nicks exists
// to prevent.
func TestRewritingCannotHandTwoPeopleTheSameName(t *testing.T) {
	cases := []struct{ first, second, wantFirst, wantSecond string }{
		{"sam_jones", "sam jones", "sam_jones", "sam_jones1"},
		{"sam jones", "sam_jones", "sam_jones", "sam_jones1"},
		{"sam jones", "sam\tjones", "sam_jones", "sam_jones1"},
		{"sam", "sam!", "sam", "sam1"},
		{"sam!", "sam", "sam", "sam1"},
		{"bob_host", "bob@host", "bob_host", "bob_host1"},
	}
	for _, tc := range cases {
		h := nickHub(t, nil)
		a, _, _ := connectKeyed(t, h, 0xA1, tc.first)
		b, _, _ := connectKeyed(t, h, 0xB2, tc.second)

		if got := nickOf(a); got != tc.wantFirst {
			t.Errorf("%q then %q: first is %q, want %q", tc.first, tc.second, got, tc.wantFirst)
		}
		if got := nickOf(b); got != tc.wantSecond {
			t.Errorf("%q then %q: second is %q, want %q", tc.first, tc.second, got, tc.wantSecond)
		}
	}
}

// And the consequence that matters: the two names still reach two
// different people. Uniqueness is not the point in itself — resolving a
// mention to ONE person is.
func TestConvergedNamesStillReachTheRightPerson(t *testing.T) {
	h := nickHub(t, nil)
	idA := visitAndLeave(t, h, 0xA1, "sam_jones")
	idB := visitAndLeave(t, h, 0xB2, "sam jones") // becomes sam_jones1

	sc, _, idC := connectKeyed(t, h, 0xC3, "alice")
	join(t, sc, idC, "lobby", "")
	say(t, sc, idC, "lobby", "@sam_jones1 are you the second one?")

	if held := pendingFor(h, idB); len(held) != 1 {
		t.Errorf("the second sam got %d mentions, want 1", len(held))
	}
	if held := pendingFor(h, idA); len(held) != 0 {
		t.Errorf("the first sam got %d mentions, want 0 — the names are not distinct", len(held))
	}
}

// Ownership is by identity and survives disconnection, so a rewritten
// name must come back to the same person rather than being handed to
// whoever claims it next.
func TestARewrittenNameIsStillOwnedByItsIdentity(t *testing.T) {
	h := nickHub(t, nil)
	s, _, _ := connectKeyed(t, h, 0xA1, "sam jones")
	if got := nickOf(s); got != "sam_jones" {
		t.Fatalf("granted %q, want %q", got, "sam_jones")
	}
	s.Close()

	again, _, _ := connectKeyed(t, h, 0xA1, "sam jones")
	if got := nickOf(again); got != "sam_jones" {
		t.Errorf("on reconnect the owner got %q, want its own %q back", got, "sam_jones")
	}
}

// --- names that look alike ---------------------------------------------

// The question this answers: how do you pick the right sam out of a
// list when both rows read "sam"? You cannot, so the hub does not
// create that list. A name that renders like one already in use is
// granted with a suffix, which is visible.
func TestAnInvisibleCharacterDoesNotMakeANewName(t *testing.T) {
	for _, hidden := range []string{
		"sam\u200b", // zero-width space
		"sa\u200cm", // zero-width non-joiner
		"sa\u200dm", // zero-width joiner
		"sam\u2060", // word joiner
		"\ufeffsam", // byte-order mark
		"sam\u00ad", // soft hyphen
		"\u202esam", // right-to-left override
	} {
		h := nickHub(t, nil)
		connectKeyed(t, h, 0xA1, "sam")
		b, _, _ := connectKeyed(t, h, 0xB2, hidden)

		if got := nickOf(b); got != "sam1" {
			t.Errorf("%+q was granted %q, want %q — two rows would read \"sam\"", hidden, got, "sam1")
		}
	}
}

// The invisible character is removed from the granted name, not merely
// ignored when comparing. Keeping it would leave the hub holding a name
// that prints as "sam" but is not the string "sam" — which every other
// part of the hub (mentions, /who, the directory) then disagrees about.
func TestInvisibleCharactersAreStrippedFromTheName(t *testing.T) {
	h := nickHub(t, nil)
	s, _, _ := connectKeyed(t, h, 0xA1, "sa\u200bm")
	if got := nickOf(s); got != "sam" {
		t.Errorf("granted %+q, want %q", got, "sam")
	}
}

// A Cyrillic \u0430 is not a Latin a, and on screen it is. Same for the
// Greek and fullwidth forms.
func TestAHomoglyphDoesNotMakeANewName(t *testing.T) {
	for _, lookalike := range []string{
		"s\u0430m",           // Cyrillic a
		"\u0455am",           // Cyrillic s
		"\u0455\u0430\u043c", // all Cyrillic: s, a, m
		"s\u03b1m",           // Greek alpha
		"\uff53\uff41\uff4d", // fullwidth sam
		"SAM",                // the case-folding that already worked
	} {
		h := nickHub(t, nil)
		connectKeyed(t, h, 0xA1, "sam")
		b, _, _ := connectKeyed(t, h, 0xB2, lookalike)

		if got := nickOf(b); got == "sam" || !strings.HasSuffix(got, "1") {
			t.Errorf("%+q was granted %q, want a suffixed name — it renders as \"sam\"", lookalike, got)
		}
	}
}

// The display name keeps the characters its owner typed. Folding for
// COMPARISON costs a Cyrillic speaker nothing; folding for DISPLAY would
// rewrite their name into somebody else's alphabet.
func TestANonLatinNameIsNotRewritten(t *testing.T) {
	for _, name := range []string{"\u0441\u0430\u0448\u0430", "\u591a\u5b57\u8282", "\u03b1\u03bb\u03ad\u03be\u03b7\u03c2", "Ana-Mar\u00eda"} {
		h := nickHub(t, nil)
		s, _, _ := connectKeyed(t, h, 0xA1, name)
		if got := nickOf(s); got != name {
			t.Errorf("granted %q, want the name as typed, %q", got, name)
		}
	}
}

// Folding must not collapse names that are genuinely different, or it
// hands out suffixes nobody needed.
func TestTheFoldDoesNotOverReach(t *testing.T) {
	h := nickHub(t, nil)
	a, _, _ := connectKeyed(t, h, 0xA1, "sam")
	b, _, _ := connectKeyed(t, h, 0xB2, "sami")
	c, _, _ := connectKeyed(t, h, 0xC3, "sa_m")

	if nickOf(a) != "sam" || nickOf(b) != "sami" || nickOf(c) != "sa_m" {
		t.Errorf("distinct names were folded together: %q %q %q", nickOf(a), nickOf(b), nickOf(c))
	}
}

// Told, and told the RIGHT thing: "already taken" would send somebody
// looking for a conflict they cannot see.
func TestALookalikeIsToldWhatHappened(t *testing.T) {
	h := nickHub(t, nil)
	connectKeyed(t, h, 0xA1, "sam")
	_, link, _ := connectKeyed(t, h, 0xB2, "s\u0430m")

	got := allNoticeText(t, link)
	if !strings.Contains(got, "renders the same") {
		t.Errorf("the peer was not told its name was a lookalike:\n%s", got)
	}
	// It must not name the other person: that confirms a target exists
	// to somebody who went looking for one.
	if strings.Contains(got, "already taken") {
		t.Errorf("a lookalike was reported as an ordinary name clash:\n%s", got)
	}
}

// And the reason all of this matters: the mention still reaches exactly
// one of them, and the sender can see which.
func TestAMentionStillReachesOneOfTwoLookalikes(t *testing.T) {
	h := nickHub(t, nil)
	idA := visitAndLeave(t, h, 0xA1, "sam")
	idB := visitAndLeave(t, h, 0xB2, "s\u0430m") // renders as sam, granted sam1

	sc, _, idC := connectKeyed(t, h, 0xC3, "alice")
	join(t, sc, idC, "lobby", "")
	say(t, sc, idC, "lobby", "@sam are you the first one?")

	if held := pendingFor(h, idA); len(held) != 1 {
		t.Errorf("the first sam got %d mentions, want 1", len(held))
	}
	if held := pendingFor(h, idB); len(held) != 0 {
		t.Errorf("the lookalike got %d mentions, want 0", len(held))
	}
}

// The key is the whole mechanism, so its own behaviour is pinned.
func TestNickKeyFoldsWhatItClaimsTo(t *testing.T) {
	same := [][2]string{
		{"sam", "SAM"},
		{"sam", "sam\u200b"},
		{"sam", "s\u0430m"},
		{"sam", "\u0455\u0430\u043c"},
		{"sam", "s\u03b1m"},
		{"sam", "\uff53\uff41\uff4d"},
	}
	for _, p := range same {
		if nickKey(p[0]) != nickKey(p[1]) {
			t.Errorf("nickKey(%+q)=%q and nickKey(%+q)=%q differ; they render alike",
				p[0], nickKey(p[0]), p[1], nickKey(p[1]))
		}
	}
	differ := [][2]string{{"sam", "sami"}, {"sam", "sa_m"}, {"sam", "sam1"}, {"kim", "tim"}}
	for _, p := range differ {
		if nickKey(p[0]) == nickKey(p[1]) {
			t.Errorf("nickKey folded %q and %q together", p[0], p[1])
		}
	}
}
