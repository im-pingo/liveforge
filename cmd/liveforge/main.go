package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/module/httpstream"
	"github.com/im-pingo/liveforge/module/rtmp"
)

var version = "dev"

func main() {
	configPath := flag.String("c", "configs/streamserver.yaml", "config file path")
	showVersion := flag.Bool("v", false, "show version")
	flag.Parse()

	if *showVersion {
		fmt.Printf("liveforge %s\n", version)
		os.Exit(0)
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	s := core.NewServer(cfg)

	if cfg.RTMP.Enabled {
		s.RegisterModule(rtmp.NewModule())
	}

	if cfg.HTTP.Enabled {
		s.RegisterModule(httpstream.NewModule())
	}

	if err := s.Init(); err != nil {
		log.Fatalf("server init failed: %v", err)
	}

	log.Printf("liveforge %s started, server name: %s", version, cfg.Server.Name)

	// Block until signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh
	log.Printf("received signal %v, shutting down...", sig)

	s.Shutdown()
	log.Println("liveforge stopped")
}
