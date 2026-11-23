package distributor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lightwebinc/shard-common/interest"
	"github.com/lightwebinc/shard-common/objfmt"
)

func tid(name string) interest.TopicID { return interest.TopicID(objfmt.TopicID(name)) }

func rec(name, payload string) []byte {
	return objfmt.EncodeBEEFDelivery(objfmt.TopicID(name), []byte(payload))
}

func route(addr netip.AddrPort) interest.Delivery {
	return interest.Delivery{Kind: interest.DeliverUnicast, Encoding: interest.EncodeRecords, Addr: addr.Addr(), Port: addr.Port()}
}

// sink is a host: a TCP listener that keeps every record it reads.
type sink struct {
	ln    net.Listener
	done  chan struct{}
	mu    sync.Mutex
	got   [][]byte
	conns []net.Conn
	read  bool // false: accept and never read, a host that stalls
}

// kill stops the host as a crash would: the listener and every connection.
func (s *sink) kill() {
	_ = s.ln.Close()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.conns {
		_ = c.Close()
	}
}

func newSink(t *testing.T, read bool) *sink {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return serveSink(t, ln, read)
}

func serveSink(t *testing.T, ln net.Listener, read bool) *sink {
	s := &sink{ln: ln, read: read, done: make(chan struct{})}
	t.Cleanup(func() { close(s.done); _ = ln.Close() })
	go s.serve()
	return s
}

func (s *sink) serve() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.conns = append(s.conns, c)
		s.mu.Unlock()
		go func() {
			defer func() { _ = c.Close() }()
			if !s.read {
				<-s.done // hold the connection and read nothing until the test ends
				return
			}
			br := bufio.NewReader(c)
			for {
				var hdr [objfmt.BEEFDeliveryHeaderSize]byte
				if _, err := io.ReadFull(br, hdr[:]); err != nil {
					return
				}
				size := int(binary.BigEndian.Uint32(hdr[32:36]))
				r := make([]byte, objfmt.BEEFDeliveryHeaderSize+size)
				copy(r, hdr[:])
				if _, err := io.ReadFull(br, r[objfmt.BEEFDeliveryHeaderSize:]); err != nil {
					return
				}
				s.mu.Lock()
				s.got = append(s.got, r)
				s.mu.Unlock()
			}
		}()
	}
}

func (s *sink) addr() netip.AddrPort { return netip.MustParseAddrPort(s.ln.Addr().String()) }

// waitFor returns once the sink holds n records, or fails.
func (s *sink) waitFor(t *testing.T, n int) [][]byte {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.mu.Lock()
		got := append([][]byte(nil), s.got...)
		s.mu.Unlock()
		if len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("sink %s has %d records, want %d", s.ln.Addr(), len(got), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (s *sink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.got)
}

// start runs a distributor and returns it with a connected lane.
func start(t *testing.T, cfg Config) (*Distributor, net.Conn) {
	t.Helper()
	d := New(cfg)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = d.ServeListener(ctx, ln)
	}()
	t.Cleanup(func() { cancel(); <-done })
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return d, c
}

func write(t *testing.T, c net.Conn, recs ...[]byte) {
	t.Helper()
	for _, r := range recs {
		if _, err := c.Write(r); err != nil {
			t.Fatal(err)
		}
	}
}

// Each record goes to every host that asked for its topic, in order, and
// to no other.
func TestFanOut(t *testing.T) {
	a, b := newSink(t, true), newSink(t, true)
	d, lane := start(t, Config{})
	x, y := tid("x"), tid("y")
	d.Route(x, route(a.addr()), true)
	d.Route(x, route(b.addr()), true)
	d.Route(y, route(b.addr()), true)
	x1, y1, x2 := rec("x", "x1"), rec("y", "y1"), rec("x", "x2")
	write(t, lane, x1, y1, x2)
	if got := a.waitFor(t, 2); !bytes.Equal(got[0], x1) || !bytes.Equal(got[1], x2) {
		t.Fatalf("a got %q", got)
	}
	if got := b.waitFor(t, 3); !bytes.Equal(got[0], x1) || !bytes.Equal(got[1], y1) || !bytes.Equal(got[2], x2) {
		t.Fatalf("b got %q", got)
	}

	// The route goes: b stops receiving x, a does not.
	d.Route(x, route(b.addr()), false)
	x3 := rec("x", "x3")
	write(t, lane, x3)
	if got := a.waitFor(t, 3); !bytes.Equal(got[2], x3) {
		t.Fatalf("a got %q", got)
	}
	time.Sleep(100 * time.Millisecond)
	if b.count() != 3 {
		t.Fatalf("b got x after its route went: %d records", b.count())
	}
	if st := d.Stats(); st.RecordsIn != 4 || st.Routes != 2 || len(st.Hosts) != 2 {
		t.Fatalf("stats %+v", st)
	}
}

