// Package config loads the RRC hub's TOML configuration. The schema
// mirrors the reference Python hub rrcd's HubRuntimeConfig so an operator
// can run either hub from an equivalent config.
package config

import (
	"fmt"
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
	RoomRegistryPath          string   `toml:"room_registry_path"`
	KlinePath                 string   `toml:"kline_path"`
	RoomRegistryPruneAfter    Duration `toml:"room_registry_prune_after"`
	RoomRegistryPruneInterval Duration `toml:"room_registry_prune_interval"`
	RoomInviteTimeout         Duration `toml:"room_invite_timeout"`

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
			Name:                           "RRC Hub",
			Version:                        "rrc-hub-go/0.1.0",
			IdentityPath:                   "hub_identity",
			DestName:                       "rrc.hub",
			AnnounceOnStart:                true,
			AnnounceInterval:               Duration{5 * time.Minute},
			RoomRegistryPruneAfter:         Duration{30 * 24 * time.Hour},
			RoomRegistryPruneInterval:      Duration{time.Hour},
			RoomInviteTimeout:              Duration{15 * time.Minute},
			IncludeJoinedMemberList:        false,
			MaxSessions:                    256,
			MaxRooms:                       512,
			MaxRegisteredRoomsPerIdentity:  16,
			MaxRoomAclEntries:              256,
			PingInterval:                   Duration{0},
			PingTimeout:                    Duration{0},
			EnableResourceTransfer:         true,
			MaxResourceBytes:               262144,
			MaxPendingResourceExpectations: 8,
			ResourceExpectationTTL:         Duration{30 * time.Second},
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
