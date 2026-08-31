// Package config loads the RRC hub's TOML configuration. The schema
// mirrors the reference Python hub rrcd's HubRuntimeConfig so an operator
// can run either hub from an equivalent config.
package config

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/BurntSushi/toml"
)

// Config is the parsed rrc-hub configuration file.
type Config struct {
	Hub        HubConfig         `toml:"hub"`
	Interfaces []InterfaceConfig `toml:"interfaces"`
}

// HubConfig holds the hub identity, policy, persistence, and protocol
// settings. Field defaults are applied by Load and match rrcd.
type HubConfig struct {
	// Identity and presence.
	Name             string   `toml:"name"`
	Version          string   `toml:"version"`
	Greeting         string   `toml:"greeting"`
	IdentityPath     string   `toml:"identity_path"`
	DestName         string   `toml:"dest_name"`
	AnnounceOnStart  bool     `toml:"announce_on_start"`
	AnnounceInterval Duration `toml:"announce_interval"`

	// Trust model. Hex identity hashes; trusted ones are server
	// operators, banned ones are refused at link-identify time.
	TrustedIdentities []string `toml:"trusted_identities"`
	BannedIdentities  []string `toml:"banned_identities"`

	// Persistence.
	RoomRegistryPath string `toml:"room_registry_path"`
	// DefaultRooms are created, registered, at every start so a brand
	// new hub is not an empty prompt. Absent from the config means
	// ["lobby"]; an explicit empty list means none.
	//
	// They are REGISTERED because that is what makes a room real when
	// nobody is in it: /list only shows registered rooms, and an
	// unregistered one is dropped the moment its last member parts,
	// taking its transcript with it. A default room that evaporated
	// between visitors would not be a default room.
	//
	// They get no founder and no operators. The room belongs to the
	// hub, not to whoever happened to arrive first — server operators
	// (hub.trusted) can already administer any room, so it is not
	// ownerless in practice.
	DefaultRooms              []string `toml:"default_rooms"`
	KlinePath                 string   `toml:"kline_path"`
	RoomRegistryPruneAfter    Duration `toml:"room_registry_prune_after"`
	RoomRegistryPruneInterval Duration `toml:"room_registry_prune_interval"`

	// MentionPushInterval is how often the hub tries to deliver mentions
	// waiting for peers who are not connected.
	//
	// It has its own timer because it used to share the room-registry
	// prune timer, whose default is an hour — so a hub with mention
	// notification switched on would sit on somebody's message for up
	// to an hour before the first delivery attempt, which is not a
	// notification. The two jobs have nothing to do with each other:
	// pruning stale rooms is housekeeping, and this is the feature.
	MentionPushInterval Duration `toml:"mention_push_interval"`
	RoomInviteTimeout   Duration `toml:"room_invite_timeout"`

	// Behavior.
	IncludeJoinedMemberList bool `toml:"include_joined_member_list"`

	// DoS caps. A zero (or negative) value disables the individual cap,
	// letting an operator opt out. Defaults are applied by Load.
	MaxSessions                   int `toml:"max_sessions"`
	MaxRooms                      int `toml:"max_rooms"`
	MaxRegisteredRoomsPerIdentity int `toml:"max_registered_rooms_per_identity"`
	MaxRoomAclEntries             int `toml:"max_room_acl_entries"`

	// MaxTopicBytes bounds a room topic. Server-side, and deliberately
	// NOT part of LimitsConfig: that set is advertised in WELCOME, and
	// adding a key to it is a wire change every client would have to be
	// taught. A topic is persisted to rooms.toml and re-sent to every
	// joiner in the room-info NOTICE, so it needs a bound of its own
	// rather than inheriting whatever max_msg_body_bytes happens to be.
	//
	// The default is 256 rather than 350 so that a MAXIMUM-length topic
	// still fits inside the command that sets it: a topic arrives as
	// "/topic <room> <text>", the whole line is bounded by
	// max_msg_body_bytes (350), and a 64-byte room name plus the verb
	// costs ~73 of those. Setting this equal to max_msg_body_bytes
	// would make the last ~90 bytes of the range unreachable — a limit
	// that advertises more than it can accept.
	MaxTopicBytes int `toml:"max_topic_bytes"`

	// Hub-initiated keepalive. A zero PingInterval disables hub PINGs; a
	// zero PingTimeout disables tearing a link down for a missing PONG.
	PingInterval Duration `toml:"ping_interval"`
	PingTimeout  Duration `toml:"ping_timeout"`

	// Large-payload transfer over RNS Resource.
	EnableResourceTransfer         bool     `toml:"enable_resource_transfer"`
	MaxResourceBytes               int      `toml:"max_resource_bytes"`
	MaxPendingResourceExpectations int      `toml:"max_pending_resource_expectations"`
	ResourceExpectationTTL         Duration `toml:"resource_expectation_ttl"`

	// History — the retained room transcript and the replay a joining
	// client gets. Off by default: turning it on means the hub starts
	// keeping plaintext conversation on disk, which is an operator's
	// decision to make, not one an upgrade should make for them.
	HistoryEnabled         bool     `toml:"history_enabled"`
	HistoryPath            string   `toml:"history_path"`
	HistoryRetention       Duration `toml:"history_retention"`
	HistoryMaxBytesPerRoom int64    `toml:"history_max_bytes_per_room"`
	HistoryMaxTotalBytes   int64    `toml:"history_max_total_bytes"`
	// HistoryReplayCount / HistoryReplayBytes bound the automatic replay
	// sent on JOIN. They are deliberately small: a client may be on a
	// LoRa link where a week of backlog is minutes of airtime, so the
	// join replay is a taste of the conversation and /history is how a
	// client asks for more.
	HistoryReplayCount int `toml:"history_replay_count"`
	HistoryReplayBytes int `toml:"history_replay_bytes"`
	// HistoryPullCount / HistoryPullBytes bound one /history request.
	HistoryPullCount int `toml:"history_pull_count"`
	HistoryPullBytes int `toml:"history_pull_bytes"`

	// Mentions — telling someone they were named while they were not
	// looking. Independent of history: a hub can notify without
	// retaining, or retain without notifying.
	// UniqueNicks makes the hub GRANT a nickname rather than accept
	// one: the first identity to claim a name keeps it, and a later
	// claimant becomes sam1, sam2, and so on.
	//
	// On by default, because the alternative is not "names are
	// flexible" but "@mentions silently reach nobody" — resolution
	// declines rather than guess between two people answering to the
	// same name, and says nothing about having declined.
	UniqueNicks bool `toml:"unique_nicks"`

	// MentionNotify turns on mention detection and the peer directory
	// that makes it work.
	//
	// On by default. It was off, on the reasoning that a hub should not
	// start keeping a record of who has visited without being asked —
	// but the effect of that default was that the hub's most useful
	// feature was invisible until an operator read the config closely
	// enough to find it, and "@nick" silently did nothing on every hub
	// nobody had tuned.
	//
	// Be aware of what it turns on: the hub begins retaining a
	// directory of identities it has met — public key, last nickname,
	// last seen — at PeerRegistryPath. That is what makes somebody
	// addressable after their link is gone, and there is no way to
	// notify an absent peer without it. Set false to keep no such
	// record.
	MentionNotify       bool   `toml:"mention_notify"`
	PeerRegistryPath    string `toml:"peer_registry_path"`
	MaxKnownPeers       int    `toml:"max_known_peers"`
	MaxPendingMentions  int    `toml:"max_pending_mentions"`
	MentionSnippetBytes int    `toml:"mention_snippet_bytes"`
	// MentionLXMF delivers a waiting mention over LXMF — directly when
	// the recipient answers, and otherwise into store-and-forward for
	// their client to collect. It is the only way to reach somebody
	// whose RRC link is gone. Without it a mention still waits, but
	// nothing tells them to come and look, which is the difference
	// between a feature and a footnote.
	//
	// On by default, with a consequence worth knowing: the hub then
	// ANNOUNCES an lxmf.delivery destination. It has to — a recipient
	// who has never heard that announce holds no public key to verify
	// the signature against and drops every notification in silence
	// (see the incident registry). Every messaging client on the mesh
	// will therefore list the hub as a contact. It announces under
	// LXMFName() rather than the hub's own name, and answers anyone
	// who messages it, so it does not masquerade as a person.
	MentionLXMF bool `toml:"mention_lxmf"`
	// LXMFIdentityPath is where the identity behind the hub's OWN
	// lxmf.delivery destination is stored. Created on first run.
	// Empty derives it from IdentityPath — see LXMFIdentityFile.
	//
	// That destination now hangs off an identity of its OWN. It used to
	// share the hub's: different aspects, so the two destination hashes
	// always differed, but one public key and one identity hash behind
	// both of them. Any client that keys what it stores by identity
	// rather than by destination therefore saw the hub's two announces
	// as one entry — and the later announce overwrote the earlier, which
	// is a hub that turns into a "(noreply)" contact and stops looking
	// like somewhere you can join a room. Separate identities remove the
	// shared key that makes that collapse possible.
	//
	// The cost is that the notification address changes when a hub that
	// ran an older build upgrades: the new identity is generated on
	// first run, so recipients see notifications from a new sender and
	// any reply thread against the old address is orphaned. Point this
	// at the hub identity file to keep the old address instead.
	LXMFIdentityPath string `toml:"lxmf_identity_path"`
	// LXMFPropagationNode pins the store-and-forward node the fallback
	// route uses. Empty auto-selects; see internal/service/propnodes.go.
	LXMFPropagationNode string `toml:"lxmf_propagation_node"`
	// LXMFPropagationFanout is how many auto-selected nodes one
	// notification is left with. Ignored when a node is pinned.
	//
	// More than one because the hub cannot know which node the recipient
	// syncs from — nothing in an announce says — so a single choice out
	// of the dozens announcing is close to a guess, and a message parked
	// on the wrong node is never seen. Copies cost the sender airtime
	// and cost the recipient nothing: LXMF dedupes on message_id, so a
	// message that arrives twice is shown once.
	//
	// Kept small deliberately. This is the FALLBACK route, reached only
	// after direct delivery has already failed, and each copy is its own
	// link handshake and transfer to a different node.
	LXMFPropagationFanout int `toml:"lxmf_propagation_fanout"`

	Limits LimitsConfig `toml:"limits"`
}

