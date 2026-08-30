// Package config loads the RRC hub's TOML configuration. The schema
// mirrors the reference Python hub rrcd's HubRuntimeConfig so an operator
// can run either hub from an equivalent config.
package config

import (
	"fmt"
	"strings"
	"time"

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

	MentionNotify       bool   `toml:"mention_notify"`
	PeerRegistryPath    string `toml:"peer_registry_path"`
	MaxKnownPeers       int    `toml:"max_known_peers"`
	MaxPendingMentions  int    `toml:"max_pending_mentions"`
	MentionSnippetBytes int    `toml:"mention_snippet_bytes"`
	// MentionLXMF hands a waiting mention to an LXMF propagation node,
	// where the recipient's own client collects it — the only way to
	// reach someone whose RRC link is gone. Without it a mention still
	// waits, but nothing tells them to come and look.
	MentionLXMF bool `toml:"mention_lxmf"`
	// LXMFDisplayName is the name the hub's lxmf.delivery destination
	// announces under. Empty derives one from Name.
	//
	// It is separate from Name because the two destinations mean
	// different things to whoever is browsing announces. The rrc.hub
	// entry is a place to join; the lxmf.delivery entry is a sender
	// that will never read a reply — but a messaging client lists it
	// beside real people, and under the same name it is indistinguishable
	// from one. See LXMFName.
	LXMFDisplayName string `toml:"lxmf_display_name"`
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
			MentionNotify:                  false,
			PeerRegistryPath:               "peers.toml",
			MaxKnownPeers:                  2048,
			MaxPendingMentions:             20,
			MentionSnippetBytes:            140,
			DefaultRooms:                   []string{"lobby"},
			MentionLXMF:                    false,
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

// LXMFNotifySuffix is appended to Name when no lxmf_display_name is
// configured.
const LXMFNotifySuffix = " — RRC notifications"

// LXMFName is the display name for the hub's lxmf.delivery announce.
//
// Defaults to the hub name plus a suffix saying what the destination
// is, because the alternative is what shipped before: the hub appearing
// twice in an announce list under one name, once as a room to join and
// once as somebody to message. Only the first of those is true.
func (h HubConfig) LXMFName() string {
	if n := strings.TrimSpace(h.LXMFDisplayName); n != "" {
		return n
	}
	name := strings.TrimSpace(h.Name)
	if name == "" {
		return "RRC notifications"
	}
	return name + LXMFNotifySuffix
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
