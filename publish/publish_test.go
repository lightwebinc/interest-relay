package publish

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/lightwebinc/shard-common/interest"
	"github.com/lightwebinc/shard-common/objfmt"

	"github.com/lightwebinc/interest-relay/distributor"
)

var beef = []byte{0x01, 0x00, 0xBE, 0xEF, 0x42, 0x43}

func record(t *testing.T, object []byte, names ...string) []byte {
	t.Helper()
	r, err := objfmt.EncodeBEEFRecord(names, object)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func tid(name string) interest.TopicID { return interest.TopicID(objfmt.TopicID(name)) }

// upstream stands in for the up-tunnel queue.
type upstream struct {
	mu   sync.Mutex
	got  [][]byte
	full bool
}

func (u *upstream) Publish(_ context.Context, rec []byte, sent func()) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.full {
		return errors.New("queue full")
	}
	u.got = append(u.got, rec)
	if sent != nil {
		sent()
	}
	return nil
}

func (u *upstream) records() [][]byte {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([][]byte(nil), u.got...)
}

// host is a sibling's lane: a listener that keeps the delivery records it
// reads.
type host struct {
	ln  net.Listener
	mu  sync.Mutex
	got [][]byte
}

func newHost(t *testing.T, addr string) *host {
	t.Helper()
	ln, err := net.Listen("tcp", addr+":0")
	if err != nil {
		t.Skip("cannot listen on", addr, err)
	}
	h := &host{ln: ln}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = c.Close() }()
				br := bufio.NewReader(c)
				for {
					var hdr [objfmt.BEEFDeliveryHeaderSize]byte
					if _, err := io.ReadFull(br, hdr[:]); err != nil {
						return
					}
					n := int(hdr[32])<<24 | int(hdr[33])<<16 | int(hdr[34])<<8 | int(hdr[35])
					r := make([]byte, len(hdr)+n)
					copy(r, hdr[:])
					if _, err := io.ReadFull(br, r[len(hdr):]); err != nil {
						return
					}
					h.mu.Lock()
					h.got = append(h.got, r)
					h.mu.Unlock()
				}
			}()
		}
	}()
	return h
}

func (h *host) addr() netip.AddrPort { return netip.MustParseAddrPort(h.ln.Addr().String()) }

func (h *host) records() [][]byte {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([][]byte(nil), h.got...)
}

func (h *host) waitFor(t *testing.T, n int) [][]byte {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if got := h.records(); len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("host %s has %d records, want %d", h.ln.Addr(), len(h.records()), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func route(a netip.AddrPort) interest.Delivery {
	return interest.Delivery{Kind: interest.DeliverUnicast, Encoding: interest.EncodeRecords, Addr: a.Addr(), Port: a.Port()}
}

// rig is a distributor and a publish point in front of a stand-in upstream.
type rig struct {
	dist *distributor.Distributor
	srv  *Server
	up   *upstream
	addr string
}

func newRig(t *testing.T, cfg Config) *rig {
	t.Helper()
	if cfg.Allow == nil {
		cfg.Allow = []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}
	}
	ctx, cancel := context.WithCancel(context.Background())
	dist := distributor.New(distributor.Config{Listen: "127.0.0.1:0"})
	dln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	up := &upstream{}
	srv := newServer(cfg, up, dist)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = dist.ServeListener(ctx, dln) }()
	go func() { defer wg.Done(); _ = srv.ServeListener(ctx, ln) }()
	t.Cleanup(func() { cancel(); wg.Wait() })
	return &rig{dist: dist, srv: srv, up: up, addr: ln.Addr().String()}
}

func (r *rig) dial(t *testing.T) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", r.addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func (r *rig) waitUp(t *testing.T, n int) [][]byte {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if got := r.up.records(); len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("upstream has %d records, want %d", len(r.up.records()), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A host's record goes up the tunnel verbatim and, at once, to its sibling
// as the delivery record the edge would send; never back to the publisher.
func TestPublishGoesUpAndToSiblings(t *testing.T) {
	r := newRig(t, Config{})
	self := newHost(t, "127.0.0.1")    // the publisher's own lane
	sibling := newHost(t, "127.0.0.2") // another host behind the relay
	r.dist.Route(tid("x"), route(self.addr()), true)
	r.dist.Route(tid("x"), route(sibling.addr()), true)

	rec := record(t, beef, "x")
	c := r.dial(t)
	if _, err := c.Write(rec); err != nil {
		t.Fatal(err)
	}
	if got := r.waitUp(t, 1); !bytes.Equal(got[0], rec) {
		t.Fatalf("upstream got %x", got[0])
	}
	want := objfmt.EncodeBEEFDelivery(objfmt.TopicID("x"), rec)
	if got := sibling.waitFor(t, 1); !bytes.Equal(got[0], want) {
		t.Fatalf("sibling got %x, want %x", got[0], want)
	}
	time.Sleep(100 * time.Millisecond)
	if n := len(self.records()); n != 0 {
		t.Fatalf("the publisher got %d of its own records", n)
	}
	st := r.srv.Stats()
	if st.RecordsIn != 1 || st.Accepted != 1 || st.Confirmed != 1 || st.Copies != 1 {
		t.Fatalf("stats %+v", st)
	}
}