// LimitsConfig is the client-facing limit set advertised in WELCOME.
type LimitsConfig struct {
	MaxNickBytes        int `toml:"max_nick_bytes"`
	MaxRoomNameBytes    int `toml:"max_room_name_bytes"`
	MaxMsgBodyBytes     int `toml:"max_msg_body_bytes"`
	MaxRoomsPerSession  int `toml:"max_rooms_per_session"`
	RateLimitMsgsPerMin int `toml:"rate_limit_msgs_per_minute"`
}

// InterfaceConfig is one Reticulum transport attachment. Only
// "tcp_client" is supported — attach to an rnsd TCPServerInterface.
type InterfaceConfig struct {
	Type    string `toml:"type"`
	Address string `toml:"address"` // host:port
}

// Duration is a TOML-friendly wrapper around time.Duration ("5m", "1h").
type Duration struct{ time.Duration }

// UnmarshalText parses a Go duration string from the TOML value.
func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

// MarshalText renders the duration back to a Go duration string.
func (d Duration) MarshalText() ([]byte, error) {
	return []byte(d.Duration.String()), nil
}

// defaults returns a Config pre-populated with rrcd-equivalent defaults.
// toml.DecodeFile only overwrites keys present in the file, so any key
// the operator omits keeps the value set here — this is how the bool
// fields default to true.
func defaults() Config {
	return Config{
		Hub: HubConfig{
			Name:    "thatSFguy",
			Version: DefaultVersion,
			// Says what this hub is, because it is not what a client
			// connecting to it will assume. Everything named here is
			// invisible until you know to look for it: history arrives
			// as ordinary replayed messages and a mention notification
			// arrives somewhere else entirely. Operators are expected
			// to replace this with their own words.
			Greeting:        "Welcome. This is rrc-hub by thatSFguy - RRC, plus some things a standard hub does not do:\n  - rooms open with the recent conversation, not a blank screen\n  - name someone with @nick while they are away and the hub delivers it to their LXMF inbox, so they actually find out\nNone of it needs a special client - your existing one already speaks everything used here.\n/join #lobby to start, /help for commands.",
			IdentityPath:    "hub_identity",
			DestName:        "rrc.hub",
			AnnounceOnStart: true,
			// 30 minutes, not 5. A hub announces TWO destinations
			// (rrc.hub and lxmf.delivery), so the interval is really
			// two packets flooded across the mesh per tick, forever —
			// and upstream RNS transport nodes police this: an
			// interface with announce_rate_target set counts violations
			// per destination and, past announce_rate_grace, applies
			// announce_rate_penalty (RNS/Transport.py:2219-2240).
			// Sustained 5-minute announces on a public node earned
			// exactly that treatment during testing.
			//
			// Nothing needs the faster rate. Path discovery for a
			// client that has never heard the hub goes through a path
			// request, not the periodic announce, and cached paths
			// outlive 30 minutes comfortably.
			AnnounceInterval:              Duration{30 * time.Minute},
			RoomRegistryPruneAfter:        Duration{30 * 24 * time.Hour},
			RoomRegistryPruneInterval:     Duration{time.Hour},
			MentionPushInterval:           Duration{time.Minute},
			RoomInviteTimeout:             Duration{15 * time.Minute},
			IncludeJoinedMemberList:       false,
			MaxSessions:                   256,
			MaxRooms:                      512,
			MaxRegisteredRoomsPerIdentity: 16,
			MaxRoomAclEntries:             256,
			MaxTopicBytes:                 256,
			// Keepalive ON by default. These used to default to 0,
			// which disabled hub PINGs *and* link teardown on a missing
			// PONG — so a hub never learned that a client had gone and
			// listed dead peers as present in a room forever. Every
			// feature that turns on "is this person here?" was wrong out
			// of the box, mention notification worst of all: the people
			// most in need of an offline notification (dropped link,
			// killed app, dead battery) were exactly the ones the hub
			// still believed were in the room. Set either to 0 to
			// restore the old behaviour deliberately.
			PingInterval:                   Duration{30 * time.Second},
			PingTimeout:                    Duration{60 * time.Second},
			EnableResourceTransfer:         true,
			MaxResourceBytes:               262144,
			MaxPendingResourceExpectations: 8,
			ResourceExpectationTTL:         Duration{30 * time.Second},
			HistoryEnabled:                 false,
			HistoryPath:                    "history",
			HistoryRetention:               Duration{7 * 24 * time.Hour},
			HistoryMaxBytesPerRoom:         4 * 1024 * 1024,
			HistoryMaxTotalBytes:           128 * 1024 * 1024,
			HistoryReplayCount:             10,
			HistoryReplayBytes:             2048,
			HistoryPullCount:               100,
			HistoryPullBytes:               16384,
			UniqueNicks:                    true,
			MentionNotify:                  true,
			PeerRegistryPath:               "peers.toml",
			MaxKnownPeers:                  2048,
			MaxPendingMentions:             20,
			MentionSnippetBytes:            140,
			DefaultRooms:                   []string{"lobby"},
			MentionLXMF:                    true,
			LXMFPropagationFanout:          2,
			Limits: LimitsConfig{
				MaxNickBytes:        32,
				MaxRoomNameBytes:    64,
				MaxMsgBodyBytes:     350,
				MaxRoomsPerSession:  32,
				RateLimitMsgsPerMin: 240,
			},
		},
	}
}

