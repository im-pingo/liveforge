package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/im-pingo/liveforge/config"
	configruntime "github.com/im-pingo/liveforge/config/runtime"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/module/api"
	"github.com/im-pingo/liveforge/module/auth"
	"github.com/im-pingo/liveforge/module/cluster"
	dvrmod "github.com/im-pingo/liveforge/module/dvr"
	gb28181mod "github.com/im-pingo/liveforge/module/gb28181"
	"github.com/im-pingo/liveforge/module/httpstream"
	metricsmod "github.com/im-pingo/liveforge/module/metrics"
	"github.com/im-pingo/liveforge/module/notify"
	"github.com/im-pingo/liveforge/module/record"
	"github.com/im-pingo/liveforge/module/rtmp"
	"github.com/im-pingo/liveforge/module/rtsp"
	sipmod "github.com/im-pingo/liveforge/module/sip"
	sipgwmod "github.com/im-pingo/liveforge/module/sipgateway"
	srtmod "github.com/im-pingo/liveforge/module/srt"
	webrtcmod "github.com/im-pingo/liveforge/module/webrtc"
	"github.com/im-pingo/liveforge/pkg/logger"
)

var version = "dev"

func newRuntimeManager(cfg *config.Config, configPath string, server *core.Server) (*configruntime.Manager, error) {
	source, err := configruntime.BuildSource(cfg.Runtime, configPath)
	if err != nil {
		return nil, err
	}
	manager, err := configruntime.NewManager(configruntime.Options{
		Source:       source,
		PollInterval: cfg.Runtime.PollInterval,
		LoadTimeout:  cfg.Runtime.LoadTimeout,
		Initial:      cfg,
		Apply: func(snapshot *configruntime.ConfigSnapshot, changeSet configruntime.ChangeSet) error {
			if err := server.UpdateConfigSnapshot(snapshot); err != nil {
				return fmt.Errorf("apply runtime config: %w", err)
			}
			logger.Init(server.Config().Server.LogLevel)
			slog.Info("config snapshot published", "version", snapshot.Version.Value, "changes", len(changeSet.Changes), "restart_required", len(changeSet.Restart))
			return nil
		},
	})
	if err != nil {
		_ = source.Close()
		return nil, err
	}
	server.SetConfigManager(manager)
	return manager, nil
}

func initializeServerAndRuntime(ctx context.Context, server *core.Server, manager *configruntime.Manager) error {
	if err := server.Init(); err != nil {
		return fmt.Errorf("server init: %w", err)
	}
	if err := manager.Start(ctx); err != nil {
		return fmt.Errorf("start runtime config manager: %w", err)
	}
	return nil
}

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
	manager, err := newRuntimeManager(cfg, *configPath, s)
	if err != nil {
		log.Fatalf("failed to configure runtime config manager: %v", err)
	}
	if cfg.AudioCodec.Enabled {
		s.StreamHub().SetAudioCodecEnabled(true)
	}

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

	// SIP must be registered before GB28181 so its service is available.
	var sipModule *sipmod.Module
	if cfg.SIP.Enabled {
		sipModule = sipmod.NewModule()
		s.RegisterModule(sipModule)
	}

	if cfg.GB28181.Enabled {
		if sipModule == nil {
			log.Fatal("gb28181 requires sip to be enabled")
		}
		s.RegisterModule(gb28181mod.NewModule(sipModule.Service()))
	}

	if cfg.SIP.Gateway.Enabled {
		if sipModule == nil {
			log.Fatal("sip gateway requires sip to be enabled")
		}
		s.RegisterModule(sipgwmod.NewModule(sipModule.Service()))
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

	if cfg.DVR.Enabled {
		s.RegisterModule(dvrmod.NewModule())
	}

	if cfg.Metrics.Enabled {
		s.RegisterModule(metricsmod.NewModule())
	}

	if err := initializeServerAndRuntime(context.Background(), s, manager); err != nil {
		s.Shutdown()
		_ = manager.Close()
		log.Fatalf("server startup failed: %v", err)
	}

	slog.Info("server started", "version", version, "name", cfg.Server.Name)

	// Block until signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	for {
		sig := <-sigCh
		if sig == syscall.SIGHUP {
			slog.Info("received SIGHUP, scheduling config refresh", "source", manager.Status().Source)
			if err := manager.Refresh(context.Background()); err != nil {
				slog.Error("config refresh scheduling failed", "error", err)
			}
			continue
		}
		slog.Info("shutting down", "signal", sig.String())
		break
	}

	s.Shutdown()
	_ = manager.Close()
	slog.Info("server stopped")
}
