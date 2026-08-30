package hub

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/thatSFguy/reticulum-relay-chat/internal/config"
)

// allNoticeText joins every NOTICE a link received. The listing
// commands pack their output into as few envelopes as it fits, so an
// assertion must look at the whole answer rather than the last frame.
func allNoticeText(t *testing.T, link *fakeLink) string {
	t.Helper()
	return strings.Join(noticesOn(t, link), "\n")
}

// --- the table is the documentation ----------------------------------

// TestEveryDispatchableCommandIsDocumented is the guard that makes the
// table worth having: a command reachable by typing it must appear in
// /help, with a usage line that starts with its own name.
func TestEveryDispatchableCommandIsDocumented(t *testing.T) {
	for name, spec := range commandIndex {
		if spec == nil {
			t.Fatalf("command %q indexes to nil", name)
		}
		if spec.usage == "" || spec.summary == "" {
			t.Errorf("command /%s has no usage or summary", name)
		}
		if spec.run == nil {
			t.Errorf("command /%s has no handler", name)
		}
		if !strings.HasPrefix(spec.usage, "/"+spec.name) {
			t.Errorf("/%s: usage %q does not start with the command name", spec.name, spec.usage)
		}
	}
	for i := range commands {
		if commandIndex[commands[i].name] != &commands[i] {
			t.Errorf("/%s is in the table but not indexed", commands[i].name)
		}
	}
}

// TestGreetingPointsAtACommandThatExists is the bug this work started
// from: the shipped default greeting tells every arriving client
// "/help for commands", and until now the hub answered "unrecognized
// command".
func TestGreetingPointsAtACommandThatExists(t *testing.T) {
	greeting := config.DefaultsForTest().Greeting
	refs := commandsNamedIn(greeting)
	if len(refs) == 0 {
		t.Fatal("the default greeting names no commands — this test would prove nothing")
	}
	for _, ref := range refs {
		if commandIndex[ref] == nil {
			t.Errorf("the default greeting points at /%s, which the hub does not implement", ref)
		}
	}
}

// commandsNamedIn extracts slash-command references from prose.
func commandsNamedIn(text string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(text, func(r rune) bool {
		return r == ' ' || r == '\n' || r == '\t'
	}) {
		if !strings.HasPrefix(f, "/") || len(f) < 2 {
			continue
		}
		name := strings.ToLower(strings.TrimFunc(f[1:], func(r rune) bool {
			return !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'))
		}))
		if name != "" {
			out = append(out, name)
		}
	}
	return out
}

// --- /help ------------------------------------------------------------

func TestHelpListsCommandsAndHidesOperatorOnes(t *testing.T) {
	h := quietHub()
	id := bytes.Repeat([]byte{0xA1}, 16)
	s, link := connect(t, h, id)
	cmd(t, s, id, "", "/help")

	got := allNoticeText(t, link)
	for _, want := range []string{"/help", "/version", "/notify", "/whoami", "/mentions", "/seen"} {
		if !strings.Contains(got, want) {
			t.Errorf("/help omitted %s:\n%s", want, got)
		}
	}
	// A command that would only ever answer "not authorized" teaches
	// nothing, so it is not offered.
	for _, hidden := range []string{"/kline", "/reload", "/stats"} {
		if strings.Contains(got, hidden) {
			t.Errorf("/help offered %s to a non-operator:\n%s", hidden, got)
		}
	}
}

func TestHelpShowsOperatorCommandsToAnOperator(t *testing.T) {
	id := bytes.Repeat([]byte{0xA1}, 16)
	h := quietHub()
	h.cfg.TrustedIdentities = []string{hex.EncodeToString(id)}
	h.reloadTrust()
	s, link := connect(t, h, id)
	cmd(t, s, id, "", "/help")

	got := allNoticeText(t, link)
	for _, want := range []string{"/kline", "/reload", "/stats"} {
		if !strings.Contains(got, want) {
			t.Errorf("/help hid %s from a server operator:\n%s", want, got)
		}
	}
}