// A record no host asked for is counted. A malformed record closes the
// lane, which its sender then dials again.
func TestUnroutedAndMalformed(t *testing.T) {
	d, lane := start(t, Config{MaxRecord: 1024})
	write(t, lane, rec("z", "nobody asked"))
	deadline := time.Now().Add(5 * time.Second)
	for d.Stats().Unrouted != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("stats %+v", d.Stats())
		}
		time.Sleep(5 * time.Millisecond)
	}
	write(t, lane, rec("z", string(make([]byte, 2048)))) // longer than MaxRecord
	_ = lane.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := lane.Read(make([]byte, 1)); err == nil {
		t.Fatal("the lane stayed open after a record over the bound")
	}
	if st := d.Stats(); st.Malformed != 1 {
		t.Fatalf("stats %+v", st)
	}
}

// A host that stops reading is shed. The lane and the other host do not
// slow down for it.
func TestSlowHostIsShed(t *testing.T) {
	fast, stalled := newSink(t, true), newSink(t, false)
	const queue = 4
	d, lane := start(t, Config{Queue: queue, QueueBytes: 1 << 20})
	x := tid("x")
	d.Route(x, route(fast.addr()), true)
	d.Route(x, route(stalled.addr()), true)
	payload := string(make([]byte, 64<<10))
	const n = 400 // 25 MiB: more than a stalled connection's buffers hold
	for i := 0; i < n; i += queue {
		for range queue {
			write(t, lane, rec("x", payload))
		}
		// Paced by the host that reads, so that its own queue never
		// overflows. Were the lane held up by the stalled host, this would
		// time out.
		fast.waitFor(t, i+queue)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		var shed uint64
		for _, h := range d.Stats().Hosts {
			if h.Addr == stalled.addr() {
				shed = h.Shed
			}
		}
		if shed > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("nothing shed for the stalled host: %+v", d.Stats())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A host that restarts is dialed again.
func TestHostReconnects(t *testing.T) {
	first := newSink(t, true)
	addr := first.addr()
	d, lane := start(t, Config{})
	x := tid("x")
	d.Route(x, route(addr), true)
	write(t, lane, rec("x", "before"))
	first.waitFor(t, 1)
	first.kill()

	// Restart the host on the same port, and keep the lane talking until a
	// record reaches it.
	ln, err := net.Listen("tcp", addr.String())
	if err != nil {
		t.Skipf("port %s not free again: %v", addr, err)
	}
	second := serveSink(t, ln, true)
	deadline := time.Now().Add(10 * time.Second)
	for second.count() == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("the restarted host got nothing: %+v", d.Stats())
		}
		write(t, lane, rec("x", "after"))
		time.Sleep(50 * time.Millisecond)
	}
}

// Once Serve has returned, nothing is left running and a route starts
// nothing: a host started then would never be stopped. That holds when the
// listener could not be opened, too.
func TestNoHostOutlivesServe(t *testing.T) {
	a := newSink(t, true)
	d := New(Config{})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.ServeListener(ctx, ln) }()
	d.Route(tid("x"), route(a.addr()), true)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	d.Route(tid("y"), route(a.addr()), true)
	if st := d.Stats(); st.Routes != 0 || len(st.Hosts) != 0 {
		t.Fatalf("after Serve returned: %+v", st)
	}

	d = New(Config{Listen: "not an address"})
	d.Route(tid("x"), route(a.addr()), true)
	if err := d.Serve(context.Background()); err == nil {
		t.Fatal("Serve opened a listener on a bad address")
	}
	d.Route(tid("y"), route(a.addr()), true)
	if st := d.Stats(); st.Routes != 0 || len(st.Hosts) != 0 {
		t.Fatalf("after Serve failed: %+v", st)
	}
}

