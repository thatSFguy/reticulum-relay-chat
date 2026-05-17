package hub

import (
	"encoding/hex"
	"fmt"
	"strings"
	"unicode/utf8"
)

// maxNoticeChunkChars is the line-based NOTICE chunk size (rrcd's
// MAX_NOTICE_CHUNK_CHARS).
const maxNoticeChunkChars = 512

// roomLinksLocked collects the links of every member of a room. The
// caller must hold the hub mutex.
func roomLinksLocked(r *Room) []Link {
	out := make([]Link, 0, len(r.members))
	for s := range r.members {
		out = append(out, s.link)
	}
	return out
}

// roomLinksExceptLocked collects member links excluding one session.
func roomLinksExceptLocked(r *Room, except *Session) []Link {
	out := make([]Link, 0, len(r.members))
	for s := range r.members {
		if s == except {
			continue
		}
		out = append(out, s.link)
	}
	return out
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

// shortHex12 renders the first 12 hex chars of an identity hash.
func shortHex12(h []byte) string {
	s := hex.EncodeToString(h)
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// normHex lowercases a hex hash and strips an optional "0x" prefix and
// surrounding whitespace.
func normHex(s string) string {
	s = strings.TrimSpace(strings.ToLower(s))
	s = strings.TrimPrefix(s, "0x")
	return s
}

// identityHashLen is the byte length of a full RNS identity hash
// (SHA-256 truncated to 16 bytes — rns.IdentityHashLen).
const identityHashLen = 16

// parseHexHash parses a token as a full identity hash for use as a STORED
// ban/kline/invite key: optional "0x" prefix, whitespace tolerated, must
// be valid lowercase hex of exactly identityHashLen bytes. A short prefix
// is rejected (A14): ban sets are keyed by the full 16-byte hash, so a
// stored short entry would silently never match isBanned — a dead ban.
// Live-session prefix matching stays valid via resolveTargetLocked.
func parseHexHash(tok string) (string, error) {
	n := normHex(tok)
	b, err := hex.DecodeString(n)
	if err != nil {
		return "", fmt.Errorf("not valid hex")
	}
	if len(b) != identityHashLen {
		return "", fmt.Errorf("identity hash must be exactly %d bytes", identityHashLen)
	}
	return n, nil
}

// aclHasRoom reports whether a per-room ACL set (ops / voiced / bans)
// can take one more entry without exceeding the configured cap (audit
// A15). A cap <= 0 disables the limit; re-adding an already-present key
// is always allowed since it does not grow the set.
func aclHasRoom[V any](m map[string]V, key string, cap int) bool {
	if cap <= 0 {
		return true
	}
	if _, exists := m[key]; exists {
		return true
	}
	return len(m) < cap
}

// looksLikeHashPrefix reports whether a token should be treated as an
// identity-hash prefix rather than a nick: all-hex (after 0x strip) and
// at least 6 chars long.
func looksLikeHashPrefix(tok string) bool {
	n := normHex(tok)
	if len(n) < 6 {
		return false
	}
	for _, c := range n {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// normalizeNick validates and trims a nick. Returns ("", false) when the
// nick is invalid (control chars, invalid UTF-8, or too long) so the
// caller drops it.
func normalizeNick(nick string, maxBytes int) (string, bool) {
	nick = strings.TrimSpace(nick)
	if nick == "" {
		return "", false
	}
	if !utf8.ValidString(nick) {
		return "", false
	}
	if strings.ContainsAny(nick, "\n\r\x00") {
		return "", false
	}
	if maxBytes > 0 && len(nick) > maxBytes {
		return "", false
	}
	return nick, true
}

// chunkText splits text into rune-safe NOTICE chunks of at most
// maxNoticeChunkChars characters, preferring line boundaries.
func chunkText(text string) []string {
	if text == "" {
		return nil
	}
	var chunks []string
	for _, line := range strings.Split(text, "\n") {
		if utf8.RuneCountInString(line) <= maxNoticeChunkChars {
			chunks = append(chunks, line)
			continue
		}
		var cur strings.Builder
		count := 0
		for _, ch := range line {
			if count >= maxNoticeChunkChars {
				chunks = append(chunks, cur.String())
				cur.Reset()
				count = 0
			}
			cur.WriteRune(ch)
			count++
		}
		if cur.Len() > 0 {
			chunks = append(chunks, cur.String())
		}
	}
	return chunks
}

// targetMatch is one resolved peer for a /command target token.
type targetMatch struct {
	session *Session
	hashHex string
	nick    string
}

// resolveTarget resolves a command-argument token to connected peers. If
// the token looks like a hash prefix (>= 6 hex chars) it matches peers by
// hex-prefix; otherwise it matches by nick (case-insensitive). When room
// is non-nil, matches are filtered to that room. Caller must hold h.mu.
func (h *Hub) resolveTargetLocked(tok string, room *Room) []targetMatch {
	var pool []*Session
	if room != nil {
		for s := range room.members {
			pool = append(pool, s)
		}
	} else {
		for s := range h.sessions {
			pool = append(pool, s)
		}
	}

	var matches []targetMatch
	if looksLikeHashPrefix(tok) {
		prefix := normHex(tok)
		for _, s := range pool {
			id := s.identity()
			if id == nil {
				continue
			}
			hh := hex.EncodeToString(id)
			if strings.HasPrefix(hh, prefix) {
				s.mu.Lock()
				nick := s.nick
				s.mu.Unlock()
				matches = append(matches, targetMatch{s, hh, nick})
			}
		}
		return matches
	}
	want := strings.ToLower(strings.TrimSpace(tok))
	for _, s := range pool {
		s.mu.Lock()
		nick := s.nick
		s.mu.Unlock()
		if nick != "" && strings.ToLower(nick) == want {
			hh := ""
			if id := s.identity(); id != nil {
				hh = hex.EncodeToString(id)
			}
			matches = append(matches, targetMatch{s, hh, nick})
		}
	}
	return matches
}

// ambiguityNotice renders the multi-match disambiguation text.
func ambiguityNotice(matches []targetMatch) string {
	var b strings.Builder
	b.WriteString("ambiguous target — multiple matches:")
	for _, m := range matches {
		b.WriteString(fmt.Sprintf("\n  - %s nick='%s'", m.hashHex, m.nick))
	}
	b.WriteString("\nUse full or longer identity hash to disambiguate.")
	return b.String()
}

// renderMember renders a member for /who output.
func renderMember(s *Session) string {
	id := s.identity()
	s.mu.Lock()
	nick := s.nick
	s.mu.Unlock()
	if id == nil {
		return "(unidentified)"
	}
	full := hex.EncodeToString(id)
	if nick != "" {
		short := full
		if len(short) > 12 {
			short = short[:12]
		}
		return fmt.Sprintf("%s (%s)", nick, short)
	}
	return full
}
