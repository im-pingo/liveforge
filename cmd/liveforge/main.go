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
	"time"

	"github.com/im-pingo/liveforge/config"
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

const configPollInterval = 5 * time.Second

type runtimeConfigManager interface {
	Refresh(context.Context) (config.ApplyResult, error)
	Run(context.Context, func(error))
}

func initializeConfigManager(ctx context.Context, path string, pollIntervals ...time.Duration) (*config.Manager, *config.Config, error) {
	pollInterval := configPollInterval
	if len(pollIntervals) > 0 {
		pollInterval = pollIntervals[0]
	}
	source := config.NewFileSource(path, config.RuntimeOverridePath(path))
	manager := config.NewManager(source, pollInterval, nil)
	if _, err := manager.Refresh(ctx); err != nil {
		return nil, nil, err
	}
	cfg := manager.Current().Effective
	if cfg == nil {
		return nil, nil, fmt.Errorf("configuration source returned an empty snapshot")
	}
	return manager, cfg, nil
}

func waitForShutdown(ctx context.Context, manager runtimeConfigManager, sigCh <-chan os.Signal) os.Signal {
	runCtx, cancel := context.WithCancel(ctx)
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		manager.Run(runCtx, func(err error) {
			slog.Error("periodic config refresh failed", "error", err)
		})
	}()
	defer func() {
		cancel()
		<-runDone
	}()

	for {
		select {
		case <-ctx.Done():
			return nil
		case sig, ok := <-sigCh:
			if !ok {
				return nil
			}
			if sig == syscall.SIGHUP {
				slog.Info("received SIGHUP, refreshing config")
				result, err := manager.Refresh(runCtx)
				if err != nil {
					slog.Error("config refresh failed", "error", err)
					continue
				}
				slog.Info("config refresh completed", "changed", result.Changed, "revision", result.Revision, "pending_restart", result.PendingRestart)
				continue
			}
			return sig
		}
	}
}

func main() {
	configPath := flag.String("c", "configs/liveforge.yaml", "config file path")
	configPoll := flag.Duration("config-poll", configPollInterval, "configuration source polling interval")
	showVersion := flag.Bool("v", false, "show version")
	flag.Parse()

	if *showVersion {
		fmt.Printf("liveforge %s\n", version)
		os.Exit(0)
	}

	manager, cfg, err := initializeConfigManager(context.Background(), *configPath, *configPoll)
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	logger.Init(cfg.Server.LogLevel)

	s := core.NewServer(cfg)
	s.SetConfigUpdater(manager)

	if cfg.AudioCodec.Enabled {
		s.StreamHub().SetAudioCodecEnabled(true)
	}

	// The unified authorizer is always installed so auth can be enabled at runtime.
	s.RegisterModule(auth.NewModule())

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

	if err := s.Init(); err != nil {
		log.Fatalf("server init failed: %v", err)
	}
	manager.SetApply(func(_ context.Context, _, next *config.Config, _ config.ChangeSet) error {
		if err := s.ApplyConfig(next); err != nil {
			return err
		}
		logger.Init(next.Server.LogLevel)
		return nil
	})

	slog.Info("server started", "version", version, "name", cfg.Server.Name)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	sig := waitForShutdown(context.Background(), manager, sigCh)
	signal.Stop(sigCh)
	if sig != nil {
		slog.Info("shutting down", "signal", sig.String())
	}

	s.Shutdown()
	slog.Info("server stopped")
}
