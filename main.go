// Command interest-relay is a small daemon that relays topic interest: it
// takes the interest announced by the hosts and relays below it, holds
// their union, and announces that union to its own upstreams.
//
// It does only this. It carries no objects.
package main

import (
	"context"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/lightwebinc/shard-common/interest"
	"github.com/lightwebinc/shard-common/logging"

	"github.com/lightwebinc/interest-relay/config"
	"github.com/lightwebinc/interest-relay/distributor"
	"github.com/lightwebinc/interest-relay/metrics"
	"github.com/lightwebinc/interest-relay/relay"
)

// Version is set via -ldflags at build time.
var Version = "dev"

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	metrics.Version = Version

	level := logging.ParseLevel(cfg.LogLevel)
	levelVar := logging.Init(logging.Options{
		Service:    metrics.ServiceName,
		InstanceID: cfg.InstanceID,
		Version:    Version,
		Level:      level,
		Format:     logging.ParseFormat(cfg.LogFormat),
	})
	logging.InstallSIGHUPToggle(levelVar, level)

	r, err := relay.New(relay.Config{
		Upstreams:          cfg.Upstreams,
		Allow:              cfg.Allow,
		SenderID:           interest.NewSenderID(cfg.SenderLabel),
		Lifetimes:          cfg.Lifetimes,
		UpstreamLifetime:   uint32(cfg.UpstreamLifetime / time.Second),
		MaxTopicsPerSender: cfg.MaxTopicsPerSender,
		MaxSenders:         cfg.MaxSenders,
		ChangeRate:         cfg.ChangeRate,
		ChangeBurst:        cfg.ChangeBurst,
		MaxHops:            uint8(cfg.MaxHops),
		RestartHold:        cfg.RestartHold,
		WithdrawOnExit:     cfg.WithdrawOnExit,
		Distribute:         distConfig(cfg),
		Advertise:          cfg.DistributeAdvertise,
	})
	if err != nil {
		return err
	}

	down, err := net.ListenUDP("udp", net.UDPAddrFromAddrPort(cfg.Listen))
	if err != nil {
		return err
	}
	defer func() { _ = down.Close() }()
	up, err := net.ListenUDP("udp", net.UDPAddrFromAddrPort(cfg.UpstreamBind))
	if err != nil {
		return err
	}
	defer func() { _ = up.Close() }()

	slog.Info("interest-relay starting",
		"version", Version,
		"listen", down.LocalAddr().String(),
		"announce_from", up.LocalAddr().String(),
		"upstreams", cfg.Upstreams,
		"allow", cfg.Allow,
		"lifetimes", cfg.Lifetimes,
		"metrics_addr", cfg.MetricsAddr,
	)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	var wg sync.WaitGroup
	if cfg.MetricsAddr != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := metrics.Serve(ctx, cfg.MetricsAddr, metrics.Handler(r, levelVar)); err != nil {
				slog.Error("metrics listener", "err", err)
			}
		}()
	}
	err = r.Serve(ctx, down, up)
	cancel()
	wg.Wait()
	slog.Info("shutdown complete")
	return err
}

// distConfig is the distributor's configuration, or nil for a control-only
// relay.
func distConfig(cfg *config.Config) *distributor.Config {
	if cfg.DistributeListen == "" {
		return nil
	}
	return &distributor.Config{
		Listen:     cfg.DistributeListen,
		MaxRecord:  cfg.DistributeMaxRecord,
		Queue:      cfg.DistributeQueue,
		QueueBytes: cfg.DistributeQueueBytes,
	}
}
