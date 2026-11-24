// Package config loads and validates the runtime configuration of the
// interest-relay daemon. Parameters are taken from command-line flags
// first; environment variables serve as fallbacks; built-in defaults apply
// when neither is present.
package config

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/lightwebinc/interest-relay/distributor"
	"github.com/lightwebinc/interest-relay/publish"
	"github.com/lightwebinc/shard-common/interest"
	"github.com/lightwebinc/shard-common/objfmt"
)

// Config holds every runtime parameter of the daemon.
type Config struct {
	// Listen is where senders announce to.
	Listen netip.AddrPort
	// UpstreamBind is the local address the relay announces from. The
	// unspecified address lets the kernel choose per upstream.
	UpstreamBind netip.AddrPort
	// Upstreams are where the union is announced.
	Upstreams []netip.AddrPort
	// Allow lists the prefixes senders may announce from.
	Allow []netip.Prefix

	// SenderLabel distinguishes this relay from any other sender on the
	// same host. The identity sent upstream is derived from the host name
	// and this label, so it is the same after a restart.
	SenderLabel string

	// Lifetimes are enforced on the senders below.
	Lifetimes interest.Lifetimes
	// UpstreamLifetime is asked of the upstreams; zero asks for theirs.
	UpstreamLifetime time.Duration

	MaxTopicsPerSender int
	MaxSenders         int
	ChangeRate         float64
	ChangeBurst        int
	MaxHops            int

	// RestartHold: see relay.Config. Negative means automatic.
	RestartHold    time.Duration
	WithdrawOnExit bool

	// DistributeListen turns on the unicast distributor: the TCP address
	// the consumer's lane is delivered to. Empty is control only.
	DistributeListen string
	// DistributeAdvertise is announced upward as this relay's delivery
	// address in distributor mode: where DistributeListen is reached.
	DistributeAdvertise netip.AddrPort
	// DistributeQueue and DistributeQueueBytes bound what waits for one
	// host; DistributeMaxRecord bounds one record.
	DistributeQueue      int
	DistributeQueueBytes int64
	DistributeMaxRecord  int
	// DeliverTopics is how many of a record's leading topic names the edge
	// delivers it on: what the distributor routes by and the publish point
	// copies by.
	DeliverTopics int

	// PublishListen turns on the publish point (item L2): the TCP address
	// the hosts' bridges publish to (their -edge-ingress). PublishIngress
	// are the edge's ingress addresses in failover order, and PublishSource
	// the consumer's own address, the tunnel's inner address.
	PublishListen  string
	PublishIngress []string
	PublishSource  netip.Addr
	// PublishMaxObject bounds one object. The rates are a host's budget and
	// the relay's, per second; zero is no limit.
	PublishMaxObject                         int
	PublishSourceObjects, PublishSourceBytes float64
	PublishObjects, PublishBytes             float64
	PublishQueue                             int
	PublishQueueBytes                        int64

	MetricsAddr string
	InstanceID  string
	LogLevel    string
	LogFormat   string
}

// Load parses flags and the environment into a Config and validates it.
func Load() (*Config, error) {
	return parse(flag.CommandLine, os.Args[1:], os.Getenv)
}

