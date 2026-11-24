// Package publish makes a distributing relay its domain's publish point.
//
// Hosts behind one relay share one consumer: one tunnel, one lane. The edge
// leaves a consumer's own publications off its lane, matching the address a
// frame was submitted from against the consumer's own, so a host publishing
// through the tunnel from that address never reaches its siblings through
// the edge, and one publishing from another address is sent its own object
// back, billed. Here the hosts publish to the relay instead (a bridge's
// -edge-ingress names it). The relay:
//
//   - takes each BRC-149 submission record a host sends, and applies the
//     edge's ingress bounds to it (object size and marker, per-source and
//     total rates), since its copy comes before the edge's checks;
//   - sends it up the tunnel from the consumer's own address, so the edge
//     publishes it to every other consumer and leaves it off this one's lane;
//   - copies it at once to the sibling hosts that asked for any of its
//     deliverable topics, as the delivery record the edge would have sent,
//     skipping the publisher by its source address.
//
// Nothing of a host's own comes back or is billed, and siblings receive it
// without the round trip through the edge. The relay remembers no object:
// the publisher is skipped by the submission's source, and nothing returns
// from the edge because the relay publishes from the consumer's address. A
// domain larger than one relay serves is several consumers, each with its
// own relay; relays never talk to each other.
//
// A record is copied only once the up-tunnel queue has taken it, so a host
// never receives what the edge will not carry for the others. Like the lane,
// the copies are at most once: a sibling that falls behind is shed by the
// distributor.
package publish

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lightwebinc/overlay-bridge/uptunnel"
	"github.com/lightwebinc/shard-common/interest"
	"github.com/lightwebinc/shard-common/objfmt"

	"github.com/lightwebinc/interest-relay/distributor"
)

// Defaults, as the edge applies them to a consumer tunnel (an authenticated
// source): shard-proxy's -beef-max-object-bytes-auth, -beef-max-topics and
// its per-source budget.
const (
	DefaultMaxObject     = 8 << 20
	DefaultObjectsPerSec = 2000
	DefaultBytesPerSec   = 80 << 20
	DefaultQueue         = 4096
	DefaultQueueBytes    = 256 << 20
	DefaultMaxConns      = 1024
	recordTimeout        = 2 * time.Minute
	idleTimeout          = 5 * time.Minute // a host's bridge redials on its next record
	maxTrackedSources    = 4096
	readBuffer           = 64 << 10
	readChunk            = 1 << 20
)

// Config configures a [Server].
type Config struct {
	// Listen is the TCP address hosts publish to, where their bridges'
	// -edge-ingress points.
	Listen string
	// Allow are the prefixes hosts may publish from; a connection from any
	// other address is closed. The relay's own -allow.
	Allow []netip.Prefix
	// Ingress are the edge's ingress addresses (host:port), in failover
	// order, side A then side B, as a bridge's -edge-ingress.
	Ingress []string
	// Source is the consumer's own address, the tunnel's inner address the
	// edge excludes from the consumer's lane. Required: from any other
	// address every host would be sent its own publications back.
	Source netip.Addr
	// MaxObject bounds one object. Zero is DefaultMaxObject.
	MaxObject int
	// DeliverTopics is how many of a record's leading topic names the edge
	// delivers it on. Zero is distributor.DefaultDeliverTopics.
	DeliverTopics int
	// SourceObjects and SourceBytes are one host's budget per second, and
	// TotalObjects and TotalBytes the relay's, which is the whole consumer's
	// at the edge. Zero is no limit.
	SourceObjects, SourceBytes float64
	TotalObjects, TotalBytes   float64
	// Queue and QueueBytes bound what waits to go up the tunnel; a record
	// that finds it full is refused and not copied. Zero uses the defaults.
	Queue      int
	QueueBytes int64
	// MaxConns bounds the hosts' connections. Zero is DefaultMaxConns.
	MaxConns int
	// Logger defaults to slog.Default.
	Logger *slog.Logger
}

// Upstream is where accepted records go: the up-tunnel queue.
type Upstream interface {
	Publish(ctx context.Context, record []byte, sent func()) error
}

// Server is a running publish point. Build one with [New].
type Server struct {
	cfg  Config
	log  *slog.Logger
	up   Upstream
	run  func(ctx context.Context) error // runs up; nil when up runs elsewhere
	stat func() uptunnel.QueueStats
	dist *distributor.Distributor

	mu      sync.Mutex
	sources map[netip.Addr]*budget
	total   budget

	conns     atomic.Int64
	recordsIn atomic.Uint64
	bytesIn   atomic.Uint64
	accepted  atomic.Uint64
	confirmed atomic.Uint64
	copies    atomic.Uint64
	refused   [nReasons]atomic.Uint64
}

