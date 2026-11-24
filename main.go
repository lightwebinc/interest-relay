// Command interest-relay is a small daemon that relays topic interest: it
// takes the interest announced by the hosts and relays below it, holds
// their union, and announces that union to its own upstreams.
//
// That is all it does unless told more: with -distribute-listen it ends the
// consumer's lane and sends each record on to the hosts that asked for its
// topics, and with -publish-listen as well it is the domain's publish point,
// taking the hosts' submissions up the tunnel and copying each at once to
// their siblings.
package main

import (
	"context"
	"fmt"
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
	"github.com/lightwebinc/interest-relay/publish"
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

	var pub *publish.Server
	var pubLn net.Listener
	if cfg.PublishListen != "" {
		if pub, err = publish.New(publish.Config{
			Allow:         cfg.Allow,
			Ingress:       cfg.PublishIngress,
			Source:        cfg.PublishSource,
			MaxObject:     cfg.PublishMaxObject,
			DeliverTopics: cfg.DeliverTopics,
			SourceObjects: cfg.PublishSourceObjects,
			SourceBytes:   cfg.PublishSourceBytes,
			TotalObjects:  cfg.PublishObjects,
			TotalBytes:    cfg.PublishBytes,
			Queue:         cfg.PublishQueue,
			QueueBytes:    cfg.PublishQueueBytes,
		}, r.Distributor()); err != nil {
			return err
		}
		if pubLn, err = net.Listen("tcp", cfg.PublishListen); err != nil {
			return err
		}
		defer func() { _ = pubLn.Close() }() // ServeListener closes it too; this covers a start that fails first
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
		"distribute", cfg.DistributeListen,
		"publish", cfg.PublishListen,
		"publish_source", cfg.PublishSource,
		"publish_ingress", cfg.PublishIngress,
	)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	var wg sync.WaitGroup
	if cfg.MetricsAddr != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := metrics.Serve(ctx, cfg.MetricsAddr, metrics.HandlerWith(r, pub, levelVar)); err != nil {
				slog.Error("metrics listener", "err", err)
			}
		}()
	}
	var pubErr error
	if pub != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := pub.ServeListener(ctx, pubLn); err != nil && ctx.Err() == nil {
				pubErr = fmt.Errorf("publish point: %w", err)
				cancel() // a relay that cannot publish for its hosts stops, rather than drop their submissions quietly
			}
		}()
	}
	err = r.Serve(ctx, down, up)
	cancel()
	wg.Wait()
	if err == nil {
		err = pubErr // a non-zero exit, so a supervisor restarts it
	}
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
		Listen:        cfg.DistributeListen,
		MaxRecord:     cfg.DistributeMaxRecord,
		Queue:         cfg.DistributeQueue,
		QueueBytes:    cfg.DistributeQueueBytes,
		DeliverTopics: cfg.DeliverTopics,
	}
}