func parse(fs *flag.FlagSet, args []string, getenv func(string) string) (*Config, error) {
	host, _ := os.Hostname()
	env := func(key, def string) string {
		if v := getenv(key); v != "" {
			return v
		}
		return def
	}
	var (
		listen       = fs.String("listen", env("LISTEN", fmt.Sprintf("[::]:%d", interest.DefaultPort)), "address and port senders announce to")
		upstreamBind = fs.String("upstream-bind", env("UPSTREAM_BIND", "[::]:0"), "local address and port the relay announces from; port 0 lets the kernel choose")
		upstreams    = fs.String("upstream", env("UPSTREAM", ""), "comma-separated upstream addresses, host:port, in the manner of default routes (required)")
		allow        = fs.String("allow", env("ALLOW", ""), "comma-separated prefixes senders may announce from (required)")
		label        = fs.String("sender-label", env("SENDER_LABEL", "interest-relay"), "label that distinguishes this relay from other senders on the host")
		shortest     = fs.Duration("lifetime-min", envDuration(getenv, "LIFETIME_MIN", 30*time.Second), "shortest lifetime granted to a sender")
		standard     = fs.Duration("lifetime-default", envDuration(getenv, "LIFETIME_DEFAULT", 300*time.Second), "lifetime granted when a sender asks for the default")
		longest      = fs.Duration("lifetime-max", envDuration(getenv, "LIFETIME_MAX", 3600*time.Second), "longest lifetime granted to a sender")
		upLife       = fs.Duration("upstream-lifetime", envDuration(getenv, "UPSTREAM_LIFETIME", 0), "lifetime asked of the upstreams; 0 asks for each upstream's default")
		maxTopics    = fs.Int("max-topics-per-sender", envInt(getenv, "MAX_TOPICS_PER_SENDER", 0), "cap on topics held for one sender; 0 is no cap")
		maxSenders   = fs.Int("max-senders", envInt(getenv, "MAX_SENDERS", 0), "cap on senders tracked; 0 is no cap")
		changeRate   = fs.Float64("change-rate", envFloat(getenv, "CHANGE_RATE", 0), "new topics a sender may add per second; 0 is no limit")
		changeBurst  = fs.Int("change-burst", envInt(getenv, "CHANGE_BURST", 0), "burst allowed above -change-rate")
		maxHops      = fs.Int("max-hops", envInt(getenv, "MAX_HOPS", interest.DefaultMaxHops), "relays a message may have crossed before it is refused")
		hold         = fs.Duration("restart-hold", envDuration(getenv, "RESTART_HOLD", -1), "after a start, how long the upstreams keep what the previous run announced; negative derives it from -lifetime-default")
		withdraw     = fs.Bool("withdraw-on-exit", envBool(getenv, "WITHDRAW_ON_EXIT", false), "withdraw everything from the upstreams on exit; leave off for a relay that restarts")
		distListen   = fs.String("distribute-listen", env("DISTRIBUTE_LISTEN", ""), "TCP address the consumer's lane is delivered to; turns on the unicast distributor, which sends each record on to the senders below that announced its topic with a delivery address. Empty is control only")
		distAdv      = fs.String("distribute-advertise", env("DISTRIBUTE_ADVERTISE", ""), "host:port announced upward as this relay's delivery address in distributor mode: where -distribute-listen is reached")
		distQueue    = fs.Int("distribute-queue", envInt(getenv, "DISTRIBUTE_QUEUE", distributor.DefaultQueue), "records that may wait for one host before it is shed")
		distQueueMiB = fs.Int("distribute-queue-mib", envInt(getenv, "DISTRIBUTE_QUEUE_MIB", distributor.DefaultQueueBytes>>20), "MiB that may wait for one host before it is shed")
		distMaxMiB   = fs.Int("distribute-max-object-mib", envInt(getenv, "DISTRIBUTE_MAX_OBJECT_MIB", objfmt.DefaultMaxObject>>20), "largest object on the lane, in MiB; a longer record closes the lane")
		deliver      = fs.Int("deliver-topics", envInt(getenv, "DELIVER_TOPICS", distributor.DefaultDeliverTopics), "how many of a record's leading topic names the edge delivers it on (its -beef-max-topics for a consumer tunnel): the distributor routes, and the publish point copies, by these")
		pubListen    = fs.String("publish-listen", env("PUBLISH_LISTEN", ""), "TCP address the hosts' bridges publish to (their -edge-ingress); turns on the publish point: each submission goes up the tunnel from -publish-source and is copied at once to the sibling hosts that asked for its topics. Empty is off")
		pubIngress   = fs.String("publish-ingress", env("PUBLISH_INGRESS", ""), "the edge's ingress addresses, host:port, comma list in failover order (side A then side B)")
		pubSource    = fs.String("publish-source", env("PUBLISH_SOURCE", ""), "the consumer's own address (the tunnel's inner address), which the edge excludes from the consumer's lane; publishing from any other would send every host its own objects back")
		pubMaxMiB    = fs.Int("publish-max-object-mib", envInt(getenv, "PUBLISH_MAX_OBJECT_MIB", publish.DefaultMaxObject>>20), "largest object taken, in MiB (the edge's bound for a consumer tunnel)")
		pubSrcObjs   = fs.Float64("publish-source-objects-per-sec", envFloat(getenv, "PUBLISH_SOURCE_OBJECTS_PER_SEC", publish.DefaultObjectsPerSec), "one host's budget, objects per second; 0 is no limit")
		pubSrcMiB    = fs.Float64("publish-source-mib-per-sec", envFloat(getenv, "PUBLISH_SOURCE_MIB_PER_SEC", publish.DefaultBytesPerSec>>20), "one host's budget, MiB per second; 0 is no limit")
		pubObjs      = fs.Float64("publish-objects-per-sec", envFloat(getenv, "PUBLISH_OBJECTS_PER_SEC", publish.DefaultObjectsPerSec), "the relay's budget, objects per second: the edge's for one source; 0 is no limit")
		pubMiB       = fs.Float64("publish-mib-per-sec", envFloat(getenv, "PUBLISH_MIB_PER_SEC", publish.DefaultBytesPerSec>>20), "the relay's budget, MiB per second; 0 is no limit")
		pubQueue     = fs.Int("publish-queue", envInt(getenv, "PUBLISH_QUEUE", publish.DefaultQueue), "records that may wait to go up the tunnel; a record that finds it full is refused and not copied")
		pubQueueMiB  = fs.Int("publish-queue-mib", envInt(getenv, "PUBLISH_QUEUE_MIB", publish.DefaultQueueBytes>>20), "MiB that may wait to go up the tunnel")
		metricsAddr  = fs.String("metrics-addr", env("METRICS_ADDR", ":9600"), "address of the metrics and health listener; empty disables it")
		instance     = fs.String("instance-id", env("INSTANCE_ID", host), "service.instance.id for logs and metrics")
		logLevel     = fs.String("log-level", env("LOG_LEVEL", "info"), "debug, info, warn or error")
		logFormat    = fs.String("log-format", env("LOG_FORMAT", "text"), "text (stderr) or json (stdout)")
	)
	if err := fs.Parse(args); err != nil {
		return nil, err
	}

	cfg := &Config{
		SenderLabel:        *label,
		UpstreamLifetime:   *upLife,
		MaxTopicsPerSender: *maxTopics,
		MaxSenders:         *maxSenders,
		ChangeRate:         *changeRate,
		ChangeBurst:        *changeBurst,
		MaxHops:            *maxHops,
		RestartHold:        *hold,
		WithdrawOnExit:     *withdraw,
		MetricsAddr:        *metricsAddr,
		InstanceID:         *instance,
		LogLevel:           *logLevel,
		LogFormat:          *logFormat,
	}
	var err error
	if cfg.Listen, err = netip.ParseAddrPort(*listen); err != nil {
		return nil, fmt.Errorf("-listen %q: %w", *listen, err)
	}
	if cfg.Listen.Port() == 0 {
		return nil, fmt.Errorf("-listen %q: senders need a fixed port to announce to", *listen)
	}
	if cfg.UpstreamBind, err = netip.ParseAddrPort(*upstreamBind); err != nil {
		return nil, fmt.Errorf("-upstream-bind %q: %w", *upstreamBind, err)
	}
	if cfg.Upstreams, err = resolveUpstreams(*upstreams); err != nil {
		return nil, err
	}
	if cfg.Allow, err = parsePrefixes(*allow); err != nil {
		return nil, err
	}
	if cfg.SenderLabel == "" {
		return nil, errors.New("-sender-label must not be empty")
	}
	for name, d := range map[string]time.Duration{"-lifetime-min": *shortest, "-lifetime-default": *standard, "-lifetime-max": *longest} {
		if d < time.Second || d%time.Second != 0 {
			return nil, fmt.Errorf("%s %s: lifetimes are whole seconds, at least one", name, d)
		}
	}
	cfg.Lifetimes = interest.Lifetimes{
		Min:     uint32(*shortest / time.Second),
		Default: uint32(*standard / time.Second),
		Max:     uint32(*longest / time.Second),
	}
	if cfg.Lifetimes.Min > cfg.Lifetimes.Default || cfg.Lifetimes.Default > cfg.Lifetimes.Max {
		return nil, fmt.Errorf("lifetimes must satisfy min <= default <= max, got %s, %s, %s", *shortest, *standard, *longest)
	}
	if cfg.UpstreamLifetime < 0 || cfg.UpstreamLifetime%time.Second != 0 {
		return nil, fmt.Errorf("-upstream-lifetime %s: whole seconds, or 0 for the upstream's default", cfg.UpstreamLifetime)
	}
	if cfg.MaxHops < 1 || cfg.MaxHops > 255 {
		return nil, fmt.Errorf("-max-hops %d: between 1 and 255", cfg.MaxHops)
	}
	if cfg.MaxTopicsPerSender < 0 || cfg.MaxSenders < 0 || cfg.ChangeRate < 0 || cfg.ChangeBurst < 0 {
		return nil, errors.New("caps and rates must not be negative")
	}
	if *distListen != "" {
		cfg.DistributeListen = *distListen
		if cfg.DistributeAdvertise, err = netip.ParseAddrPort(*distAdv); err != nil || cfg.DistributeAdvertise.Port() == 0 ||
			cfg.DistributeAdvertise.Addr().IsUnspecified() {
			return nil, fmt.Errorf("-distribute-advertise %q: the address and port the upstream reaches -distribute-listen at", *distAdv)
		}
		if *distQueue < 1 || *distQueueMiB < 1 || *distMaxMiB < 1 {
			return nil, errors.New("-distribute-queue, -distribute-queue-mib and -distribute-max-object-mib must be at least 1")
		}
		cfg.DistributeQueue = *distQueue
		cfg.DistributeQueueBytes = int64(*distQueueMiB) << 20
		cfg.DistributeMaxRecord = objfmt.BEEFDeliveryHeaderSize + *distMaxMiB<<20
	} else if *distAdv != "" {
		return nil, errors.New("-distribute-advertise is set without -distribute-listen")
	}
	if *deliver < 1 || *deliver > objfmt.BEEFMaxTopics {
		return nil, fmt.Errorf("-deliver-topics %d: between 1 and %d", *deliver, objfmt.BEEFMaxTopics)
	}
	cfg.DeliverTopics = *deliver
	if *pubListen != "" {
		cfg.PublishListen = *pubListen
		for _, f := range strings.Split(*pubIngress, ",") {
			if f = strings.TrimSpace(f); f == "" {
				continue
			}
			if _, _, err := net.SplitHostPort(f); err != nil {
				return nil, fmt.Errorf("-publish-ingress %q: %w", f, err)
			}
			cfg.PublishIngress = append(cfg.PublishIngress, f)
		}
		if len(cfg.PublishIngress) == 0 {
			return nil, errors.New("-publish-listen needs -publish-ingress, the edge's ingress addresses")
		}
		if cfg.PublishSource, err = netip.ParseAddr(*pubSource); err != nil || cfg.PublishSource.IsUnspecified() {
			return nil, fmt.Errorf("-publish-source %q: the consumer's own address, which the edge excludes from its lane", *pubSource)
		}
		if *pubMaxMiB < 1 || *pubQueue < 1 || *pubQueueMiB < 1 || *pubSrcObjs < 0 || *pubSrcMiB < 0 || *pubObjs < 0 || *pubMiB < 0 {
			return nil, errors.New("-publish-max-object-mib, -publish-queue and -publish-queue-mib must be at least 1, and the budgets not negative")
		}
		if cfg.DistributeListen == "" {
			return nil, errors.New("-publish-listen needs -distribute-listen: the publish point copies to the hosts the distributor serves")
		}
		cfg.PublishMaxObject = *pubMaxMiB << 20
		cfg.PublishSourceObjects, cfg.PublishSourceBytes = *pubSrcObjs, *pubSrcMiB*(1<<20)
		cfg.PublishObjects, cfg.PublishBytes = *pubObjs, *pubMiB*(1<<20)
		cfg.PublishQueue, cfg.PublishQueueBytes = *pubQueue, int64(*pubQueueMiB)<<20
	} else if *pubIngress != "" || *pubSource != "" {
		return nil, errors.New("-publish-ingress and -publish-source are set without -publish-listen")
	}
	return cfg, nil
}