// A sibling that asked for any deliverable name gets the record, under the
// first name; one that asked for a label does not.
func TestCopiedOnDeliverableNames(t *testing.T) {
	r := newRig(t, Config{DeliverTopics: 2})
	b, d := newHost(t, "127.0.0.2"), newHost(t, "127.0.0.3")
	r.dist.Route(tid("b"), route(b.addr()), true)
	r.dist.Route(tid("d"), route(d.addr()), true)
	rec := record(t, beef, "a", "b", "c", "d")
	c := r.dial(t)
	if _, err := c.Write(rec); err != nil {
		t.Fatal(err)
	}
	want := objfmt.EncodeBEEFDelivery(objfmt.TopicID("a"), rec)
	if got := b.waitFor(t, 1); !bytes.Equal(got[0], want) {
		t.Fatalf("b got %x", got[0])
	}
	time.Sleep(100 * time.Millisecond)
	if len(d.records()) != 0 {
		t.Fatal("a label's host got the record")
	}
}

// Records the edge would refuse are skipped and the stream goes on; none is
// sent up or copied.
func TestBoundsApplied(t *testing.T) {
	r := newRig(t, Config{MaxObject: 16, SourceObjects: 2})
	sibling := newHost(t, "127.0.0.2")
	r.dist.Route(tid("x"), route(sibling.addr()), true)
	c := r.dial(t)
	big := record(t, append(append([]byte{}, beef...), make([]byte, 32)...), "x")
	notBEEF := record(t, []byte("plain text"), "x")
	ok1, ok2, over := record(t, beef, "x"), record(t, append(append([]byte{}, beef...), 1), "x"), record(t, append(append([]byte{}, beef...), 2), "x")
	for _, b := range [][]byte{big, notBEEF, ok1, ok2, over} {
		if _, err := c.Write(b); err != nil {
			t.Fatal(err)
		}
	}
	r.waitUp(t, 2)
	time.Sleep(100 * time.Millisecond)
	if got := r.up.records(); len(got) != 2 || !bytes.Equal(got[0], ok1) || !bytes.Equal(got[1], ok2) {
		t.Fatalf("upstream got %d records", len(got))
	}
	sibling.waitFor(t, 2)
	st := r.srv.Stats()
	if st.Refused[ReasonOversize] != 1 || st.Refused[ReasonNotBEEF] != 1 || st.Refused[ReasonRate] != 1 {
		t.Fatalf("refused %v", st.Refused)
	}
}

// A record the up-tunnel queue cannot take is not copied: no sibling gets
// what the edge will not carry for the others.
func TestNotCopiedWhenNotSentUp(t *testing.T) {
	r := newRig(t, Config{})
	r.up.full = true
	sibling := newHost(t, "127.0.0.2")
	r.dist.Route(tid("x"), route(sibling.addr()), true)
	c := r.dial(t)
	if _, err := c.Write(record(t, beef, "x")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for r.srv.Stats().Refused[ReasonQueueFull] != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("stats %+v", r.srv.Stats())
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	if len(sibling.records()) != 0 {
		t.Fatal("a record that never went up was copied")
	}
}

// A stream that is not submission records, a malformed record, and a
// connection from outside the allowed prefixes are closed.
func TestRefusedStreams(t *testing.T) {
	r := newRig(t, Config{Allow: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}})
	closed := func(c net.Conn) bool {
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, err := c.Read(make([]byte, 1))
		return err != nil && !errors.Is(err, context.DeadlineExceeded)
	}
	c := r.dial(t)
	_, _ = c.Write([]byte{0xE3, 0xE1, 0xF3, 0xE8, 0, 0}) // framed objects
	if !closed(c) {
		t.Fatal("a framed stream was kept")
	}
	c = r.dial(t)
	_, _ = c.Write([]byte{0xBE, 0xEF, 0x01, 0x00}) // no topics
	if !closed(c) {
		t.Fatal("a malformed record was kept")
	}
	d := net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP("127.0.0.2")}}
	c2, err := d.Dial("tcp", r.addr)
	if err != nil {
		t.Skip("cannot dial from 127.0.0.2:", err)
	}
	defer func() { _ = c2.Close() }()
	_, _ = c2.Write(record(t, beef, "x"))
	if !closed(c2) {
		t.Fatal("a host outside the allowed prefixes was kept")
	}
	st := r.srv.Stats()
	if st.Refused[ReasonGrammar] != 1 || st.Refused[ReasonMalformed] != 1 || st.Refused[ReasonSource] != 1 || len(r.up.records()) != 0 {
		t.Fatalf("refused %v, up %d", st.Refused, len(r.up.records()))
	}
}