// Load reads and validates the config at path, filling in defaults.
func Load(path string) (*Config, error) {
	c := defaults()
	if _, err := toml.DecodeFile(path, &c); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	applyLimitDefaults(&c.Hub.Limits)
	applyHistoryDefaults(&c.Hub)
	applyMentionDefaults(&c.Hub)
	if c.Hub.MaxResourceBytes <= 0 {
		c.Hub.MaxResourceBytes = 262144
	}
	if c.Hub.MaxTopicBytes <= 0 {
		c.Hub.MaxTopicBytes = 256
	}
	if c.Hub.MaxPendingResourceExpectations <= 0 {
		c.Hub.MaxPendingResourceExpectations = 8
	}
	if c.Hub.ResourceExpectationTTL.Duration <= 0 {
		c.Hub.ResourceExpectationTTL.Duration = 30 * time.Second
	}
	if len(c.Interfaces) == 0 {
		return nil, fmt.Errorf("config: at least one [[interfaces]] block is required")
	}
	for i, iface := range c.Interfaces {
		if iface.Type != "tcp_client" {
			return nil, fmt.Errorf("config: interface %d: unsupported type %q (only tcp_client)", i, iface.Type)
		}
		if iface.Address == "" {
			return nil, fmt.Errorf("config: interface %d: address is required", i)
		}
	}
	return &c, nil
}