// resolveUpstreams turns "host:port,host:port" into addresses. Names are
// resolved once, here: an upstream whose address changes needs a restart.
func resolveUpstreams(s string) ([]netip.AddrPort, error) {
	var out []netip.AddrPort
	seen := map[netip.AddrPort]bool{}
	for _, f := range strings.Split(s, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		ap, err := netip.ParseAddrPort(f)
		if err != nil {
			ua, rerr := net.ResolveUDPAddr("udp", f)
			if rerr != nil {
				return nil, fmt.Errorf("-upstream %q: %w", f, rerr)
			}
			ap = ua.AddrPort()
		}
		ap = netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())
		if ap.Port() == 0 || ap.Addr().IsUnspecified() || ap.Addr().IsMulticast() {
			return nil, fmt.Errorf("-upstream %q: need a unicast address and a port", f)
		}
		if !seen[ap] {
			seen[ap] = true
			out = append(out, ap)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("-upstream is required: where the relay announces to")
	}
	return out, nil
}

func parsePrefixes(s string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, f := range strings.Split(s, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		p, err := netip.ParsePrefix(f)
		if err != nil {
			// A bare address is a prefix of one.
			a, aerr := netip.ParseAddr(f)
			if aerr != nil {
				return nil, fmt.Errorf("-allow %q: %w", f, err)
			}
			p = netip.PrefixFrom(a, a.BitLen())
		}
		out = append(out, p.Masked())
	}
	if len(out) == 0 {
		return nil, errors.New("-allow is required: which prefixes may announce")
	}
	return out, nil
}

func envInt(getenv func(string) string, key string, def int) int {
	if v, err := strconv.Atoi(getenv(key)); err == nil {
		return v
	}
	return def
}

func envFloat(getenv func(string) string, key string, def float64) float64 {
	if v, err := strconv.ParseFloat(getenv(key), 64); err == nil {
		return v
	}
	return def
}

func envBool(getenv func(string) string, key string, def bool) bool {
	if v, err := strconv.ParseBool(getenv(key)); err == nil {
		return v
	}
	return def
}

func envDuration(getenv func(string) string, key string, def time.Duration) time.Duration {
	if v, err := time.ParseDuration(getenv(key)); err == nil {
		return v
	}
	return def
}