// A record larger than the queue's byte bound still reaches a host that
// keeps up. Under the defaults, that is the largest object the lane takes.
func TestRecordLargerThanTheQueue(t *testing.T) {
	a := newSink(t, true)
	d, lane := start(t, Config{QueueBytes: 1 << 10})
	d.Route(tid("x"), route(a.addr()), true)
	big := rec("x", string(make([]byte, 4<<10)))
	write(t, lane, big)
	if got := a.waitFor(t, 1); !bytes.Equal(got[0], big) {
		t.Fatalf("got %d bytes, want %d", len(got[0]), len(big))
	}

	h := &host{q: make(chan []byte, 1), maxBytes: DefaultQueueBytes}
	h.offer(make([]byte, DefaultMaxRecord))
	if h.shed.Load() != 0 || len(h.q) != 1 {
		t.Fatal("the largest record the lane takes was shed under the default bounds")
	}
	h.offer(make([]byte, 64))
	if h.shed.Load() != 1 {
		t.Fatal("a record queued behind a full queue was not shed")
	}
}

// A host that reads slowly but steadily gets a record that takes far longer
// than one write timeout. A host that stops reading is cut after one.
func TestWriteRecordPaced(t *testing.T) {
	record := make([]byte, 1<<20)
	steady := func() net.Conn {
		w, r := net.Pipe()
		t.Cleanup(func() { _ = w.Close(); _ = r.Close() })
		go func() {
			buf := make([]byte, 16<<10)
			for {
				if _, err := io.ReadFull(r, buf); err != nil {
					return
				}
				time.Sleep(10 * time.Millisecond) // about 40 ms a chunk, 640 ms in all
			}
		}()
		return w
	}
	const timeout = 300 * time.Millisecond
	began := time.Now()
	if err := writeRecord(steady(), record, 64<<10, timeout); err != nil {
		t.Fatalf("a steady reader was cut after %s: %v", time.Since(began), err)
	}
	if took := time.Since(began); took < timeout {
		t.Fatalf("the record took %s, under one timeout: the test shows nothing", took)
	}
	// One deadline for the whole record, as before chunking, cuts the same
	// reader.
	if err := writeRecord(steady(), record, len(record), timeout); err == nil {
		t.Fatal("one deadline for the whole record did not cut a reader that slow")
	}

	w2, r2 := net.Pipe()
	defer func() { _ = w2.Close(); _ = r2.Close() }()
	if err := writeRecord(w2, record, 64<<10, 100*time.Millisecond); err == nil {
		t.Fatal("a host that reads nothing was not cut")
	}
}

// Stopping does not wait for a dial to a host that does not answer.
func TestStopDoesNotWaitForADial(t *testing.T) {
	d := New(Config{})
	dialing := make(chan struct{}, 3)
	d.dial = func(ctx context.Context, _, _ string) (net.Conn, error) {
		dialing <- struct{}{}
		<-ctx.Done() // a host that never answers
		return nil, ctx.Err()
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- d.ServeListener(ctx, ln) }()
	for i := range 3 {
		d.Route(tid("x"), route(netip.AddrPortFrom(netip.MustParseAddr("192.0.2.1"), uint16(9000+i))), true)
	}
	lane, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lane.Close() }()
	write(t, lane, rec("x", "every host dials"))
	for range 3 {
		select {
		case <-dialing:
		case <-time.After(5 * time.Second):
			t.Fatal("the hosts did not dial")
		}
	}
	began := time.Now()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
		if took := time.Since(began); took > time.Second {
			t.Fatalf("stopping took %s", took)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stopping waited for the dials")
	}
}

// flakyListener fails its first Accepts, as a listener out of descriptors
// does.
type flakyListener struct {
	net.Listener
	fails atomic.Int32
}

func (l *flakyListener) Accept() (net.Conn, error) {
	if l.fails.Add(-1) >= 0 {
		return nil, errors.New("accept: too many open files")
	}
	return l.Listener.Accept()
}

// An error accepting a lane, other than the listener closing, is waited
// out. It does not end the distributor, and with it the relay.
func TestAcceptErrorIsWaitedOut(t *testing.T) {
	a := newSink(t, true)
	d := New(Config{})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fl := &flakyListener{Listener: ln}
	fl.fails.Store(3)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.ServeListener(ctx, fl) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	d.Route(tid("x"), route(a.addr()), true)
	lane, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lane.Close() }()
	write(t, lane, rec("x", "after the errors"))
	a.waitFor(t, 1)
}

// Only a unicast stream of delivery records is distributed.
func TestOnlyUnicastRecords(t *testing.T) {
	d := New(Config{})
	group := interest.Delivery{Kind: interest.DeliverMulticast, Encoding: interest.EncodeFrames, Addr: netip.MustParseAddr("ff0e::1"), Port: 9000}
	d.Route(tid("x"), group, true)
	if st := d.Stats(); st.Routes != 0 || len(st.Hosts) != 0 {
		t.Fatalf("stats %+v", st)
	}
}
