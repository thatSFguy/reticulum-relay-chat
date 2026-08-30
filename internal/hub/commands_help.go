package hub

import (
	"fmt"
	"strings"
	"time"

	"github.com/thatSFguy/reticulum-relay-chat/internal/rrc"
)

// The command table.
//
// Every hub-local slash command is declared here once: how it is
// spelled, what it does, and who may run it. handleCommand dispatches
// from this table and /help prints it, so the two cannot disagree.
//
// The summaries are written for someone reading them on a phone over a
// LoRa link. One line each, no wrapping to spare, and they say what the
// command is FOR rather than what it is called.

// commandSpec is one hub-local slash command.
type commandSpec struct {
	name    string
	aliases []string
	usage   string
	summary string
	// serverOp marks a command only a configured server operator may
	// run, and which /help hides from everyone else. Room-operator
	// gating is per-room and stays inside the handler — a room op is
	// not a server op, and /help has no room to reason about.
	serverOp bool
	run      func(s *Session, parts []string, room string)
}

// commands is the table; commandIndex maps every name and alias into
// it. Both are built in init because cmdHelp reads the table, and a
// package-level initializer that reaches its own variable through a
// function body is an initialization cycle.
var (
	commands     []commandSpec
	commandIndex map[string]*commandSpec
)

