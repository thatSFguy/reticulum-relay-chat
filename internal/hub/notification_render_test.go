package hub

import (
	"strings"
	"testing"

	"github.com/thatSFguy/reticulum-relay-chat/internal/config"
	"github.com/thatSFguy/reticulum-relay-chat/internal/peerreg"
)

// --- notification content ----------------------------------------------

// The notification does not arrive next to the conversation it is
// about — it arrives in a general messaging app, possibly hours later.
// Which room it came from is the fact that makes it actionable.
func TestNotificationNamesTheRoom(t *testing.T) {
	h := quietHubCfg(config.HubConfig{Name: "Test Hub"})
	h.SetDestHash(mustHexBytes(t, testDest))

	h.mu.Lock()
	title, body := h.renderMentionNotificationLocked([]peerreg.Mention{
		{Room: "ops", ByNick: "alice", Text: "@bob the relay is down", TS: h.nowUnix()},
	})
	h.mu.Unlock()

	if !strings.Contains(title, "#ops") {
		t.Errorf("title does not name the room: %q", title)
	}
	if !strings.Contains(body, "#ops") {
		t.Errorf("body does not name the room: %q", body)
	}
	if !strings.Contains(body, "alice") || !strings.Contains(body, "the relay is down") {
		t.Errorf("body lost the author or the text: %q", body)
	}
	// The way back is a link in the syntax NomadNet already uses
	// (SPEC §11.6.3): a room name alone is not directions, because room
	// names are not unique across hubs.
	if !strings.Contains(body, "rrc@"+testDest+":/room/ops") {
		t.Errorf("body does not carry a link back to the room: %q", body)
	}
	if !strings.Contains(body, "/notify off") {
		t.Errorf("body does not say how to stop these: %q", body)
	}
}

func TestNotificationNamesEveryRoomWhenMentionsSpanSeveral(t *testing.T) {
	h := quietHubCfg(config.HubConfig{Name: "Test Hub"})
	h.mu.Lock()
	title, body := h.renderMentionNotificationLocked([]peerreg.Mention{
		{Room: "ops", ByNick: "alice", Text: "@bob one", TS: h.nowUnix()},
		{Room: "lobby", ByNick: "carol", Text: "@bob two", TS: h.nowUnix()},
	})
	h.mu.Unlock()

	// A bare count is "something happened"; the rooms are "the thing
	// you care about happened in #ops".
	if !strings.Contains(title, "#ops") || !strings.Contains(title, "#lobby") {
		t.Errorf("title does not name both rooms: %q", title)
	}
	if !strings.Contains(body, "#ops") || !strings.Contains(body, "#lobby") {
		t.Errorf("body does not name both rooms: %q", body)
	}
}

func TestNotificationCollapsesOneRoomInTheTitle(t *testing.T) {
	h := quietHubCfg(config.HubConfig{Name: "Test Hub"})
	h.mu.Lock()
	title, _ := h.renderMentionNotificationLocked([]peerreg.Mention{
		{Room: "ops", ByNick: "alice", Text: "one", TS: h.nowUnix()},
		{Room: "ops", ByNick: "carol", Text: "two", TS: h.nowUnix()},
	})
	h.mu.Unlock()
	if !strings.Contains(title, "2 mentions in #ops") {
		t.Errorf("title = %q, want it to collapse to one room", title)
	}
}
