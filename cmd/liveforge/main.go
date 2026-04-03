package main

import (
	"flag"
	"fmt"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/logger"
	"github.com/im-pingo/liveforge/module/api"
	"github.com/im-pingo/liveforge/module/auth"
	"github.com/im-pingo/liveforge/module/httpstream"
	"github.com/im-pingo/liveforge/module/notify"
	"github.com/im-pingo/liveforge/module/record"
	"github.com/im-pingo/liveforge/module/rtmp"
	"github.com/im-pingo/liveforge/module/rtsp"
	"github.com/im-pingo/liveforge/module/cluster"
	metricsmod "github.com/im-pingo/liveforge/module/metrics"
	srtmod "github.com/im-pingo/liveforge/module/srt"
	webrtcmod "github.com/im-pingo/liveforge/module/webrtc"
)

var version = "dev"

func main() {
	configPath := flag.String("c", "configs/liveforge.yaml", "config file path")
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

	logger.Init(cfg.Server.LogLevel)

	s := core.NewServer(cfg)

	// Auth module must be registered before protocol modules
	// so its hooks are in place when connections arrive.
	if cfg.Auth.Enabled {
		s.RegisterModule(auth.NewModule())
	}

	if cfg.RTMP.Enabled {
		s.RegisterModule(rtmp.NewModule())
	}

	if cfg.RTSP.Enabled {
		s.RegisterModule(rtsp.NewModule())
	}

	if cfg.HTTP.Enabled {
		s.RegisterModule(httpstream.NewModule())
	}

	if cfg.SRT.Enabled {
		s.RegisterModule(srtmod.NewModule())
	}

	if cfg.WebRTC.Enabled {
		s.RegisterModule(webrtcmod.NewModule())
	}

	// Notify must be registered before API so its WebSocket handler
	// is available when the API module registers routes.
	if cfg.Notify.HTTP.Enabled || cfg.Notify.WebSocket.Enabled {
		s.RegisterModule(notify.NewModule())
	}

	// Cluster must be registered before API so its signaling handlers
	// are available when the API module snapshots routes.
	if cfg.Cluster.Forward.Enabled || cfg.Cluster.Origin.Enabled {
		s.RegisterModule(cluster.NewModule())
	}

	if cfg.API.Enabled {
		s.RegisterModule(api.NewModule())
	}

	if cfg.Record.Enabled {
		s.RegisterModule(record.NewModule())
	}

	if cfg.Metrics.Enabled {
		s.RegisterModule(metricsmod.NewModule())
	}

	if err := s.Init(); err != nil {
		log.Fatalf("server init failed: %v", err)
	}

	slog.Info("server started", "version", version, "name", cfg.Server.Name)

	// Block until signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh
	slog.Info("shutting down", "signal", sig.String())

	s.Shutdown()
	slog.Info("server stopped")
}
