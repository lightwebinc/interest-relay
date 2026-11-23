// Package distributor sends a delivery lane on to the hosts that asked for
// its topics: the relay's unicast distributor.
//
// The lane is a TCP stream of BRC-149 delivery records, each a 32-byte
// TopicID, a 4-byte big-endian length and the object, which an edge (or a
// distributor above) writes to the address provisioned for the consumer. The
// distributor ends it and writes each record, unchanged, to every host that
// announced the record's topic with a delivery address: one TCP stream per
// host. Delivery is at most once, as on the lane. A host that cannot keep up
// loses records, which are counted, and never slows the lane or the other
// hosts.
//
// Routes come from the relay's receiver ([interest.ReceiverConfig.OnDelivery]),
// so the data path reads its own table and never waits on the receiver.
package distributor

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lightwebinc/shard-common/interest"
	"github.com/lightwebinc/shard-common/objfmt"
)

const (
	// DefaultMaxRecord bounds a record, header included: the largest object
	// the object plane carries, plus the header.
	DefaultMaxRecord = objfmt.BEEFDeliveryHeaderSize + objfmt.DefaultMaxObject
	// DefaultQueue is how many records may wait for one host.
	DefaultQueue = 1024
	// DefaultQueueBytes is how many bytes may wait for one host. A slow host
	// is shed at whichever bound it reaches first, so a queue of large
	// objects cannot hold the relay's memory.
	DefaultQueueBytes = 64 << 20
	// writeTimeout is how long a host may take to accept writeChunk bytes. A
	// host that stops reading is disconnected and dialed again, instead of
	// holding its queue forever; one that reads slowly but steadily keeps
	// its stream, however large the record.
	writeTimeout = 10 * time.Second
	writeChunk   = 1 << 20
)

// Config configures a [Distributor].
type Config struct {
	// Listen is the TCP address the lane is delivered to.
	Listen string
	// MaxRecord bounds one record, its header included. A longer record
	// means a broken lane, which is closed. Zero uses DefaultMaxRecord.
	MaxRecord int
	// Queue and QueueBytes bound what waits for one host. Zero uses the
	// defaults.
	Queue      int
	QueueBytes int64
	// DialTimeout bounds one connection attempt to a host. Zero is 5 s.
	DialTimeout time.Duration
	// Logger defaults to slog.Default.
	Logger *slog.Logger
}

// Distributor is a running distributor. Build one with [New].
type Distributor struct {
	cfg  Config
	log  *slog.Logger
	dial func(ctx context.Context, network, addr string) (net.Conn, error)

	mu     sync.RWMutex
	routes map[interest.TopicID][]*host
	hosts  map[netip.AddrPort]*host
	closed bool // Serve has returned: a host started now would never stop

	addr      atomic.Pointer[net.Addr]
	lanes     atomic.Int64
	recordsIn atomic.Uint64
	bytesIn   atomic.Uint64
	unrouted  atomic.Uint64
	malformed atomic.Uint64
}

// New builds a distributor. Run it with [Distributor.Serve], and feed it
// routes with [Distributor.Route].
func New(cfg Config) *Distributor {
	if cfg.MaxRecord <= 0 {
		cfg.MaxRecord = DefaultMaxRecord
	}
	if cfg.Queue <= 0 {
		cfg.Queue = DefaultQueue
	}
	if cfg.QueueBytes <= 0 {
		cfg.QueueBytes = DefaultQueueBytes
	}
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = 5 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Distributor{
		cfg:    cfg,
		log:    cfg.Logger,
		dial:   (&net.Dialer{Timeout: cfg.DialTimeout}).DialContext,
		routes: make(map[interest.TopicID][]*host),
		hosts:  make(map[netip.AddrPort]*host),
	}
}

// Route adds or removes one route, a topic to a host. It is the relay's
// [interest.ReceiverConfig.OnDelivery]: the receiver reference-counts an
// address across the senders that give it, so each route arrives once. Only
// a unicast stream of delivery records is distributed; the relay's policy
// honors nothing else. Once Serve has returned, Route does nothing.
func (d *Distributor) Route(topic interest.TopicID, dl interest.Delivery, live bool) {
	if dl.Kind != interest.DeliverUnicast || dl.Encoding != interest.EncodeRecords {
		return
	}
	addr := netip.AddrPortFrom(dl.Addr.Unmap(), dl.Port)
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return
	}
	hs := d.routes[topic]
	at := -1
	for i, h := range hs {
		if h.addr == addr {
			at = i
		}
	}
	if live {
		if at >= 0 {
			return
		}
		h := d.hosts[addr]
		if h == nil {
			h = d.startHost(addr)
			d.hosts[addr] = h
		}
		h.topics++
		// A new slice, so a lane reading the old one is never written under.
		d.routes[topic] = append(append(make([]*host, 0, len(hs)+1), hs...), h)
		return
	}
	if at < 0 {
		return
	}
	h := hs[at]
	rest := append(append(make([]*host, 0, len(hs)-1), hs[:at]...), hs[at+1:]...)
	if len(rest) == 0 {
		delete(d.routes, topic)
	} else {
		d.routes[topic] = rest
	}
	if h.topics--; h.topics == 0 {
		delete(d.hosts, addr)
		close(h.stop)
	}
}