// Reasons a record or a connection is refused.
const (
	ReasonSource    = iota // a connection from outside Allow
	ReasonGrammar          // a stream that is not submission records
	ReasonMalformed        // a record the grammar refuses; the connection is closed
	ReasonOversize         // an object over MaxObject; skipped
	ReasonNotBEEF          // an object without a BEEF marker; skipped
	ReasonRate             // over a host's or the relay's budget; skipped
	ReasonQueueFull        // the up-tunnel queue had no room; skipped
	ReasonConns            // over MaxConns
	nReasons
)

// ReasonNames are the reasons' labels, by index.
var ReasonNames = [nReasons]string{"source", "grammar", "malformed", "oversize", "not_beef", "rate", "queue_full", "conns"}

// New builds a publish point that sends up the tunnel through the bridge's
// up-tunnel client and copies through dist (nil: no copies, only the
// upward path).
func New(cfg Config, dist *distributor.Distributor) (*Server, error) {
	if !cfg.Source.IsValid() {
		return nil, errors.New("publish: the consumer's own address (Source) is required")
	}
	if len(cfg.Ingress) == 0 {
		return nil, errors.New("publish: the edge's ingress (Ingress) is required")
	}
	if len(cfg.Allow) == 0 {
		return nil, errors.New("publish: the prefixes hosts publish from (Allow) are required")
	}
	cfg = withDefaults(cfg)
	client := &uptunnel.Client{
		Addrs:     cfg.Ingress,
		LocalAddr: &net.TCPAddr{IP: cfg.Source.AsSlice(), Zone: cfg.Source.Zone()},
		Log:       cfg.Logger,
	}
	q := uptunnel.NewQueue(client, cfg.Queue, cfg.Logger)
	q.MaxBytes = cfg.QueueBytes
	s := newServer(cfg, q, dist)
	s.run = func(ctx context.Context) error {
		defer func() { _ = client.Close() }()
		return q.Run(ctx)
	}
	s.stat = q.Stats
	return s, nil
}

func withDefaults(cfg Config) Config {
	if cfg.MaxObject <= 0 {
		cfg.MaxObject = DefaultMaxObject
	}
	if cfg.DeliverTopics <= 0 {
		cfg.DeliverTopics = distributor.DefaultDeliverTopics
	}
	if cfg.Queue <= 0 {
		cfg.Queue = DefaultQueue
	}
	if cfg.QueueBytes <= 0 {
		cfg.QueueBytes = DefaultQueueBytes
	}
	if cfg.MaxConns <= 0 {
		cfg.MaxConns = DefaultMaxConns
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return cfg
}

func newServer(cfg Config, up Upstream, dist *distributor.Distributor) *Server {
	cfg = withDefaults(cfg)
	s := &Server{cfg: cfg, log: cfg.Logger, up: up, dist: dist, sources: map[netip.Addr]*budget{}}
	s.total = budget{objects: bucket{rate: cfg.TotalObjects}, bytes: bucket{rate: cfg.TotalBytes}}
	return s
}

// Serve listens on Config.Listen and takes the hosts' records until ctx is
// done.
func (s *Server) Serve(ctx context.Context) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", s.cfg.Listen)
	if err != nil {
		return err
	}
	return s.ServeListener(ctx, ln)
}

// ServeListener takes the hosts' records on ln until ctx is done, and runs
// the upward queue. It closes ln.
func (s *Server) ServeListener(ctx context.Context, ln net.Listener) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	errs := make(chan error, 1)
	if s.run != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.run(ctx); err != nil && ctx.Err() == nil {
				errs <- err
				cancel()
			}
		}()
	}
	stop := context.AfterFunc(ctx, func() { _ = ln.Close() })
	defer stop()
	defer func() { cancel(); wg.Wait() }()
	var delay time.Duration
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				select {
				case err := <-errs:
					return err
				default:
					return nil
				}
			}
			if errors.Is(err, net.ErrClosed) {
				return err
			}
			if delay == 0 {
				delay = 5 * time.Millisecond
			} else if delay *= 2; delay > time.Second {
				delay = time.Second
			}
			s.log.Warn("PUBLISH: accepting a host failed; trying again", "err", err, "retry_in", delay)
			select {
			case <-ctx.Done():
			case <-time.After(delay):
			}
			continue
		}
		delay = 0
		if s.conns.Load() >= int64(s.cfg.MaxConns) {
			s.refused[ReasonConns].Add(1)
			_ = c.Close()
			continue
		}
		s.conns.Add(1)
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer s.conns.Add(-1)
			s.serveConn(ctx, c)
		}()
	}
}

