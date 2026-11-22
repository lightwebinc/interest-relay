// Package relay is a topic interest relay: it holds the union of the
// interest announced by the senders below it and announces that union to
// its own upstreams.
//
// A relay is what lets a host several network hops from the customer edge
// be heard. Hosts and lower relays are configured with the relay's address,
// in the manner of a default route. The relay is configured with its own
// upstreams: a higher relay, or the delivery edge at the far end of the
// consumer's tunnel. The same datagrams are used at every hop.
//
// The relay is control only. It carries no objects.
//
// The package is the daemon's core and is meant to be embedded: a process
// that already sits where a relay would (a bridge, a gateway, a router) can
// run one with [New] and [Relay.Serve] on its own sockets.
package relay

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lightwebinc/shard-common/interest"
)

// Principal is the one principal a relay holds interest for: everything
// below it. The relay does not tell its senders apart beyond their caps,
// because what goes upstream is their union.
const Principal = "downstream"

// Config configures a [Relay].
type Config struct {
	// Upstreams are where the union is announced. At least one. Every
	// upstream is told every topic it serves.
	Upstreams []netip.AddrPort
	// Allow lists the prefixes senders may announce from. A datagram from
	// anywhere else is ignored without an answer. An upstream's own
	// address is never accepted as a sender, whatever Allow says, so two
	// relays pointed at each other cannot feed each other.
	Allow []netip.Prefix
	// SenderID identifies this relay to its upstreams. It must be the same
	// after a restart; see [interest.NewSenderID].
	SenderID uint64
	// Lifetimes are enforced on the senders below. Zero uses the defaults.
	Lifetimes interest.Lifetimes
	// UpstreamLifetime is the lifetime asked of the upstreams, in seconds.
	// Zero asks for each upstream's default.
	UpstreamLifetime uint32
	// MaxTopicsPerSender, MaxSenders, ChangeRate, ChangeBurst and MaxHops
	// are the receiver's limits; zero is no limit (MaxHops: the default).
	MaxTopicsPerSender int
	MaxSenders         int
	ChangeRate         float64
	ChangeBurst        int
	MaxHops            uint8
	// RestartHold is how long after starting the relay waits before it
	// lets its upstreams drop what its previous run announced and it has
	// not heard again. It should be at least the interval at which the
	// senders below refresh, a third of the default lifetime. Negative
	// uses exactly that, plus a margin. Zero drops at once.
	RestartHold time.Duration
	// WithdrawOnExit withdraws everything from the upstreams when the
	// relay stops. Leave it off for a relay that will be started again.
	WithdrawOnExit bool
	// Logger defaults to slog.Default.
	Logger *slog.Logger
}

// Relay is a running relay's state. Build one with [New].
type Relay struct {
	cfg       Config
	log       *slog.Logger
	rcv       *interest.Receiver
	cli       *interest.Client
	scope     atomic.Pointer[interest.Scope]
	upstreams map[netip.Addr]struct{}

	scopeCh chan struct{}
	mu      sync.Mutex
	pokes   map[interest.TopicID]struct{}
	pokeCh  chan struct{}
	hops    atomic.Uint32
	started time.Time
}