// Serve listens on Config.Listen and distributes every lane that connects,
// until ctx is done.
func (d *Distributor) Serve(ctx context.Context) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", d.cfg.Listen)
	if err != nil {
		d.stopHosts()
		return err
	}
	return d.ServeListener(ctx, ln)
}

// ServeListener distributes every lane that connects to ln, until ctx is
// done. It closes ln. When it returns every host's stream is closed, and
// the distributor is not served again.
func (d *Distributor) ServeListener(ctx context.Context, ln net.Listener) error {
	a := ln.Addr()
	d.addr.Store(&a)
	stop := context.AfterFunc(ctx, func() { _ = ln.Close() })
	defer stop()
	var wg sync.WaitGroup
	defer func() {
		wg.Wait()
		d.stopHosts()
	}()
	var delay time.Duration // since the last failed accept
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if errors.Is(err, net.ErrClosed) {
				return err
			}
			// Anything else (out of descriptors, say) passes: wait and accept
			// again, as net/http does, rather than end the relay with it.
			if delay == 0 {
				delay = 5 * time.Millisecond
			} else if delay *= 2; delay > time.Second {
				delay = time.Second
			}
			d.log.Warn("DISTRIBUTOR: accepting a lane failed; trying again", "err", err, "retry_in", delay)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(delay):
			}
			continue
		}
		delay = 0
		wg.Add(1)
		go func() {
			defer wg.Done()
			d.lane(ctx, c)
		}()
	}
}

// Addr is the address Serve listens on, once it does, or nil.
func (d *Distributor) Addr() net.Addr {
	if a := d.addr.Load(); a != nil {
		return *a
	}
	return nil
}

// lane reads one lane connection until it closes.
func (d *Distributor) lane(ctx context.Context, c net.Conn) {
	d.lanes.Add(1)
	defer d.lanes.Add(-1)
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()
	defer func() { _ = c.Close() }()
	br := bufio.NewReaderSize(c, 64<<10)
	var hdr [objfmt.BEEFDeliveryHeaderSize]byte
	for {
		if _, err := io.ReadFull(br, hdr[:]); err != nil {
			return // the lane closed; its sender dials again
		}
		n := binary.BigEndian.Uint32(hdr[32:36])
		total := objfmt.BEEFDeliveryHeaderSize + int64(n)
		if n == 0 || total > int64(d.cfg.MaxRecord) {
			d.malformed.Add(1)
			d.log.Warn("DISTRIBUTOR: closing a lane that sent a malformed record",
				"from", c.RemoteAddr().String(), "length", n, "max_record", d.cfg.MaxRecord)
			return
		}
		rec := make([]byte, total)
		copy(rec, hdr[:])
		if _, err := io.ReadFull(br, rec[objfmt.BEEFDeliveryHeaderSize:]); err != nil {
			return
		}
		d.recordsIn.Add(1)
		d.bytesIn.Add(uint64(total))
		var topic interest.TopicID
		copy(topic[:], rec[:32])
		d.mu.RLock()
		hs := d.routes[topic]
		for _, h := range hs {
			h.offer(rec)
		}
		d.mu.RUnlock()
		if len(hs) == 0 {
			d.unrouted.Add(1)
		}
	}
}

// host is one downstream: a TCP stream of the records of every topic routed
// to it.
type host struct {
	addr     netip.AddrPort
	topics   int // routes to it, under Distributor.mu
	q        chan []byte
	maxBytes int64
	queued   atomic.Int64
	stop     chan struct{}
	done     chan struct{}
	cur      atomic.Pointer[net.Conn] // the open stream, closed at once on stop

	connected  atomic.Bool
	sent       atomic.Uint64
	sentBytes  atomic.Uint64
	shed       atomic.Uint64
	dials      atomic.Uint64
	dialErrors atomic.Uint64
}