// applyHistoryDefaults fills in any history knob an operator left unset.
// A hub that enables history without tuning it must still get bounded,
// sane behavior rather than an unbounded store or an empty replay.
func applyHistoryDefaults(h *HubConfig) {
	d := defaults().Hub
	if h.HistoryPath == "" {
		h.HistoryPath = d.HistoryPath
	}
	if h.HistoryRetention.Duration <= 0 {
		h.HistoryRetention = d.HistoryRetention
	}
	if h.HistoryMaxBytesPerRoom <= 0 {
		h.HistoryMaxBytesPerRoom = d.HistoryMaxBytesPerRoom
	}
	if h.HistoryMaxTotalBytes <= 0 {
		h.HistoryMaxTotalBytes = d.HistoryMaxTotalBytes
	}
	if h.HistoryReplayCount < 0 {
		h.HistoryReplayCount = 0
	} else if h.HistoryReplayCount == 0 {
		h.HistoryReplayCount = d.HistoryReplayCount
	}
	if h.HistoryReplayBytes <= 0 {
		h.HistoryReplayBytes = d.HistoryReplayBytes
	}
	if h.HistoryPullCount <= 0 {
		h.HistoryPullCount = d.HistoryPullCount
	}
	if h.HistoryPullBytes <= 0 {
		h.HistoryPullBytes = d.HistoryPullBytes
	}
}