// New builds a relay. It does not open sockets or start goroutines.
func New(cfg Config) (*Relay, error) {
	if len(cfg.Upstreams) == 0 {
		return nil, errors.New("relay: no upstream configured")
	}
	if len(cfg.Allow) == 0 {
		return nil, errors.New("relay: no sender prefix allowed; nothing could announce")
	}
	if cfg.SenderID == 0 {
		return nil, errors.New("relay: no sender identity")
	}
	if cfg.Lifetimes == (interest.Lifetimes{}) {
		cfg.Lifetimes = interest.DefaultLifetimes
	}
	if cfg.Lifetimes.Min == 0 || cfg.Lifetimes.Min > cfg.Lifetimes.Default || cfg.Lifetimes.Default > cfg.Lifetimes.Max {
		return nil, fmt.Errorf("relay: lifetimes must satisfy 0 < min <= default <= max, got %+v", cfg.Lifetimes)
	}
	if cfg.RestartHold < 0 {
		cfg.RestartHold = time.Duration(cfg.Lifetimes.Default)*time.Second/3 + 5*time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	r := &Relay{
		cfg:       cfg,
		log:       cfg.Logger,
		upstreams: make(map[netip.Addr]struct{}, len(cfg.Upstreams)),
		scopeCh:   make(chan struct{}, 1),
		pokes:     make(map[interest.TopicID]struct{}),
		pokeCh:    make(chan struct{}, 1),
	}
	for i, u := range cfg.Upstreams {
		if !u.IsValid() || u.Port() == 0 {
			return nil, fmt.Errorf("relay: upstream %d is not an address and port", i)
		}
		r.upstreams[u.Addr().Unmap()] = struct{}{}
	}
	r.cli = interest.NewClient(interest.ClientConfig{
		SenderID:  cfg.SenderID,
		Upstreams: cfg.Upstreams,
		Lifetime:  cfg.UpstreamLifetime,
		SweepHold: cfg.RestartHold,
		OnScope: func() {
			select {
			case r.scopeCh <- struct{}{}:
			default:
			}
		},
		OnStatus: r.onStatus,
	})
	r.cli.SetHops(1)
	r.hops.Store(1)
	r.rcv = interest.NewReceiver(interest.ReceiverConfig{
		Policy:             (*policy)(r),
		Lifetimes:          cfg.Lifetimes,
		MaxHops:            cfg.MaxHops,
		MaxTopicsPerSender: cfg.MaxTopicsPerSender,
		MaxSenders:         cfg.MaxSenders,
		ChangeRate:         cfg.ChangeRate,
		ChangeBurst:        cfg.ChangeBurst,
		OnChange: func(_ string, t interest.TopicID, live bool) {
			// The union changed: say so upstream. A delivery address is
			// meaningful only to the next hop, and this relay does not
			// distribute, so none goes up.
			if live {
				r.cli.Add(t)
			} else {
				r.cli.Remove(t)
			}
		},
	})
	return r, nil
}

// Receiver returns the downstream half: who announces what. It is the
// relay's readable table, for a component in the same process that needs to
// know what each sender below wants.
func (r *Relay) Receiver() *interest.Receiver { return r.rcv }

// Client returns the upstream half: what the relay announces, and how each
// upstream answered.
func (r *Relay) Client() *interest.Client { return r.cli }

// Hops returns the hop count the relay announces with.
func (r *Relay) Hops() uint8 { return uint8(r.hops.Load()) }

// Started returns when [Relay.Serve] began, or the zero time.
func (r *Relay) Started() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.started
}

// Ready reports whether at least one upstream is reachable and holds
// everything the relay wants it to. During the hold after a start the
// upstream may hold more than that, which does not make the relay unready.
func (r *Relay) Ready() bool {
	for _, u := range r.cli.Upstreams() {
		if u.Reachable && (u.Synced || u.Holding) {
			return true
		}
	}
	return false
}

// onStatus is called by the client when the outcome for a topic changes.
// When a topic turns out not to be served, the senders that hold it are
// poked so that they hear of it now.
func (r *Relay) onStatus(t interest.TopicID, st interest.Status) {
	if st.Served() || st == interest.StatusPending {
		return
	}
	r.mu.Lock()
	r.pokes[t] = struct{}{}
	r.mu.Unlock()
	select {
	case r.pokeCh <- struct{}{}:
	default:
	}
}

