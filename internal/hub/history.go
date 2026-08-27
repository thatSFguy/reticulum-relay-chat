package hub

import (
	"fmt"
	"time"

	"github.com/thatSFguy/reticulum-relay-chat/internal/history"
	"github.com/thatSFguy/reticulum-relay-chat/internal/rrc"
)

// This file is the hub's side of the retained transcript: what gets
// recorded, what a joining client is replayed, and how a client asks for
// more. See internal/history for the store itself.
//
// Nothing here changes the RRC wire format. A replayed message is the
// original envelope rebuilt — same K_ID, same K_TS, same K_SRC — and the
// brackets around a replay are ordinary NOTICEs. That is a hard
// requirement rather than a preference: the deployed clients cannot be
// modified, so a feature they must be taught to understand is a feature
// nobody can use.

// historyEnabled reports whether the hub is retaining transcripts.
func (h *Hub) historyEnabled() bool { return h.history != nil }

// recordMessage retains one relayed room message.
//
// Called after fan-out, on the frame the hub actually relayed, so what
// is stored is what the room saw: K_SRC already rewritten to the
// link-verified identity, nick already normalized. Recording the
// client's original envelope instead would persist a spoofable sender.
//
// Best-effort by design. A full disk or a read-only mount must degrade
// to "no history" rather than taking the room down, so failures are
// logged and dropped.
func (h *Hub) recordMessage(room string, env *rrc.Envelope) {
	if !h.historyEnabled() || room == "" {
		return
	}
	// MSG and ACTION are the conversation. NOTICE is hub-to-client
	// chatter — greetings, mode broadcasts, command replies — and
	// replaying it would re-narrate old events as if they were
	// happening now.
	if env.Type != rrc.TMsg && env.Type != rrc.TAction {
		return
	}
	body, ok := env.Body.(string)
	if !ok {
		return // non-text bodies are not replayable as text
	}
	nick := ""
	if env.Nick != nil {
		nick = *env.Nick
	}
	rec := history.Record{
		TimestampMs: env.TimestampMs,
		MsgID:       env.MsgID,
		Src:         env.Src,
		Nick:        nick,
		Type:        env.Type,
		Body:        body,
	}
	if err := h.history.Append(room, rec); err != nil {
		h.log.Printf("history: append to #%s failed: %v", room, err)
	}
}

// dropHistory discards a room's transcript. Used when an unregistered
// room is (re)created, so an ephemeral room's conversation does not
// outlive it — RRC's own semantics are that such a room dies with its
// last member, and history must not quietly resurrect it for whoever
// creates a room of the same name next.
func (h *Hub) dropHistory(room string) {
	if !h.historyEnabled() {
		return
	}
	if err := h.history.Drop(room); err != nil {
		h.log.Printf("history: drop #%s failed: %v", room, err)
	}
}

// pruneHistory expires and re-bounds the store. Runs on the hub's
// existing prune loop.
func (h *Hub) pruneHistory() {
	if !h.historyEnabled() {
		return
	}
	if err := h.history.Prune(); err != nil {
		h.log.Printf("history: prune failed: %v", err)
	}
}

// replayTo sends a room's recent messages to one session, bracketed by
// NOTICEs so a client can tell replayed traffic from live traffic
// without understanding anything new.
//
// The brackets are the whole compatibility story for this feature: an
// unmodified client renders them as two lines of hub text around some
// messages, which is exactly what they mean. A client that wants to do
// better — collapse the block, suppress notification sounds — can match
// on them, but nothing requires it to.
func (s *Session) replayTo(room string, q history.Query) int {
	h := s.hub
	if !h.historyEnabled() {
		return 0
	}
	recs, err := h.history.Recent(room, q)
	if err != nil {
		h.log.Printf("history: read #%s failed: %v", room, err)
		return 0
	}
	if len(recs) == 0 {
		return 0
	}

	s.sendNotice(&room, fmt.Sprintf("--- %s ---", replayHeader(len(recs), q)))
	for _, rec := range recs {
		s.send(recordEnvelope(room, rec))
	}
	s.sendNotice(&room, "--- end of history ---")
	return len(recs)
}

// replayHeader describes what is about to be replayed.
func replayHeader(n int, q history.Query) string {
	unit := "messages"
	if n == 1 {
		unit = "message"
	}
	if q.Since.IsZero() {
		return fmt.Sprintf("%d %s from earlier", n, unit)
	}
	return fmt.Sprintf("%d %s from the last %s", n, unit, humanDuration(time.Since(q.Since)))
}

// humanDuration renders a replay window the way a person would say it.
func humanDuration(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	case d >= 2*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d >= 2*time.Minute:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return "moment"
	}
}

// recordEnvelope rebuilds the envelope a retained record came from.
//
// K_ID and K_TS are the originals, not fresh values: a client that saw
// the message live and kept its id can recognise the replay as the same
// message instead of showing it twice, and the timestamp shown is when
// it was said rather than when it was replayed.
func recordEnvelope(room string, rec history.Record) *rrc.Envelope {
	env := &rrc.Envelope{
		Version:     rrc.Version,
		Type:        rec.Type,
		MsgID:       rec.MsgID,
		TimestampMs: rec.TimestampMs,
		Src:         rec.Src,
		Room:        &room,
		Body:        rec.Body,
	}
	if rec.Nick != "" {
		nick := rec.Nick
		env.Nick = &nick
	}
	return env
}
