package relay

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/lightwebinc/shard-common/interest"
	"github.com/lightwebinc/shard-common/shard"
)

// The tests run a small network on one host. Every party gets its own
// address in 127.0.0.0/8, which Linux treats as local without any
// configuration, so that source addresses tell the parties apart exactly
// as they would on separate machines.

func topic(name string) interest.TopicID { return interest.TopicID(sha256.Sum256([]byte(name))) }

func topics(prefix string, n int) []interest.TopicID {
	out := make([]interest.TopicID, n)
	for i := range out {
		out[i] = topic(fmt.Sprintf("%s-%d", prefix, i))
	}
	return out
}

func listen(t *testing.T, ip string) (*net.UDPConn, netip.AddrPort) {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP(ip)})
	if err != nil {
		t.Skipf("cannot bind %s: %v", ip, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn, conn.LocalAddr().(*net.UDPAddr).AddrPort()
}

func eventually(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// shortLives make soft state turn over in seconds instead of minutes.
var shortLives = interest.Lifetimes{Min: 1, Default: 3, Max: 10}

// openPolicy is a terminus that takes anything from anyone, with an
// optional scope.
type openPolicy struct {
	mu    sync.Mutex
	scope *interest.Scope
}

func (p *openPolicy) Identify(netip.AddrPort) (string, bool) { return "edge", true }
func (p *openPolicy) Scope(string) *interest.Scope {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.scope
}
func (p *openPolicy) Bound(string, interest.TopicID) interest.Bound { return interest.BoundFree }
func (p *openPolicy) Quota(string) int                              { return -1 }

// terminus is a delivery edge stand-in on its own socket.
type terminus struct {
	t      *testing.T
	addr   netip.AddrPort
	conn   *net.UDPConn
	pol    *openPolicy
	mu     sync.Mutex
	rcv    *interest.Receiver
	gone   map[interest.TopicID]int // topics reported no longer live, and how often
	cancel context.CancelFunc
	done   chan struct{}
}

func newTerminus(t *testing.T, ip string) *terminus {
	conn, addr := listen(t, ip)
	tm := &terminus{t: t, addr: addr, conn: conn, pol: &openPolicy{}, gone: map[interest.TopicID]int{}}
	tm.start()
	t.Cleanup(tm.stop)
	return tm
}

func (tm *terminus) start() {
	rcv := interest.NewReceiver(interest.ReceiverConfig{Policy: tm.pol, Lifetimes: shortLives, OnChange: func(_ string, x interest.TopicID, live bool) {
		if !live {
			tm.mu.Lock()
			tm.gone[x]++
			tm.mu.Unlock()
		}
	}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	tm.mu.Lock()
	tm.rcv, tm.cancel, tm.done = rcv, cancel, done
	tm.mu.Unlock()
	go func() {
		defer close(done)
		if err := interest.ServeReceiver(ctx, tm.conn, rcv, interest.ServeOptions{Tick: 50 * time.Millisecond}); err != nil {
			tm.t.Errorf("terminus: %v", err)
		}
	}()
}

func (tm *terminus) stop() {
	tm.mu.Lock()
	cancel, done := tm.cancel, tm.done
	tm.cancel = nil
	tm.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}

// restart replaces the receiver with a new one that holds nothing, on the
// same socket: what an edge restart looks like from below.
func (tm *terminus) restart() {
	tm.stop()
	tm.start()
}

func (tm *terminus) receiver() *interest.Receiver {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	return tm.rcv
}

func (tm *terminus) live() int { return len(tm.receiver().Live("edge")) }

func (tm *terminus) dropped(x interest.TopicID) int {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	return tm.gone[x]
}

// node is a running relay.
type node struct {
	r      *Relay
	addr   netip.AddrPort
	cancel context.CancelFunc
	done   chan struct{}
}

func startRelay(t *testing.T, ip string, cfg Config) *node {
	return startRelayOn(t, nil, ip, cfg)
}

// startRelayOn starts a relay on an existing downstream socket, as a
// restarted relay comes back on its configured port.
func startRelayOn(t *testing.T, down *net.UDPConn, ip string, cfg Config) *node {
	t.Helper()
	var addr netip.AddrPort
	if down == nil {
		down, addr = listen(t, ip)
	} else {
		addr = down.LocalAddr().(*net.UDPAddr).AddrPort()
	}
	up, _ := listen(t, ip)
	if cfg.Lifetimes == (interest.Lifetimes{}) {
		cfg.Lifetimes = shortLives
	}
	if cfg.SenderID == 0 {
		cfg.SenderID = interest.NewSenderID("relay-" + ip)
	}
	if cfg.Allow == nil {
		cfg.Allow = []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}
	}
	cfg.Logger = quiet
	r, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	n := &node{r: r, addr: addr, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(n.done)
		if err := r.Serve(ctx, down, up); err != nil {
			t.Errorf("relay %s: %v", ip, err)
		}
	}()
	t.Cleanup(n.stop)
	return n
}

func (n *node) stop() {
	n.cancel()
	<-n.done
}

// host is an announcer.
type host struct {
	c      *interest.Client
	cancel context.CancelFunc
	done   chan struct{}
}

func startHost(t *testing.T, ip string, upstream netip.AddrPort) *host {
	t.Helper()
	conn, _ := listen(t, ip)
	c := interest.NewClient(interest.ClientConfig{SenderID: interest.NewSenderID("host-" + ip), Upstreams: []netip.AddrPort{upstream}})
	ctx, cancel := context.WithCancel(context.Background())
	h := &host{c: c, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(h.done)
		if err := interest.RunClient(ctx, conn, c, interest.RunOptions{}); err != nil {
			t.Errorf("host %s: %v", ip, err)
		}
	}()
	t.Cleanup(func() { cancel(); <-h.done })
	return h
}

// Two hosts behind two chained relays reach a terminus. The terminus sees
// one sender, the union of what the hosts want, and a topic two hosts want
// stays until the second lets go.
func TestChain(t *testing.T) {
	tm := newTerminus(t, "127.0.0.1")
	ra := startRelay(t, "127.0.0.2", Config{Upstreams: []netip.AddrPort{tm.addr}})
	rb := startRelay(t, "127.0.0.3", Config{Upstreams: []netip.AddrPort{ra.addr}})
	h1 := startHost(t, "127.0.0.4", rb.addr)
	h2 := startHost(t, "127.0.0.5", rb.addr)

	only1, only2, shared := topics("one", 40), topics("two", 30), topics("shared", 10)
	for _, x := range append(only1, shared...) {
		h1.c.Add(x)
	}
	for _, x := range append(only2, shared...) {
		h2.c.Add(x)
	}
	eventually(t, 10*time.Second, "the union at the terminus", func() bool { return tm.live() == 80 })

	if got := tm.receiver().Senders(); len(got) != 1 || got[0].Topics != 80 {
		t.Fatalf("the terminus sees %+v, want the one relay with the union", got)
	}
	if got := ra.r.Receiver().Senders(); len(got) != 1 || got[0].Topics != 80 {
		t.Fatalf("the upper relay sees %+v", got)
	}
	if got := rb.r.Receiver().Senders(); len(got) != 2 {
		t.Fatalf("the lower relay sees %d senders, want the two hosts", len(got))
	}
	if st := rb.r.Receiver().Stats(); st.Topics != 90 || st.LiveTopics != 80 {
		t.Fatalf("lower relay holds %d topics, %d distinct", st.Topics, st.LiveTopics)
	}
	// Hop counts: a host is 0, each relay adds one.
	eventually(t, 5*time.Second, "hop counts to settle", func() bool {
		s := tm.receiver().Senders()
		return len(s) == 1 && s[0].Hops == 2 && rb.r.Hops() == 1 && ra.r.Hops() == 2
	})
	for _, x := range append(only1, shared...) {
		if st := h1.c.Status(x); st != interest.StatusOK {
			t.Fatalf("host status %v", st)
		}
	}

	for _, x := range shared {
		h1.c.Remove(x)
	}
	time.Sleep(300 * time.Millisecond)
	if tm.live() != 80 {
		t.Fatalf("%d live after one of two holders let go, want 80 still", tm.live())
	}
	for _, x := range shared {
		h2.c.Remove(x)
	}
	eventually(t, 10*time.Second, "the shared topics to go", func() bool { return tm.live() == 70 })
	if !ra.r.Ready() || !rb.r.Ready() {
		t.Fatal("relays not ready")
	}
}

// The terminus delivers only some groups. A topic outside them is not held
// anywhere on the way, and the host that asked is told no stream will come.
func TestScopeReachesTheHost(t *testing.T) {
	const bits = 3
	sc, err := interest.NewScope(shard.DomainBEEF, bits, [16]byte{7}, []uint16{0x1000, 0x1001, 0x1002})
	if err != nil {
		t.Fatal(err)
	}
	tm := newTerminus(t, "127.0.0.1")
	tm.pol.mu.Lock()
	tm.pol.scope = sc
	tm.pol.mu.Unlock()
	ra := startRelay(t, "127.0.0.2", Config{Upstreams: []netip.AddrPort{tm.addr}})
	rb := startRelay(t, "127.0.0.3", Config{Upstreams: []netip.AddrPort{ra.addr}})
	h := startHost(t, "127.0.0.4", rb.addr)

	ts := topics("t", 200)
	in := 0
	for _, x := range ts {
		h.c.Add(x)
		if sc.Contains(x) {
			in++
		}
	}
	if in == 0 || in == len(ts) {
		t.Fatal("the test needs topics on both sides of the scope")
	}
	eventually(t, 15*time.Second, "the host to learn what is served", func() bool {
		for _, x := range ts {
			want := interest.StatusNotServed
			if sc.Contains(x) {
				want = interest.StatusOK
			}
			// Before the scope has travelled down, the relay holds an
			// unserved topic and says so with a note; afterwards the host
			// does not send it at all.
			if got := h.c.Status(x); got != want {
				return false
			}
		}
		return tm.live() == in
	})
	// Nothing on the way holds state for what is not served.
	eventually(t, 10*time.Second, "the relays to drop what is not served", func() bool {
		return ra.r.Receiver().Stats().Topics == in && rb.r.Receiver().Stats().Topics == in &&
			ra.r.Client().Wanted() == in && rb.r.Client().Wanted() == in
	})
	if got := tm.receiver().Stats().Topics; got != in {
		t.Fatalf("the terminus holds %d topics, %d are in scope", got, in)
	}
	if up := h.c.Upstreams()[0]; up.Scope == nil || !up.Scope.Equal(sc) {
		t.Fatalf("the host's view of the scope: %+v", up.Scope)
	}

	// The scope moves. What is newly served arrives; what is no longer
	// served goes, all the way down.
	sc2, _ := interest.NewScope(shard.DomainBEEF, bits, [16]byte{7}, []uint16{0x1002, 0x1003, 0x1004, 0x1005})
	in2 := 0
	for _, x := range ts {
		if sc2.Contains(x) {
			in2++
		}
	}
	tm.pol.mu.Lock()
	tm.pol.scope = sc2
	tm.pol.mu.Unlock()
	tm.receiver().Reevaluate("edge", time.Now())
	tm.receiver().PokeAll()
	eventually(t, 15*time.Second, "the new scope to reach the host", func() bool {
		for _, x := range ts {
			want := interest.StatusNotServed
			if sc2.Contains(x) {
				want = interest.StatusOK
			}
			if h.c.Status(x) != want {
				return false
			}
		}
		return tm.live() == in2
	})
}

// The edge restarts and holds nothing. The relay notices at its next
// refresh and restores everything without the hosts doing anything.
func TestTerminusRestart(t *testing.T) {
	tm := newTerminus(t, "127.0.0.1")
	ra := startRelay(t, "127.0.0.2", Config{Upstreams: []netip.AddrPort{tm.addr}})
	h := startHost(t, "127.0.0.4", ra.addr)
	for _, x := range topics("t", 120) {
		h.c.Add(x)
	}
	eventually(t, 10*time.Second, "the set at the terminus", func() bool { return tm.live() == 120 })
	tm.restart()
	if tm.live() != 0 {
		t.Fatal("a restarted terminus must start empty")
	}
	// The refresh interval is a third of the 3 s lifetime.
	eventually(t, 10*time.Second, "the set at the restarted terminus", func() bool { return tm.live() == 120 })
	if st := h.c.Upstreams()[0].Stats; st.Syncs != 1 {
		t.Fatalf("the host replayed %d times; the restart was two hops away and none of its business", st.Syncs)
	}
}

// A relay restarts. It comes back empty and relearns its set from the
// hosts over the next refresh. Meanwhile its upstream keeps what the
// previous run announced: nothing that is still wanted stops.
func TestRelayRestartDropsNothing(t *testing.T) {
	tm := newTerminus(t, "127.0.0.1")
	down, _ := listen(t, "127.0.0.2")
	cfg := Config{Upstreams: []netip.AddrPort{tm.addr}, RestartHold: -1}
	ra := startRelayOn(t, down, "127.0.0.2", cfg)
	h := startHost(t, "127.0.0.4", ra.addr)
	ts := topics("t", 60)
	for _, x := range ts {
		h.c.Add(x)
	}
	eventually(t, 10*time.Second, "the set at the terminus", func() bool { return tm.live() == 60 })

	ra.stop()
	// While the relay is down the host stops wanting some topics.
	for _, x := range ts[:15] {
		h.c.Remove(x)
	}
	ra2 := startRelayOn(t, down, "127.0.0.2", cfg)
	eventually(t, 10*time.Second, "the relay to relearn its set", func() bool { return ra2.r.Client().Wanted() == 45 })
	if tm.live() != 60 {
		t.Fatalf("%d live at the terminus during the hold; the previous run's 60 must still stand", tm.live())
	}
	// What the hosts no longer want goes when the hold ends or when its
	// lifetime at the terminus runs out, whichever comes first. With the
	// short lifetimes of this test that is the lifetime.
	eventually(t, 15*time.Second, "the unwanted topics to go", func() bool { return tm.live() == 45 })
	for _, x := range ts[15:] {
		if tm.dropped(x) != 0 {
			t.Fatal("a topic that was wanted throughout was dropped at the terminus")
		}
	}
	for _, x := range ts[:15] {
		if tm.dropped(x) != 1 {
			t.Fatalf("a topic no longer wanted was dropped %d times", tm.dropped(x))
		}
	}
}

// Without a hold, the same restart drops everything until the hosts check
// in again. This is the behaviour the hold exists to prevent, kept as a
// test so the difference stays visible.
func TestRelayRestartWithoutHold(t *testing.T) {
	tm := newTerminus(t, "127.0.0.1")
	down, _ := listen(t, "127.0.0.2")
	cfg := Config{Upstreams: []netip.AddrPort{tm.addr}}
	ra := startRelayOn(t, down, "127.0.0.2", cfg)
	h := startHost(t, "127.0.0.4", ra.addr)
	ts := topics("t", 20)
	for _, x := range ts {
		h.c.Add(x)
	}
	eventually(t, 10*time.Second, "the set at the terminus", func() bool { return tm.live() == 20 })
	ra.stop()
	startRelayOn(t, down, "127.0.0.2", cfg)
	eventually(t, 10*time.Second, "the set to come back", func() bool {
		return tm.live() == 20 && tm.dropped(ts[0]) > 0
	})
}

// A sender outside the allowed prefixes is ignored, and so is anything
// from the relay's own upstream.
func TestWhoMayAnnounce(t *testing.T) {
	tm := newTerminus(t, "127.0.0.1")
	ra := startRelay(t, "127.0.0.2", Config{
		Upstreams: []netip.AddrPort{tm.addr},
		Allow:     []netip.Prefix{netip.MustParsePrefix("127.0.0.4/32"), netip.MustParsePrefix("127.0.0.1/32")},
	})
	allowed := startHost(t, "127.0.0.4", ra.addr)
	denied := startHost(t, "127.0.0.5", ra.addr)
	// 127.0.0.1 is in the allow list and is also the upstream's address.
	fromUpstream := startHost(t, "127.0.0.1", ra.addr)
	allowed.c.Add(topic("a"))
	denied.c.Add(topic("d"))
	fromUpstream.c.Add(topic("u"))
	eventually(t, 10*time.Second, "the allowed host's topic", func() bool { return tm.live() == 1 })
	time.Sleep(500 * time.Millisecond)
	if tm.live() != 1 || !tm.receiver().IsLive("edge", topic("a")) {
		t.Fatalf("%d live at the terminus", tm.live())
	}
	if st := ra.r.Receiver().Stats(); st.Unidentified < 2 || st.Senders != 1 {
		t.Fatalf("relay stats %+v", st)
	}
	if denied.c.Upstreams()[0].Reachable {
		t.Fatal("an ignored sender must get no answer at all")
	}
}

// Three relays configured in a ring. Interest circulates, the hop count
// climbs until a relay refuses it, and a topic nobody wants any more does
// not live on in the ring.
func TestRingDoesNotKeepATopicAlive(t *testing.T) {
	if testing.Short() {
		t.Skip("takes several lifetimes")
	}
	xd, xa := listen(t, "127.0.0.2")
	yd, ya := listen(t, "127.0.0.3")
	zd, za := listen(t, "127.0.0.6")
	lives := interest.Lifetimes{Min: 1, Default: 2, Max: 4}
	x := startRelayOn(t, xd, "127.0.0.2", Config{Upstreams: []netip.AddrPort{ya}, MaxHops: 4, Lifetimes: lives})
	y := startRelayOn(t, yd, "127.0.0.3", Config{Upstreams: []netip.AddrPort{za}, MaxHops: 4, Lifetimes: lives})
	z := startRelayOn(t, zd, "127.0.0.6", Config{Upstreams: []netip.AddrPort{xa}, MaxHops: 4, Lifetimes: lives})
	h := startHost(t, "127.0.0.4", xa)
	tp := topic("ring")
	h.c.Add(tp)
	eventually(t, 10*time.Second, "the topic to go round the ring", func() bool {
		return y.r.Receiver().IsLive(Principal, tp) && z.r.Receiver().IsLive(Principal, tp) &&
			len(x.r.Receiver().Senders()) == 2
	})
	h.c.Remove(tp)
	eventually(t, 60*time.Second, "the topic to die out of the ring", func() bool {
		return !x.r.Receiver().IsLive(Principal, tp) && !y.r.Receiver().IsLive(Principal, tp) &&
			!z.r.Receiver().IsLive(Principal, tp)
	})
	limited := x.r.Receiver().Stats().HopLimited + y.r.Receiver().Stats().HopLimited + z.r.Receiver().Stats().HopLimited
	if limited == 0 {
		t.Fatal("the ring was never refused for its hop count")
	}
	// And it stays dead.
	time.Sleep(3 * time.Second)
	if x.r.Receiver().IsLive(Principal, tp) || y.r.Receiver().IsLive(Principal, tp) || z.r.Receiver().IsLive(Principal, tp) {
		t.Fatal("the topic came back")
	}
}

func TestNewRejects(t *testing.T) {
	up := []netip.AddrPort{netip.MustParseAddrPort("192.0.2.1:8729")}
	allow := []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}
	cases := map[string]Config{
		"no upstream":   {Allow: allow, SenderID: 1},
		"no allow":      {Upstreams: up, SenderID: 1},
		"no sender id":  {Upstreams: up, Allow: allow},
		"bad lifetimes": {Upstreams: up, Allow: allow, SenderID: 1, Lifetimes: interest.Lifetimes{Min: 10, Default: 5, Max: 20}},
		"upstream port": {Upstreams: []netip.AddrPort{netip.AddrPortFrom(netip.MustParseAddr("192.0.2.1"), 0)}, Allow: allow, SenderID: 1},
	}
	for name, cfg := range cases {
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	r, err := New(Config{Upstreams: up, Allow: allow, SenderID: 1, RestartHold: -1})
	if err != nil {
		t.Fatal(err)
	}
	if r.cfg.RestartHold != 105*time.Second {
		t.Fatalf("automatic hold %s, want a third of the default lifetime and a margin", r.cfg.RestartHold)
	}
	if r.Ready() || r.Hops() != 1 {
		t.Fatal("a relay that has not started is not ready and is one hop")
	}
}

func TestDownstreamNote(t *testing.T) {
	cases := map[interest.Status]interest.Status{
		interest.StatusOK:                interest.StatusOK,
		interest.StatusPending:           interest.StatusOK,
		interest.StatusAddressNotHonored: interest.StatusOK,
		interest.StatusNotServed:         interest.StatusUnserved,
		interest.StatusUnserved:          interest.StatusUnserved,
		interest.StatusOutsideBound:      interest.StatusUpstreamRefused,
		interest.StatusOverCap:           interest.StatusUpstreamRefused,
		interest.StatusUpstreamRefused:   interest.StatusUpstreamRefused,
	}
	for in, want := range cases {
		if got := downstreamNote(in); got != want {
			t.Errorf("%v: note %v, want %v", in, got, want)
		}
	}
}