// applyMentionDefaults fills in any unset mention knob, so enabling
// notifications without tuning them still yields bounded behavior.
func applyMentionDefaults(h *HubConfig) {
	d := defaults().Hub
	if h.PeerRegistryPath == "" {
		h.PeerRegistryPath = d.PeerRegistryPath
	}
	if h.MaxKnownPeers <= 0 {
		h.MaxKnownPeers = d.MaxKnownPeers
	}
	if h.MaxPendingMentions <= 0 {
		h.MaxPendingMentions = d.MaxPendingMentions
	}
	if h.MentionSnippetBytes <= 0 {
		h.MentionSnippetBytes = d.MentionSnippetBytes
	}
	if h.LXMFPropagationFanout <= 0 {
		h.LXMFPropagationFanout = d.LXMFPropagationFanout
	}
	// A hub that fanned out to every node it has heard would be a
	// broadcast amplifier with its own return address on it, on a mesh
	// where announces are free and unauthenticated.
	if h.LXMFPropagationFanout > maxPropagationFanout {
		h.LXMFPropagationFanout = maxPropagationFanout
	}
}

// maxPropagationFanout caps lxmf_propagation_fanout however it is
// configured.
const maxPropagationFanout = 5

func applyLimitDefaults(l *LimitsConfig) {
	if l.MaxNickBytes <= 0 {
		l.MaxNickBytes = 32
	}
	if l.MaxRoomNameBytes <= 0 {
		l.MaxRoomNameBytes = 64
	}
	if l.MaxMsgBodyBytes <= 0 {
		l.MaxMsgBodyBytes = 350
	}
	if l.MaxRoomsPerSession <= 0 {
		l.MaxRoomsPerSession = 32
	}
	if l.RateLimitMsgsPerMin <= 0 {
		l.RateLimitMsgsPerMin = 240
	}
}