func init() {
	commands = []commandSpec{
		// --- everyone ---
		{
			name:    "help",
			usage:   "/help [command]",
			summary: "list these commands, or explain one",
			run:     func(s *Session, p []string, r string) { s.cmdHelp(p, r) },
		},
		{
			name:    "version",
			usage:   "/version",
			summary: "hub software version, uptime, and which features are switched on",
			run:     func(s *Session, p []string, r string) { s.cmdVersion(p, r) },
		},
		{
			name:    "motd",
			usage:   "/motd",
			summary: "re-read the hub's greeting",
			run:     func(s *Session, p []string, r string) { s.cmdMOTD(p, r) },
		},
		{
			name:    "join",
			usage:   "/join <room>",
			summary: "join a room (most clients do this for you)",
			run:     func(s *Session, p []string, r string) { s.cmdJoin(p, r) },
		},
		{
			name:    "part",
			aliases: []string{"leave"},
			usage:   "/part [room]",
			summary: "leave a room",
			run:     func(s *Session, p []string, r string) { s.cmdPart(p, r) },
		},
		{
			name:    "link",
			usage:   "/link [room]",
			summary: "a link to this room you can paste anywhere, so somebody can find it",
			run:     func(s *Session, p []string, r string) { s.cmdLink(p, r) },
		},
		{
			name:    "list",
			usage:   "/list",
			summary: "registered public rooms",
			run:     func(s *Session, p []string, r string) { s.cmdList(p, r) },
		},
		{
			name:    "who",
			aliases: []string{"names"},
			usage:   "/who [room]",
			summary: "who is in a room right now",
			run:     func(s *Session, p []string, r string) { s.cmdWho(p, r) },
		},
		{
			name:    "whoami",
			usage:   "/whoami",
			summary: "your identity, the nick the hub filed for you, and how it can reach you",
			run:     func(s *Session, p []string, r string) { s.cmdWhoami(p, r) },
		},
		{
			name:    "seen",
			usage:   "/seen <nick|hashprefix>",
			summary: "when the hub last heard from someone",
			run:     func(s *Session, p []string, r string) { s.cmdSeen(p, r) },
		},
		{
			name:    "away",
			usage:   "/away [reason]",
			summary: "mark yourself away — mentions are then sent to you, not just shown here",
			run:     func(s *Session, p []string, r string) { s.cmdAway(p, r) },
		},
		{
			name:    "back",
			usage:   "/back",
			summary: "clear /away",
			run:     func(s *Session, p []string, r string) { s.cmdBack(p, r) },
		},
		{
			name:    "history",
			usage:   "/history [room] [count]",
			summary: "replay recent messages (operators: /history purge <room>)",
			run:     func(s *Session, p []string, r string) { s.cmdHistory(p, r) },
		},
		{
			name:    "notify",
			usage:   "/notify [status|on|off|address|test [full]]",
			summary: "mention notifications: your setting, and why one did or did not arrive",
			run:     func(s *Session, p []string, r string) { s.cmdNotify(p, r) },
		},
		{
			name:    "mentions",
			usage:   "/mentions [clear]",
			summary: "mentions the hub is holding for you",
			run:     func(s *Session, p []string, r string) { s.cmdMentions(p, r) },
		},
		{
			name:    "register",
			usage:   "/register <room>",
			summary: "keep a room and its history alive after everyone leaves (founder only)",
			run:     func(s *Session, p []string, r string) { s.cmdRegister(p, r) },
		},
		{
			name:    "unregister",
			usage:   "/unregister <room>",
			summary: "stop keeping a room (founder only; does not purge its transcript)",
			run:     func(s *Session, p []string, r string) { s.cmdUnregister(p, r) },
		},
		{
			name:    "topic",
			usage:   "/topic <room> [topic]",
			summary: "read or set a room's topic",
			run:     func(s *Session, p []string, r string) { s.cmdTopic(p, r) },
		},

		// --- room operators (gating is per room, inside the handler) ---
		{
			name:    "kick",
			usage:   "/kick <room> <nick|hashprefix>",
			summary: "remove someone from a room (room operators)",
			run:     func(s *Session, p []string, r string) { s.cmdKick(p, r) },
		},
		{
			name:    "op",
			aliases: []string{"deop", "voice", "devoice"},
			usage:   "/op <room> <nick|hashprefix>",
			summary: "grant or remove room privileges (room operators)",
			run:     func(s *Session, p []string, r string) { s.cmdOpVoice(strings.ToLower(p[0]), p, r) },
		},
		{
			name:    "mode",
			usage:   "/mode <room> [+|-flags]",
			summary: "read or change room modes (room operators)",
			run:     func(s *Session, p []string, r string) { s.cmdMode(p, r) },
		},
		{
			name:    "ban",
			usage:   "/ban <room> add|del|list [target]",
			summary: "per-room ban list (room operators)",
			run:     func(s *Session, p []string, r string) { s.cmdBan(p, r) },
		},
		{
			name:    "invite",
			usage:   "/invite <room> add|del|list [target]",
			summary: "per-room invite list (room operators)",
			run:     func(s *Session, p []string, r string) { s.cmdInvite(p, r) },
		},

		// --- server operators ---
		{
			name:     "stats",
			usage:    "/stats",
			summary:  "hub counters and limits",
			serverOp: true,
			run:      func(s *Session, p []string, r string) { s.cmdStats(p, r) },
		},
		{
			name:     "kline",
			usage:    "/kline add|del|list [nick|hashprefix|hash]",
			summary:  "hub-wide bans",
			serverOp: true,
			run:      func(s *Session, p []string, r string) { s.cmdKline(p, r) },
		},
		{
			name:     "reload",
			usage:    "/reload",
			summary:  "re-derive trust, klines and the room registry from disk",
			serverOp: true,
			run:      func(s *Session, p []string, r string) { s.cmdReload(p, r) },
		},
	}

	commandIndex = make(map[string]*commandSpec, len(commands)*2)
	for i := range commands {
		spec := &commands[i]
		commandIndex[spec.name] = spec
		for _, a := range spec.aliases {
			commandIndex[a] = spec
		}
	}
}

// --- /help ------------------------------------------------------------

