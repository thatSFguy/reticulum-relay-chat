// Package config loads the RRC hub's TOML configuration.
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

// HubConfig holds the hub identity, announce cadence, and advertised limits.
type HubConfig struct {
	Name             string       `toml:"name"`
	Version          string       `toml:"version"`
	Greeting         string       `toml:"greeting"`
	IdentityPath     string       `toml:"identity_path"`
	AnnounceInterval Duration     `toml:"announce_interval"`
	Limits           LimitsConfig `toml:"limits"`
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

// Load reads and validates the config at path, filling in defaults.
func Load(path string) (*Config, error) {
	var c Config
	if _, err := toml.DecodeFile(path, &c); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if c.Hub.Name == "" {
		c.Hub.Name = "RRC Hub"
	}
	if c.Hub.Version == "" {
		c.Hub.Version = "rrc-hub-go/0.1.0"
	}
	if c.Hub.IdentityPath == "" {
		c.Hub.IdentityPath = "hub_identity"
	}
	if c.Hub.AnnounceInterval.Duration <= 0 {
		c.Hub.AnnounceInterval.Duration = 5 * time.Minute
	}
	applyLimitDefaults(&c.Hub.Limits)
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
		l.MaxMsgBodyBytes = 4096
	}
	if l.MaxRoomsPerSession <= 0 {
		l.MaxRoomsPerSession = 16
	}
	if l.RateLimitMsgsPerMin <= 0 {
		l.RateLimitMsgsPerMin = 30
	}
}
