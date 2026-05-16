package hub

import (
	"encoding/hex"
	"strings"
	"unicode/utf8"

	"github.com/thatSFguy/reticulum-relay-chat/internal/rrc"
)

// roomLinksLocked collects the links of every member of a room. The
// caller must hold the hub mutex.
func roomLinksLocked(r *Room) []Link {
	out := make([]Link, 0, len(r.members))
	for s := range r.members {
		out = append(out, s.link)
	}
	return out
}

// fanout encodes env once and delivers it to every recipient link.
// Per-link send failures are logged but never abort the fan-out — a
// dead link is reclaimed when its own read loop ends.
func (h *Hub) fanout(recipients []Link, env *rrc.Envelope) {
	if len(recipients) == 0 {
		return
	}
	frame, err := env.Encode()
	if err != nil {
		h.log.Printf("rrc: fan-out encode failed (type %d): %v", env.Type, err)
		return
	}
	for _, l := range recipients {
		if e := l.Send(frame); e != nil {
			h.log.Printf("rrc: fan-out send failed: %v", e)
		}
	}
}

// clampNick truncates a nick to maxBytes, never splitting a UTF-8 rune.
func clampNick(nick string, maxBytes int) string {
	if maxBytes <= 0 || len(nick) <= maxBytes {
		return nick
	}
	b := []byte(nick)[:maxBytes]
	for len(b) > 0 && !utf8.Valid(b) {
		b = b[:len(b)-1]
	}
	return string(b)
}

// shortHash renders the first 4 bytes of an identity hash for logs.
func shortHash(h []byte) string {
	if len(h) == 0 {
		return "(unidentified)"
	}
	n := 4
	if len(h) < n {
		n = len(h)
	}
	return hex.EncodeToString(h[:n])
}

// chunkText splits a string into rune-safe chunks small enough to fit a
// single NOTICE envelope under the default link MTU — used for both the
// greeting and the room-directory advert.
func chunkText(text string) []string {
	if text == "" {
		return nil
	}
	const maxChunkBytes = 300
	var (
		chunks []string
		cur    strings.Builder
	)
	for _, ch := range text {
		if cur.Len()+utf8.RuneLen(ch) > maxChunkBytes && cur.Len() > 0 {
			chunks = append(chunks, cur.String())
			cur.Reset()
		}
		cur.WriteRune(ch)
	}
	if cur.Len() > 0 {
		chunks = append(chunks, cur.String())
	}
	return chunks
}