// DefaultsForTest exposes the shipped defaults so tests can assert on
// them. The defaults are part of the product — several features only
// work if they are right — and nothing else can check that.
func DefaultsForTest() HubConfig { return defaults().Hub }

// LXMFNoReplySuffix marks the hub's lxmf.delivery destination as one
// that sends but never reads.
//
// Derived, not configurable. The name is not decoration: a messaging
// client lists this destination beside real people, and whether it is
// a correspondent or a one-way notifier is a fact about the software,
// not a preference. An operator who could set it freely could set it to
// something indistinguishable from a person — which is the exact
// confusion this exists to prevent — and every hub spelling the same
// property differently would leave users with nothing to recognise.
const LXMFNoReplySuffix = "(noreply)"

// lxmfNameMaxBytes bounds the whole derived name.
//
// The msgpack bin8 header the display name is written with (SPEC §4.3)
// tops out at 255, but an announce is broadcast repeatedly to the whole
// mesh and may cross a LoRa link, so the ceiling is not the budget. 64
// leaves 55 for the hub's own name, comfortably more than the 32 bytes
// RRC allows a nickname.
const lxmfNameMaxBytes = 64

// LXMFIdentityFile is where the notification identity is stored:
// LXMFIdentityPath when the operator set one, otherwise IdentityPath
// with ".lxmf" appended.
//
// Derived from IdentityPath rather than fixed at "hub_identity.lxmf" so
// that an operator who moved the hub identity — a data directory, a
// mounted volume, one process per hub in one working directory — gets
// the second key beside the first without having to learn that a
// second key exists. A key written somewhere the operator does not back
// up is a notification address that changes the next time the container
// is recreated.
func (h HubConfig) LXMFIdentityFile() string {
	if p := strings.TrimSpace(h.LXMFIdentityPath); p != "" {
		return p
	}
	base := strings.TrimSpace(h.IdentityPath)
	if base == "" {
		base = "hub_identity"
	}
	return base + ".lxmf"
}

// LXMFName is the display name for the hub's lxmf.delivery announce:
// the hub's name with LXMFNoReplySuffix appended, truncated to fit.
//
// The suffix is never what gets dropped. It is the part that carries
// the meaning — a name truncated to "MichMesh RRC Hu" is merely odd,
// while one that silently loses "(noreply)" is a hub posing as somebody
// you can talk to. So the base is trimmed to make room, on a rune
// boundary, and any whitespace the trim exposes goes too.
//
// The hub's rrc.hub announce is unaffected and keeps the full name.
func (h HubConfig) LXMFName() string {
	base := strings.TrimSpace(h.Name)
	if base == "" {
		base = "RRC hub"
	}
	room := lxmfNameMaxBytes - len(LXMFNoReplySuffix)
	if len(base) > room {
		base = base[:room]
		// Rune boundary: a name cut mid-character is invalid UTF-8, and
		// upstream's display_name_from_app_data does dn.decode("utf-8")
		// (SPEC §9.3) — which fails, and the name vanishes entirely.
		for len(base) > 0 && !utf8.ValidString(base) {
			base = base[:len(base)-1]
		}
		base = strings.TrimRight(base, " \t")
	}
	return base + LXMFNoReplySuffix
}

// VersionPrefix and DefaultVersion are the software version advertised
// to clients in WELCOME.
//
// DefaultVersion is a sentinel rather than a literal: cmd/rrc-hub
// replaces it with the real build version (set by -ldflags at release
// time) unless the operator configured their own string. Hardcoding the
// number here meant remembering to bump it on every tag, and it was
// already wrong once — a 0.2.0 build advertising 0.1.0.
const (
	VersionPrefix  = "rrc-hub-go/"
	DefaultVersion = VersionPrefix + "dev"
)
