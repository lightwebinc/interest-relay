// Command interest-announce announces interest in overlay topics from the
// command line and reports what each upstream answers. It is an announcer
// in the smallest form: a way to try a relay or a delivery edge by hand,
// and a reference for embedding the client in a program.
//
//	interest-announce -upstream [2001:db8::1]:8729 tm_example tm_other
//
// It keeps the interest alive until interrupted.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/lightwebinc/shard-common/interest"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "interest-announce:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		upstreams = flag.String("upstream", "", "comma-separated upstream addresses, host:port (required)")
		bind      = flag.String("bind", "[::]:0", "local address and port to announce from")
		label     = flag.String("sender-label", "interest-announce", "label that distinguishes this sender from others on the host")
		lifetime  = flag.Duration("lifetime", 0, "lifetime to ask for; 0 asks for the upstream's default")
		ids       = flag.Bool("ids", false, "arguments are 64-hex TopicIDs instead of topic names")
		generate  = flag.Int("generate", 0, "also announce this many generated topics, named <prefix>-0 ... <prefix>-(n-1)")
		prefix    = flag.String("generate-prefix", "tm_generated", "name prefix for -generate")
		duration  = flag.Duration("for", 0, "stop after this long; 0 runs until interrupted")
		withdraw  = flag.Bool("withdraw-on-exit", true, "withdraw everything on exit")
		quiet     = flag.Bool("quiet", false, "print only the summary lines")
		report    = flag.Duration("report", 5*time.Second, "how often to print the summary lines")
		deliverTo = flag.String("deliver-to", "", "host:port to have the topics delivered to, as a stream of delivery records: a delivery address with every topic. A distributing relay sends them there; an edge answers address_not_honored and delivers as provisioned")
	)
	flag.Usage = func() {
		_, _ = fmt.Fprintf(flag.CommandLine.Output(), "usage: interest-announce -upstream host:port[,host:port] [flags] topic...\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	bindAddr, err := netip.ParseAddrPort(*bind)
	if err != nil {
		return fmt.Errorf("-bind %q: %w", *bind, err)
	}
	// Resolve each upstream in the family the bind address sends from. A
	// name with both kinds of address would otherwise resolve to IPv4,
	// which a socket bound to an IPv6 address cannot reach.
	network := "udp"
	switch ba := bindAddr.Addr().Unmap(); {
	case ba.Is4():
		network = "udp4"
	case !ba.IsUnspecified():
		network = "udp6"
	}
	var ups []netip.AddrPort
	for _, f := range strings.Split(*upstreams, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		ua, err := net.ResolveUDPAddr(network, f)
		if err != nil {
			return fmt.Errorf("-upstream %q, reached from -bind %s: %w", f, *bind, err)
		}
		ups = append(ups, ua.AddrPort())
	}
	if *lifetime < 0 || *lifetime%time.Second != 0 || *lifetime > time.Duration(interest.LifetimeDefault-1)*time.Second {
		return fmt.Errorf("-lifetime %s: whole seconds, at most %d, or 0 for the upstream's default", *lifetime, interest.LifetimeDefault-1)
	}
	if len(ups) == 0 {
		flag.Usage()
		return fmt.Errorf("-upstream is required")
	}

	names := map[interest.TopicID]string{}
	var topics []interest.TopicID
	add := func(id interest.TopicID, name string) {
		if _, dup := names[id]; !dup {
			names[id] = name
			topics = append(topics, id)
		}
	}
	for _, a := range flag.Args() {
		if *ids {
			b, err := hex.DecodeString(a)
			if err != nil || len(b) != 32 {
				return fmt.Errorf("%q is not a 64-hex TopicID", a)
			}
			add(interest.TopicID(b), a[:16])
		} else {
			add(interest.TopicID(sha256.Sum256([]byte(a))), a)
		}
	}
	for i := 0; i < *generate; i++ {
		name := fmt.Sprintf("%s-%d", *prefix, i)
		add(interest.TopicID(sha256.Sum256([]byte(name))), name)
	}
	if len(topics) == 0 {
		flag.Usage()
		return fmt.Errorf("no topics given")
	}

	conn, err := net.ListenUDP("udp", net.UDPAddrFromAddrPort(bindAddr))
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	var mu sync.Mutex
	status := map[interest.TopicID]interest.Status{}
	c := interest.NewClient(interest.ClientConfig{
		SenderID:  interest.NewSenderID(*label),
		Upstreams: ups,
		Lifetime:  uint32(*lifetime / time.Second),
		OnStatus: func(t interest.TopicID, st interest.Status) {
			mu.Lock()
			status[t] = st
			mu.Unlock()
			if !*quiet {
				fmt.Printf("%s  %s\n", st, names[t])
			}
		},
	})
	var ext []byte
	if *deliverTo != "" {
		ap, err := netip.ParseAddrPort(*deliverTo)
		if err != nil || ap.Port() == 0 {
			return fmt.Errorf("-deliver-to %q: an address and a port", *deliverTo)
		}
		d := interest.Delivery{Kind: interest.DeliverUnicast, Encoding: interest.EncodeRecords, Addr: ap.Addr(), Port: ap.Port()}
		v, err := d.AppendValue(nil)
		if err != nil {
			return fmt.Errorf("-deliver-to %q: %w", *deliverTo, err)
		}
		ext = interest.AppendTLV(nil, interest.ExtDelivery, v)
	}
	for _, t := range topics {
		c.Set(t, ext)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if *duration > 0 {
		var tcancel context.CancelFunc
		ctx, tcancel = context.WithTimeout(ctx, *duration)
		defer tcancel()
	}

	go func() {
		if *report <= 0 {
			return
		}
		t := time.NewTicker(*report)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				summary(c, status, &mu, len(topics))
			}
		}
	}()

	err = interest.RunClient(ctx, conn, c, interest.RunOptions{WithdrawOnExit: *withdraw})
	summary(c, status, &mu, len(topics))
	return err
}

func summary(c *interest.Client, status map[interest.TopicID]interest.Status, mu *sync.Mutex, total int) {
	counts := map[interest.Status]int{}
	mu.Lock()
	for _, st := range status {
		counts[st]++
	}
	known := len(status)
	mu.Unlock()
	if total > known {
		counts[interest.StatusPending] += total - known
	}
	var parts []string
	for st, n := range counts {
		parts = append(parts, fmt.Sprintf("%s=%d", st, n))
	}
	sort.Strings(parts)
	for _, u := range c.Upstreams() {
		scope := "unknown"
		if u.Scope != nil {
			scope = "all groups"
			if !u.Scope.All() {
				scope = fmt.Sprintf("%d of %d groups", len(u.Scope.Groups()), 1<<u.Scope.ShardBits())
			}
		}
		fmt.Printf("# upstream %s reachable=%v synced=%v held=%d scope=%s lifetime=%ds\n",
			u.Addr, u.Reachable, u.Synced, u.Held, scope, u.Lifetimes.Default)
	}
	fmt.Printf("# topics %d: %s\n", total, strings.Join(parts, " "))
}