// Through the bridge's up-tunnel client: the record reaches the edge's
// ingress verbatim, from the consumer's own address.
func TestUpTunnelFromTheConsumersAddress(t *testing.T) {
	ingress, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ingress.Close() }()
	type got struct {
		from string
		data []byte
	}
	ch := make(chan got, 1)
	go func() {
		c, err := ingress.Accept()
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		rec := record(t, beef, "x")
		buf := make([]byte, len(rec))
		if _, err := io.ReadFull(c, buf); err == nil {
			ch <- got{c.RemoteAddr().(*net.TCPAddr).IP.String(), buf}
		}
	}()
	srv, err := New(Config{
		Listen:  "127.0.0.1:0",
		Allow:   []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")},
		Ingress: []string{ingress.Addr().String()},
		Source:  netip.MustParseAddr("127.0.0.1"),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = srv.ServeListener(ctx, ln) }()
	defer func() { cancel(); <-done }()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	rec := record(t, beef, "x")
	if _, err := c.Write(rec); err != nil {
		t.Fatal(err)
	}
	select {
	case g := <-ch:
		if g.from != "127.0.0.1" || !bytes.Equal(g.data, rec) {
			t.Fatalf("ingress got %x from %s", g.data, g.from)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("nothing reached the ingress")
	}
}

func TestNewRequires(t *testing.T) {
	base := Config{Allow: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}, Ingress: []string{"[fd00::a]:8725"}, Source: netip.MustParseAddr("fd00::1")}
	for name, mod := range map[string]func(*Config){
		"source":  func(c *Config) { c.Source = netip.Addr{} },
		"ingress": func(c *Config) { c.Ingress = nil },
		"allow":   func(c *Config) { c.Allow = nil },
	} {
		cfg := base
		mod(&cfg)
		if _, err := New(cfg, nil); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
}

// A listener closed under a live context ends ServeListener: the run and
// the connections are cancelled, not waited on forever.
func TestServeListenerEndsWhenTheListenerCloses(t *testing.T) {
	s := newServer(Config{Allow: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}}, &upstream{}, nil)
	s.run = func(ctx context.Context) error { <-ctx.Done(); return nil }
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.ServeListener(t.Context(), ln) }()
	c, err := net.Dial("tcp", ln.Addr().String()) // a host connected, idle
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	time.Sleep(50 * time.Millisecond)
	_ = ln.Close()
	select {
	case err := <-done:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("ServeListener returned %v, want the listener's close", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ServeListener did not return")
	}
}

// When every tracked source is spending, a new one is refused rather than
// every budget being started over; an idle one makes room.
func TestSourcesAreNotStartedOver(t *testing.T) {
	s := newServer(Config{SourceObjects: 1}, &upstream{}, nil)
	now := time.Now()
	for i := range maxTrackedSources {
		a := netip.AddrFrom4([4]byte{10, byte(i >> 16), byte(i >> 8), byte(i)})
		s.sources[a] = &budget{objects: bucket{rate: 1}, last: now}
	}
	busy := netip.AddrFrom4([4]byte{10, 0, 0, 1})
	s.sources[busy].objects = bucket{rate: 1, tokens: 0, last: now}
	if s.spend(netip.MustParseAddr("10.9.9.9"), 10) {
		t.Fatal("a new source was taken while every tracked source was spending")
	}
	if s.spend(busy, 10) {
		t.Fatal("a spent source was given a fresh budget")
	}
	for _, b := range s.sources {
		b.last = now.Add(-2 * time.Second)
	}
	if !s.spend(netip.MustParseAddr("10.9.9.9"), 10) {
		t.Fatal("idle sources did not make room")
	}
}