// Serve runs the relay on the given sockets until ctx is done: down is
// where senders announce to, up is an unconnected socket the relay
// announces from. It does not close them.
func (r *Relay) Serve(ctx context.Context, down, up *net.UDPConn) error {
	r.mu.Lock()
	r.started = time.Now()
	r.mu.Unlock()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		errs <- interest.ServeReceiver(ctx, down, r.rcv, interest.ServeOptions{
			// An upstream that restarted may poke the relay at the
			// address it knows, which is this socket.
			Resync: func(src netip.AddrPort) {
				if _, ok := r.upstreams[src.Addr().Unmap()]; ok {
					r.cli.Poke(src, time.Now())
				}
			},
			OnError: func(err error) { r.log.Debug("downstream socket", "err", err) },
		})
	}()
	go func() {
		defer wg.Done()
		errs <- interest.RunClient(ctx, up, r.cli, interest.RunOptions{
			WithdrawOnExit: r.cfg.WithdrawOnExit,
			OnError:        func(err error) { r.log.Debug("upstream socket", "err", err) },
		})
	}()

	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	var first error
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case err := <-errs:
			if err != nil && first == nil {
				first = err
			}
			break loop
		case <-r.scopeCh:
			// What the upstreams deliver changed. Drop what is no longer
			// served, and have every sender check in so that it learns the
			// new scope.
			sc := r.cli.Scope()
			r.scope.Store(sc)
			r.rcv.Reevaluate(Principal, time.Now())
			r.rcv.PokeAll()
			if sc == nil {
				r.log.Info("upstream scope unknown; every topic is passed on")
			} else {
				r.log.Info("upstream scope", "domain", sc.Domain(), "shard_bits", sc.ShardBits(),
					"all", sc.All(), "groups", len(sc.Groups()))
			}
		case <-r.pokeCh:
			r.mu.Lock()
			ts := r.pokes
			r.pokes = make(map[interest.TopicID]struct{})
			r.mu.Unlock()
			for t := range ts {
				r.rcv.PokeHolders(t)
			}
		case <-tick.C:
			// One more than the deepest sender below. Relays pointed at
			// each other count upward until the limit refuses them.
			h := uint32(r.rcv.MaxSenderHops()) + 1
			if h > 255 {
				h = 255
			}
			if r.hops.Swap(h) != h {
				r.cli.SetHops(uint8(h))
			}
		}
	}
	cancel()
	wg.Wait()
	if first != nil {
		return first
	}
	// Drain the second runner's result.
	select {
	case err := <-errs:
		return err
	default:
		return nil
	}
}

// policy is the relay's answers to its receiver.
type policy Relay

func (p *policy) Identify(src netip.AddrPort) (string, bool) {
	a := src.Addr().Unmap()
	if _, ok := p.upstreams[a]; ok {
		return "", false
	}
	for _, pfx := range p.cfg.Allow {
		if pfx.Contains(a) {
			return Principal, true
		}
	}
	return "", false
}

// Scope is what the upstreams deliver between them. A topic outside it has
// no stream behind it, so the relay refuses it as its upstream would.
func (p *policy) Scope(string) *interest.Scope { return p.scope.Load() }

func (p *policy) Bound(string, interest.TopicID) interest.Bound { return interest.BoundFree }

func (p *policy) Quota(string) int { return -1 }

// Note tells a sender that a topic the relay holds for it is not served
// upstream. The relay keeps holding it and keeps asking, so the sender has
// nothing to do; it only knows not to wait for a stream.
func (p *policy) Note(_ string, t interest.TopicID) interest.Status {
	return downstreamNote(p.cli.UnservedStatus(t))
}

func (p *policy) Notes(_ string, each func(interest.TopicID, interest.Status) bool) {
	p.cli.Unserved(func(t interest.TopicID, st interest.Status) bool {
		return each(t, downstreamNote(st))
	})
}

// downstreamNote turns the reason no upstream serves a topic into the note
// a sender below is given.
func downstreamNote(st interest.Status) interest.Status {
	switch st {
	case interest.StatusOK, interest.StatusPending, interest.StatusAddressNotHonored:
		return interest.StatusOK
	case interest.StatusNotServed, interest.StatusUnserved:
		return interest.StatusUnserved
	}
	return interest.StatusUpstreamRefused
}