func (d *Distributor) startHost(addr netip.AddrPort) *host {
	h := &host{
		addr:     addr,
		q:        make(chan []byte, d.cfg.Queue),
		maxBytes: d.cfg.QueueBytes,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	go d.runHost(h)
	return h
}

// offer queues a record for the host, or sheds it when the host is behind.
// A record always fits an empty queue, so the largest record the lane may
// carry reaches a host that keeps up, whatever the queue's bound. It never
// blocks: the lane does not wait for any one host.
func (h *host) offer(rec []byte) {
	n := int64(len(rec))
	if q := h.queued.Add(n); q > h.maxBytes && q != n {
		h.queued.Add(-n)
		h.shed.Add(1)
		return
	}
	select {
	case h.q <- rec:
	default:
		h.queued.Add(-n)
		h.shed.Add(1)
	}
}

// runHost keeps the host's stream up and writes its records, until the last
// route to it goes.
func (d *Distributor) runHost(h *host) {
	defer close(h.done)
	// Stopping must not wait for a dial to a host that does not answer, or
	// for a write to one that stopped reading: cancelling ends the dial, and
	// closing the stream ends the write, at once.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-h.stop
		cancel()
		if c := h.cur.Load(); c != nil {
			_ = (*c).Close()
		}
	}()
	var conn net.Conn
	defer func() {
		if conn != nil {
			_ = conn.Close()
		}
		h.connected.Store(false)
	}()
	backoff := 500 * time.Millisecond
	for {
		var rec []byte
		select {
		case <-h.stop:
			return
		case rec = <-h.q:
			h.queued.Add(-int64(len(rec)))
		}
		for conn == nil {
			h.dials.Add(1)
			c, err := d.dial(ctx, "tcp", h.addr.String())
			if err == nil {
				conn, backoff = c, 500*time.Millisecond
				h.cur.Store(&c)
				h.connected.Store(true)
				select {
				case <-h.stop: // stopped while dialing: the watcher may have missed it
					return
				default:
				}
				break
			}
			h.dialErrors.Add(1)
			d.log.Debug("distributor: cannot reach a host", "host", h.addr.String(), "err", err, "retry_in", backoff)
			// The record waiting is lost; what queues meanwhile is shed at the
			// queue's bound. At most once, as on the lane.
			h.shed.Add(1)
			rec = nil
			select {
			case <-h.stop:
				return
			case <-time.After(backoff):
			}
			if backoff *= 2; backoff > 10*time.Second {
				backoff = 10 * time.Second
			}
			select {
			case <-h.stop:
				return
			case rec = <-h.q:
				h.queued.Add(-int64(len(rec)))
			}
		}
		if rec == nil {
			continue
		}
		if err := writeRecord(conn, rec, writeChunk, writeTimeout); err != nil {
			d.log.Debug("distributor: a host's stream failed", "host", h.addr.String(), "err", err)
			h.shed.Add(1)
			h.cur.Store(nil)
			_ = conn.Close()
			conn = nil
			h.connected.Store(false)
			continue
		}
		h.sent.Add(1)
		h.sentBytes.Add(uint64(len(rec)))
	}
}

// writeRecord writes rec to c, allowing timeout for each chunk of it.
func writeRecord(c net.Conn, rec []byte, chunk int, timeout time.Duration) error {
	for len(rec) > 0 {
		n := min(len(rec), chunk)
		if err := c.SetWriteDeadline(time.Now().Add(timeout)); err != nil {
			return err
		}
		if _, err := c.Write(rec[:n]); err != nil {
			return err
		}
		rec = rec[n:]
	}
	return nil
}

func (d *Distributor) stopHosts() {
	d.mu.Lock()
	d.closed = true
	hs := make([]*host, 0, len(d.hosts))
	for _, h := range d.hosts {
		hs = append(hs, h)
	}
	d.hosts = make(map[netip.AddrPort]*host)
	d.routes = make(map[interest.TopicID][]*host)
	d.mu.Unlock()
	// All at once: each host's stop is prompt, and none waits on another's.
	for _, h := range hs {
		close(h.stop)
	}
	for _, h := range hs {
		<-h.done
	}
}

// HostStats is one downstream's counters.
type HostStats struct {
	Addr       netip.AddrPort
	Topics     int
	Connected  bool
	Sent       uint64
	SentBytes  uint64
	Shed       uint64
	Dials      uint64
	DialErrors uint64
	Queued     int64 // bytes waiting
}

// Stats is the distributor's counters.
type Stats struct {
	Lanes     int64
	RecordsIn uint64
	BytesIn   uint64
	Unrouted  uint64 // records whose topic no host asked for
	Malformed uint64 // lanes closed for a malformed record
	Routes    int    // (topic, host) pairs
	Hosts     []HostStats
}

// Stats returns the counters, hosts in address order.
func (d *Distributor) Stats() Stats {
	d.mu.RLock()
	st := Stats{
		Lanes:     d.lanes.Load(),
		RecordsIn: d.recordsIn.Load(),
		BytesIn:   d.bytesIn.Load(),
		Unrouted:  d.unrouted.Load(),
		Malformed: d.malformed.Load(),
	}
	for _, hs := range d.routes {
		st.Routes += len(hs)
	}
	for _, h := range d.hosts {
		st.Hosts = append(st.Hosts, HostStats{
			Addr: h.addr, Topics: h.topics, Connected: h.connected.Load(),
			Sent: h.sent.Load(), SentBytes: h.sentBytes.Load(), Shed: h.shed.Load(),
			Dials: h.dials.Load(), DialErrors: h.dialErrors.Load(), Queued: h.queued.Load(),
		})
	}
	d.mu.RUnlock()
	sort.Slice(st.Hosts, func(i, j int) bool { return st.Hosts[i].Addr.Compare(st.Hosts[j].Addr) < 0 })
	return st
}