// serveConn reads one host's record stream until it closes.
func (s *Server) serveConn(ctx context.Context, c net.Conn) {
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()
	defer func() { _ = c.Close() }()
	src, ok := addrOf(c.RemoteAddr())
	if !ok || !s.allowed(src) {
		s.refused[ReasonSource].Add(1)
		s.log.Warn("PUBLISH: closing a connection from outside the allowed prefixes", "from", c.RemoteAddr().String())
		return
	}
	br := bufio.NewReaderSize(c, readBuffer)
	// The stream's grammar is decided by its first bytes, as at the edge's
	// ingress; only submission records are taken here.
	_ = c.SetReadDeadline(time.Now().Add(recordTimeout))
	if lead, err := br.Peek(2); err != nil {
		return
	} else if binary.BigEndian.Uint16(lead) != objfmt.BEEFRecordTag {
		s.refused[ReasonGrammar].Add(1)
		s.log.Warn("PUBLISH: closing a stream that is not BRC-149 submission records", "from", src.String())
		return
	}
	for {
		_ = c.SetReadDeadline(time.Now().Add(idleTimeout)) // a bridge's stream idles between records
		if _, err := br.Peek(1); err != nil {
			return
		}
		_ = c.SetReadDeadline(time.Now().Add(recordTimeout))
		rec, why, err := s.readRecord(br)
		if err != nil {
			if why == ReasonMalformed {
				s.refused[ReasonMalformed].Add(1)
				s.log.Warn("PUBLISH: closing a stream that sent a malformed record", "from", src.String(), "err", err)
			}
			return
		}
		if why >= 0 {
			s.refused[why].Add(1)
			continue
		}
		s.take(ctx, src, rec)
	}
}

// readRecord reads one submission record. A record the stream can continue
// past is refused with why >= 0 and a nil error; a malformed one ends the
// stream with why ReasonMalformed.
func (s *Server) readRecord(br *bufio.Reader) (rec []byte, why int, err error) {
	env := make([]byte, 4, 4+objfmt.BEEFMaxTopics*(1+objfmt.BEEFMaxTopicLen)+4)
	if _, err := io.ReadFull(br, env); err != nil {
		return nil, -1, err
	}
	if binary.BigEndian.Uint16(env[0:2]) != objfmt.BEEFRecordTag || env[2] != objfmt.BEEFRecordVer {
		return nil, ReasonMalformed, fmt.Errorf("record tag or version % x", env[0:3])
	}
	count := int(env[3])
	if count < 1 || count > objfmt.BEEFMaxTopics {
		return nil, ReasonMalformed, fmt.Errorf("topic count %d", count)
	}
	for range count {
		n, err := br.ReadByte()
		if err != nil {
			return nil, -1, err
		}
		if n < 1 || int(n) > objfmt.BEEFMaxTopicLen {
			return nil, ReasonMalformed, fmt.Errorf("topic length %d", n)
		}
		env = append(env, n)
		name := make([]byte, n)
		if _, err := io.ReadFull(br, name); err != nil {
			return nil, -1, err
		}
		env = append(env, name...)
	}
	var lenb [4]byte
	if _, err := io.ReadFull(br, lenb[:]); err != nil {
		return nil, -1, err
	}
	objLen := binary.BigEndian.Uint32(lenb[:])
	if objLen == 0 {
		return nil, ReasonMalformed, errors.New("zero object length")
	}
	env = append(env, lenb[:]...)
	if int64(objLen) > int64(s.cfg.MaxObject) {
		// Skipped whole, so the stream stays in step, as the edge does.
		if _, err := io.CopyN(io.Discard, br, int64(objLen)); err != nil {
			return nil, -1, err
		}
		return nil, ReasonOversize, nil
	}
	// Grown as the bytes arrive, so a header that names a large object and
	// a sender that then stalls hold no more memory than they sent.
	rec = append(make([]byte, 0, len(env)+min(int(objLen), readChunk)), env...)
	for left := int(objLen); left > 0; {
		n := min(left, readChunk)
		// Doubling, capped at the object, so a large object is copied a
		// few times at most as it grows, not once per chunk.
		rec = slices.Grow(rec, min(left, max(len(rec), n)))
		rec = append(rec, make([]byte, n)...)
		if _, err := io.ReadFull(br, rec[len(rec)-n:]); err != nil {
			return nil, -1, err
		}
		left -= n
	}
	if !objfmt.IsBEEFObject(rec[len(env):]) {
		return nil, ReasonNotBEEF, nil
	}
	return rec, -1, nil
}