func TestHelpForOneCommandGivesItsUsage(t *testing.T) {
	h := quietHub()
	id := bytes.Repeat([]byte{0xA1}, 16)
	s, link := connect(t, h, id)
	cmd(t, s, id, "", "/help notify")

	got := allNoticeText(t, link)
	if !strings.Contains(got, "/notify [status|on|off|address|test [full]]") {
		t.Errorf("/help notify did not give its usage:\n%s", got)
	}
}

// A non-operator asking about an operator command is told it does not
// exist, rather than that it exists and is out of reach.
func TestHelpForAnOperatorCommandIsWithheld(t *testing.T) {
	h := quietHub()
	id := bytes.Repeat([]byte{0xA1}, 16)
	s, link := connect(t, h, id)
	cmd(t, s, id, "", "/help kline")

	if got := allNoticeText(t, link); !strings.Contains(got, "no such command") {
		t.Errorf("/help kline leaked an operator command to a non-operator:\n%s", got)
	}
}

// --- dispatch gating --------------------------------------------------

func TestOperatorCommandsAreRefusedFromTheTable(t *testing.T) {
	h := quietHub()
	id := bytes.Repeat([]byte{0xA1}, 16)
	s, link := connect(t, h, id)
	for _, c := range []string{"/stats", "/reload", "/kline list"} {
		cmd(t, s, id, "", c)
		if got := lastError(t, link); got != "not authorized" {
			t.Errorf("%s from a non-operator: got %q, want %q", c, got, "not authorized")
		}
	}
}

// --- /version ---------------------------------------------------------

// /stats is operator-only, so without /version an ordinary user cannot
// find out whether the feature they are complaining about is even
// switched on.
func TestVersionReportsFeaturesToAnOrdinaryUser(t *testing.T) {
	h := quietHubCfg(config.HubConfig{MentionNotify: true})
	id := bytes.Repeat([]byte{0xA1}, 16)
	s, link := connect(t, h, id)
	cmd(t, s, id, "", "/version")

	got := allNoticeText(t, link)
	if !strings.Contains(got, "Test Hub") || !strings.Contains(got, "test") {
		t.Errorf("/version did not name the hub and its version:\n%s", got)
	}
	if !strings.Contains(got, "mention notification: on") {
		t.Errorf("/version did not report mention notification:\n%s", got)
	}
	if !strings.Contains(got, "history: off") {
		t.Errorf("/version did not report history:\n%s", got)
	}
}

func TestVersionSaysWhenNotificationsAreOff(t *testing.T) {
	h := quietHub()
	id := bytes.Repeat([]byte{0xA1}, 16)
	s, link := connect(t, h, id)
	cmd(t, s, id, "", "/version")

	if got := allNoticeText(t, link); !strings.Contains(got, "mention notification: off") {
		t.Errorf("/version claimed notifications on a hub without them:\n%s", got)
	}
}

// --- sendNoticeLines --------------------------------------------------

// A /help silently dropped for exceeding max_msg_body_bytes is worse
// than no /help, so the listing helper must respect a tightened limit.
func TestNoticeLinesRespectATightenedBodyLimit(t *testing.T) {
	h := quietHubCfg(config.HubConfig{Limits: config.LimitsConfig{
		MaxNickBytes: 32, MaxRoomNameBytes: 64, MaxMsgBodyBytes: 64,
		MaxRoomsPerSession: 16, RateLimitMsgsPerMin: 240,
	}})
	id := bytes.Repeat([]byte{0xA1}, 16)
	s, link := connect(t, h, id)
	cmd(t, s, id, "", "/help")

	notices := noticesOn(t, link)
	if len(notices) < 2 {
		t.Fatalf("expected /help to be split across several NOTICEs, got %d", len(notices))
	}
	for _, n := range notices {
		if len(n) > 64 {
			t.Errorf("NOTICE of %d bytes exceeds max_msg_body_bytes=64: %q", len(n), n)
		}
	}
}
