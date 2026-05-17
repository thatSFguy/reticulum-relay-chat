// Command rrc-hub runs a Reticulum Relay Chat hub: it announces an
// rrc.hub destination on the attached Reticulum network and relays
// IRC-style room chat between connected clients.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/thatSFguy/reticulum-relay-chat/internal/config"
	"github.com/thatSFguy/reticulum-relay-chat/internal/service"
)

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "0.1.0-dev"

func main() {
	configPath := flag.String("config", "rrc-hub.toml", "path to the TOML config file")
	showVersion := flag.Bool("version", false, "print version and exit")
	// Optional overrides. The config file remains the primary mechanism;
	// these only take effect when explicitly passed on the command line.
	identityPath := flag.String("identity", "", "override hub.identity_path")
	hubName := flag.String("hub-name", "", "override hub.name")
	noAnnounce := flag.Bool("no-announce", false, "force hub.announce_on_start = false")
	flag.Parse()

	if *showVersion {
		fmt.Println("rrc-hub", version)
		return
	}

	logger := log.New(os.Stderr, "", log.LstdFlags|log.Lmicroseconds)

	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Fatalf("%v", err)
	}

	// Apply CLI overrides only for flags the operator actually set, so an
	// unset flag never clobbers a configured value.
	flag.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "identity":
			cfg.Hub.IdentityPath = *identityPath
		case "hub-name":
			cfg.Hub.Name = *hubName
		case "no-announce":
			if *noAnnounce {
				cfg.Hub.AnnounceOnStart = false
			}
		}
	})

	svc, err := service.New(cfg, logger)
	if err != nil {
		logger.Fatalf("%v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := svc.Run(ctx); err != nil {
		logger.Fatalf("%v", err)
	}
	logger.Printf("rrc-hub stopped cleanly")
}