// cmdHelp lists the commands this session may actually run.
//
// Hiding what the caller cannot use is the point: a list of twenty
// commands, six of which answer "not authorized", teaches nothing. A
// server operator sees the operator section; nobody else knows it is
// there.
func (s *Session) cmdHelp(parts []string, room string) {
	serverOp := s.isServerOp()

	if len(parts) >= 2 {
		name := strings.ToLower(strings.TrimPrefix(parts[1], "/"))
		spec := commandIndex[name]
		if spec == nil || (spec.serverOp && !serverOp) {
			s.sendNotice(roomPtr(room), "no such command '/"+snippet(name, 24)+"' — /help lists them")
			return
		}
		lines := []string{spec.usage, "  " + spec.summary}
		if len(spec.aliases) > 0 {
			lines = append(lines, "  also: /"+strings.Join(spec.aliases, ", /"))
		}
		s.sendNoticeLines(roomPtr(room), lines)
		return
	}

	lines := []string{"Commands on this hub (/help <command> for one):"}
	var opLines []string
	for i := range commands {
		spec := &commands[i]
		line := fmt.Sprintf("  %-46s %s", spec.usage, spec.summary)
		if spec.serverOp {
			opLines = append(opLines, line)
			continue
		}
		lines = append(lines, line)
	}
	if serverOp && len(opLines) > 0 {
		lines = append(lines, "Operator commands:")
		lines = append(lines, opLines...)
	}
	lines = append(lines,
		"Everything else you type is an ordinary message. Name someone with",
		"@nick (or @ plus 6+ characters of their identity hash) and the hub will",
		"tell them, even if they have gone.",
		"If a notification never reaches you, /notify test says why in ten seconds —",
		"usually that your messaging app is a different identity from the one you",
		"connect here with, which /notify address will show you.")
	s.sendNoticeLines(roomPtr(room), lines)
}

// --- /version ---------------------------------------------------------

// cmdVersion reports what this hub is and what it will do for you.
//
// /stats is operator-only, so without this an ordinary user has no way
// to answer "does this hub even keep history?" or "will it notify me?"
// — and both are the first question when a feature appears not to work.
func (s *Session) cmdVersion(_ []string, room string) {
	h := s.hub
	name := h.cfg.Name
	if name == "" {
		name = "(unnamed hub)"
	}
	version := h.cfg.Version
	if version == "" {
		version = "(unknown)"
	}

	features := []string{}
	if h.historyEnabled() {
		features = append(features, fmt.Sprintf("history: on — joins replay %d, /history up to %d, kept %s",
			h.cfg.HistoryReplayCount, h.cfg.HistoryPullCount,
			humanUptime(h.cfg.HistoryRetention.Duration)))
	} else {
		features = append(features, "history: off")
	}
	switch {
	case !h.cfg.MentionNotify:
		features = append(features, "mention notification: off")
	case h.mentionLXMFReady():
		features = append(features, "mention notification: on, including while you are away")
	default:
		features = append(features, "mention notification: on, but only while you are connected")
	}
	if h.cfg.EnableResourceTransfer {
		features = append(features, "large transfers: on")
	}

	lines := []string{
		fmt.Sprintf("%s — %s, RRC protocol v%d", name, version, rrc.Version),
		fmt.Sprintf("up %s", humanUptime(time.Duration(h.now()-h.startedAt)*time.Millisecond)),
	}
	for _, f := range features {
		lines = append(lines, "  "+f)
	}
	s.sendNoticeLines(roomPtr(room), lines)
}

// mentionLXMFReady reports whether a mention can reach somebody who is
// not connected. Both halves are needed: the operator's opt-in, and a
// notifier actually installed by the service layer.
func (h *Hub) mentionLXMFReady() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cfg.MentionNotify && h.cfg.MentionLXMF && h.notifier != nil
}

// humanUptime renders a duration the way an operator reads one.
func humanUptime(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	mins := int(d.Minutes()) % 60
	// A zero tail is noise: a retention of exactly a week is "7d", not
	// "7d 0h". This renders both an uptime and a configured period.
	switch {
	case days > 0 && hours > 0:
		return fmt.Sprintf("%dd %dh", days, hours)
	case days > 0:
		return fmt.Sprintf("%dd", days)
	case hours > 0 && mins > 0:
		return fmt.Sprintf("%dh %dm", hours, mins)
	case hours > 0:
		return fmt.Sprintf("%dh", hours)
	default:
		return fmt.Sprintf("%dm", mins)
	}
}

// --- /join, /part -----------------------------------------------------

// JOIN and PART are protocol frames, and a client that implements them
// as local commands never sends the text here. Not every client does,
// though — and the hub's own default greeting says "/join #lobby to
// start", so a client that passes the line through gets "unrecognized
// command" in answer to the hub's own instructions.
//
// These synthesize the frame the client would have sent, which keeps
// one implementation of joining rather than two.

