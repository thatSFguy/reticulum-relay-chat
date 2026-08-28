package hub

import (
	"context"
	"testing"
	"time"

	"github.com/thatSFguy/reticulum-relay-chat/internal/config"
)

// The defaults decide whether this feature works for someone who
// downloads a binary and switches it on, so they are worth pinning.
//
// All three of these shipped wrong: mention delivery rode the
// room-registry prune timer (1h), and keepalive was off, which meant the
// hub never learned anyone had left and so could never see them as
// absent in the first place.
func TestTheShippedDefaultsCanActuallyDeliverAMention(t *testing.T) {
	d := config.DefaultsForTest()

	if d.MentionPushInterval.Duration <= 0 {
		t.Fatal("mention pushes are disabled by default")
	}
	if d.MentionPushInterval.Duration > 5*time.Minute {
		t.Errorf("first delivery attempt waits up to %v; a notification that late is not a notification",
			d.MentionPushInterval.Duration)
	}
	// Independent of the housekeeping timer, which is deliberately slow.
	if d.MentionPushInterval.Duration >= d.RoomRegistryPruneInterval.Duration {
		t.Errorf("mention push (%v) is not faster than registry prune (%v) — they were coupled once, and that was the bug",
			d.MentionPushInterval.Duration, d.RoomRegistryPruneInterval.Duration)
	}
	// Absence detection is the precondition for the whole feature.
	if d.PingInterval.Duration <= 0 {
		t.Error("keepalive off by default: the hub can never learn a peer left, so nobody is ever 'absent'")
	}
	if d.PingTimeout.Duration <= 0 {
		t.Error("no ping timeout by default: a dead link is never torn down and the peer is listed present forever")
	}
	if d.PingTimeout.Duration <= d.PingInterval.Duration {
		t.Errorf("ping_timeout (%v) must exceed ping_interval (%v) or a live client is reaped between pings",
			d.PingTimeout.Duration, d.PingInterval.Duration)
	}
}

// The push loop only starts when there is something for it to do.
func TestTheMentionLoopIsNotStartedWhenTheFeatureIsOff(t *testing.T) {
	for _, tc := range []struct {
		name string
		tune func(*config.HubConfig)
	}{
		{"notifications off", func(c *config.HubConfig) { c.MentionNotify = false }},
		{"interval zeroed", func(c *config.HubConfig) { c.MentionPushInterval = config.Duration{} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.HubConfig{
				MentionNotify:       true,
				MentionPushInterval: config.Duration{Duration: time.Minute},
			}
			tc.tune(&cfg)
			// A zero-duration ticker panics; starting must simply not
			// happen rather than crash the hub on boot.
			h := quietHubCfg(cfg)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			h.Start(ctx)
		})
	}
}