// take sends one accepted record up the tunnel and copies it to the
// publisher's siblings.
func (s *Server) take(ctx context.Context, src netip.Addr, rec []byte) {
	s.recordsIn.Add(1)
	s.bytesIn.Add(uint64(len(rec)))
	if !s.spend(src, len(rec)) {
		s.refused[ReasonRate].Add(1)
		return
	}
	// The delivery record is built before the queue takes rec.
	var drec []byte
	var ids []interest.TopicID
	if s.dist != nil {
		r, _, err := objfmt.DecodeBEEFRecord(rec)
		if err != nil { // read by the same grammar; cannot happen
			s.refused[ReasonMalformed].Add(1)
			return
		}
		for _, n := range r.Topics[:min(s.cfg.DeliverTopics, len(r.Topics))] {
			ids = append(ids, interest.TopicID(objfmt.TopicID(n)))
		}
		drec = objfmt.EncodeBEEFDelivery(ids[0], rec)
	}
	if err := s.up.Publish(ctx, rec, func() { s.confirmed.Add(1) }); err != nil {
		s.refused[ReasonQueueFull].Add(1)
		return
	}
	s.accepted.Add(1)
	if s.dist != nil {
		s.copies.Add(uint64(s.dist.Copy(ids, drec, src)))
	}
}

func (s *Server) allowed(a netip.Addr) bool {
	for _, p := range s.cfg.Allow {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// spend takes one record of n bytes from the host's budget and the
// relay's, or neither.
func (s *Server) spend(src netip.Addr, n int) bool {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.sources[src]
	if b == nil {
		if len(s.sources) >= maxTrackedSources && !s.evictIdleLocked(now) {
			return false // every tracked source is spending: a flood of new ones waits
		}
		b = &budget{objects: bucket{rate: s.cfg.SourceObjects}, bytes: bucket{rate: s.cfg.SourceBytes}}
		s.sources[src] = b
	}
	if !b.can(n, now) || !s.total.can(n, now) {
		return false
	}
	b.take(n)
	s.total.take(n)
	return true
}

// evictIdleLocked forgets the sources whose budgets are full again, the
// same as a new source's (one that took an object larger than its rate is
// in debt until it has paid it back), and reports whether any went.
func (s *Server) evictIdleLocked(now time.Time) bool {
	gone := false
	for a, b := range s.sources {
		b.objects.fill(now)
		b.bytes.fill(now)
		if b.objects.full() && b.bytes.full() {
			delete(s.sources, a)
			gone = true
		}
	}
	return gone
}

// budget is a per-second allowance of objects and bytes.
type budget struct{ objects, bytes bucket }

func (b *budget) can(n int, now time.Time) bool {
	b.objects.fill(now)
	b.bytes.fill(now)
	return b.objects.has(1) && b.bytes.has(float64(n))
}

func (b *budget) take(n int) {
	b.objects.take(1)
	b.bytes.take(float64(n))
}

// bucket is a token bucket holding up to one second of its rate; a zero
// rate is no limit. An item larger than a second's allowance passes when the
// bucket is full, so the largest object is never refused outright.
type bucket struct {
	rate, tokens float64
	last         time.Time
}

func (b *bucket) fill(now time.Time) {
	if b.rate <= 0 {
		return
	}
	if b.last.IsZero() {
		b.tokens, b.last = b.rate, now
		return
	}
	b.tokens = min(b.rate, b.tokens+now.Sub(b.last).Seconds()*b.rate)
	b.last = now
}

func (b *bucket) has(n float64) bool { return b.rate <= 0 || b.tokens >= min(n, b.rate) }
func (b *bucket) full() bool         { return b.rate <= 0 || b.tokens >= b.rate }
func (b *bucket) take(n float64) {
	if b.rate > 0 {
		b.tokens -= n
	}
}

func addrOf(a net.Addr) (netip.Addr, bool) {
	ta, ok := a.(*net.TCPAddr)
	if !ok {
		return netip.Addr{}, false
	}
	ip, ok := netip.AddrFromSlice(ta.IP)
	return ip.Unmap(), ok
}

// Stats is the publish point's counters.
type Stats struct {
	Conns     int64
	RecordsIn uint64 // records read whole and within bounds of size and marker
	BytesIn   uint64
	Accepted  uint64 // taken by the up-tunnel queue
	Confirmed uint64 // acknowledged by the edge's ingress
	Copies    uint64 // copies offered to sibling hosts
	Refused   [nReasons]uint64
	Up        uptunnel.QueueStats
}

// Stats returns the counters.
func (s *Server) Stats() Stats {
	st := Stats{
		Conns: s.conns.Load(), RecordsIn: s.recordsIn.Load(), BytesIn: s.bytesIn.Load(),
		Accepted: s.accepted.Load(), Confirmed: s.confirmed.Load(), Copies: s.copies.Load(),
	}
	for i := range st.Refused {
		st.Refused[i] = s.refused[i].Load()
	}
	if s.stat != nil {
		st.Up = s.stat()
	}
	return st
}