func (s *Session) cmdJoin(parts []string, room string) {
	if len(parts) < 2 {
		s.sendNotice(roomPtr(room), "usage: /join <room>")
		return
	}
	target, err := s.validRoom(parts[1])
	if err != nil {
		s.sendNotice(roomPtr(room), "bad room: "+err.Error())
		return
	}
	// A room key, for a +k room: "/join room key".
	var body any
	if len(parts) >= 3 {
		body = strings.Join(parts[2:], " ")
	}
	s.handleJoin(&rrc.Envelope{Type: rrc.TJoin, Src: s.identity(), Room: &target, Body: body})
}

func (s *Session) cmdPart(parts []string, room string) {
	target := room
	if len(parts) >= 2 {
		target = parts[1]
	}
	if target == "" {
		s.sendNotice(nil, "usage: /part <room>")
		return
	}
	target, err := s.validRoom(target)
	if err != nil {
		s.sendNotice(roomPtr(room), "bad room: "+err.Error())
		return
	}
	s.handlePart(&rrc.Envelope{Type: rrc.TPart, Src: s.identity(), Room: &target})
}

// --- /link ------------------------------------------------------------

// cmdLink prints a link to a room, in the syntax NomadNet already uses
// for pages and conversations (SPEC §11.6.3, and docs/rrc-extensions.md).
//
// "Come to #ops" is not directions. There are many hubs, room names are
// not unique across them, and nothing a person can say out loud carries
// which hub they meant. This is the sentence they can paste instead.
func (s *Session) cmdLink(parts []string, room string) {
	h := s.hub
	target := room
	if len(parts) >= 2 {
		var err error
		target, err = s.validRoom(parts[1])
		if err != nil {
			s.sendNotice(roomPtr(room), "bad room: "+err.Error())
			return
		}
	}

	h.mu.Lock()
	link := h.roomLinkLocked(target)
	hubLink := h.hubLinkLocked()
	h.mu.Unlock()

	if hubLink == "" {
		// Only when the hub was never told its own destination hash,
		// which means an embedding that skipped SetDestHash.
		s.sendNotice(roomPtr(room), "this hub does not know its own address, so it cannot write a link")
		return
	}
	if target == "" {
		s.sendNoticeLines(roomPtr(room), []string{
			"this hub: " + hubLink,
			"  /link <room> for a link that opens a particular room",
		})
		return
	}
	s.sendNoticeLines(roomPtr(room), []string{
		"#" + target + ": " + link,
		"  paste that anywhere — a client that understands it opens the room,",
		"  and one that does not shows it as text somebody can still use",
	})
}

// --- /motd ------------------------------------------------------------

// cmdMOTD re-sends the greeting. It arrives once, at connect, above
// whatever the client then fills the screen with.
func (s *Session) cmdMOTD(_ []string, room string) {
	if s.hub.cfg.Greeting == "" {
		s.sendNotice(roomPtr(room), "this hub has no message of the day")
		return
	}
	s.sendGreeting()
}

// --- outbound helper --------------------------------------------------

// sendNoticeLines sends lines as as few NOTICEs as they fit into.
//
// sendNotice is one envelope, and these listings run past what a hub
// with a tightened max_msg_body_bytes will carry — a /help that is
// silently dropped for exceeding a limit is worse than no /help. Lines
// are packed rather than sent one per NOTICE because each envelope is a
// separate frame, and these clients may be a LoRa hop away.
func (s *Session) sendNoticeLines(room *string, lines []string) {
	budget := maxNoticeChunkChars
	if m := s.hub.limits.MaxMsgBodyBytes; m > 0 && m < budget {
		budget = m
	}

	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			s.sendNotice(room, cur.String())
			cur.Reset()
		}
	}
	for _, line := range lines {
		// A single over-long line is chunked on its own rather than
		// dropped; chunkText is rune-safe.
		if len(line) > budget {
			flush()
			for _, c := range chunkTextN(line, budget) {
				s.sendNotice(room, c)
			}
			continue
		}
		if cur.Len() > 0 && cur.Len()+1+len(line) > budget {
			flush()
		}
		if cur.Len() > 0 {
			cur.WriteString("\n")
		}
		cur.WriteString(line)
	}
	flush()
}
