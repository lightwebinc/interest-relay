// Package metrics exports the relay's state to Prometheus and serves the
// health endpoints. Every value is read from the relay at scrape time, so
// a series exists from the first scrape, at zero, whether or not anything
// has happened yet.
package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/lightwebinc/shard-common/interest"
	"github.com/lightwebinc/shard-common/logging"

	"github.com/lightwebinc/interest-relay/publish"
	"github.com/lightwebinc/interest-relay/relay"
)

// ServiceName is the service.name of the daemon in logs and metrics.
const ServiceName = "interest-relay"

// Version is the build version, set by the daemon at start.
var Version = "dev"

type collector struct {
	r *relay.Relay

	buildInfo, datagrams, ignored, tokens, messages, changes, refused, resyncs *prometheus.Desc
	senders, held, live, wanted, unserved, hops, ready                         *prometheus.Desc
	upReachable, upSynced, upHolding, upHeld, upTracked, upInFlight            *prometheus.Desc
	upSent, upAcks, upEvents                                                   *prometheus.Desc
	// The distributor's, exported only when the relay distributes.
	deliveries, dLanes, dRecordsIn, dBytesIn, dUnrouted, dMalformed, dRoutes    *prometheus.Desc
	hConnected, hTopics, hSent, hSentBytes, hShed, hDials, hDialErrors, hQueued *prometheus.Desc
}

// NewCollector returns a collector that reads r at scrape time.
func NewCollector(r *relay.Relay) prometheus.Collector {
	d := func(name, help string, labels ...string) *prometheus.Desc {
		return prometheus.NewDesc("bir_"+name, help, labels, nil)
	}
	return &collector{
		r:         r,
		buildInfo: d("build_info", "Constant 1, labelled with the build version.", "version"),
		datagrams: d("datagrams_total", "Datagrams received on the downstream socket, whatever became of them."),
		ignored: d("datagrams_ignored_total",
			"Datagrams that were not processed: malformed (not an interest message, or inconsistent), unidentified (from a source no -allow prefix covers, or from an upstream), version (another wire version).",
			"reason"),
		tokens: d("tokens_issued_total", "Messages answered with a token instead of being applied: first contact from a sender, or its first message after this relay restarted or its address changed."),
		messages: d("messages_total",
			"Messages from senders that held a valid token, by outcome: applied (an ANNOUNCE), matched (a DIGEST equal to the set held, which refreshes it), mismatch (a DIGEST that differed, which makes the sender replay), stale (sequence number not above the last applied), busy (a sender cap was reached), hop_limit (crossed too many relays, which usually means a loop).",
			"outcome"),
		changes: d("topic_changes_total",
			"Changes to the topics held for senders: add, refresh (announced again while held), withdraw, expire (not refreshed within its lifetime), sweep (removed at the end of a sender's replay), reevaluate (dropped because the upstream scope changed).",
			"change"),
		refused:     d("topics_refused_total", "Records refused, by reason. The topic is not held.", "reason"),
		resyncs:     d("resyncs_queued_total", "RESYNC pokes queued for senders, to make them check in before their next refresh."),
		senders:     d("senders", "Senders tracked now."),
		held:        d("topics_held", "Topics held now, summed over senders. A topic two senders announce counts twice."),
		live:        d("topics_live", "Distinct topics held now: the union announced upstream."),
		wanted:      d("topics_wanted", "Topics the relay is asking its upstreams for now."),
		unserved:    d("topics_unserved", "Wanted topics that no upstream serves now: outside every upstream's delivered groups, or refused."),
		hops:        d("hops", "Hop count the relay announces with: one more than the deepest sender below it."),
		ready:       d("ready", "1 when at least one upstream is reachable and holds everything the relay wants. During the hold after a start the upstream may hold more."),
		upReachable: d("upstream_reachable", "1 when the upstream has answered since the last three consecutive losses.", "upstream"),
		upSynced:    d("upstream_synced", "1 when the upstream holds exactly what the relay wants it to and no replay is pending. 0 during the hold after a start, when the upstream is allowed to hold more.", "upstream"),
		upHolding:   d("upstream_holding", "1 during the hold after a start: everything wanted has been said and answered, and the upstream still keeps what the previous run announced.", "upstream"),
		upHeld:      d("upstream_topics_held", "Topics the upstream holds for this relay.", "upstream"),
		upTracked:   d("upstream_topics_tracked", "Topics the relay keeps the upstream told about. Fewer than wanted when the upstream delivers only some groups.", "upstream"),
		upInFlight:  d("upstream_in_flight", "Datagrams sent to the upstream and not yet answered.", "upstream"),
		upSent:      d("upstream_datagrams_sent_total", "Datagrams sent to the upstream, by type.", "upstream", "type"),
		upAcks:      d("upstream_acks_total", "Answers from the upstream that matched a datagram in flight.", "upstream"),
		upEvents: d("upstream_events_total",
			"Things that went other than plainly: timeout (a datagram went unanswered), sync (a full replay started), token (a new token adopted: first contact or the upstream restarted), stale (the upstream had already applied a later datagram), mismatch (the upstream's set differed from the one expected), busy (the upstream refused for now), poke (the upstream asked for a digest), ignored (an answer that matched nothing in flight).",
			"upstream", "event"),
		deliveries: d("delivery_addresses", "Delivery addresses honored now, (topic, host) pairs held by at least one sender. The distributor's routes."),
		dLanes:     d("distributor_lanes", "Lane connections open now: the edge, or a distributing relay above, delivering to -distribute-listen."),
		dRecordsIn: d("distributor_records_in_total", "Delivery records read from the lane."),
		dBytesIn:   d("distributor_bytes_in_total", "Bytes of delivery records read from the lane."),
		dUnrouted:  d("distributor_records_unrouted_total", "Records whose topic no host below asked for at an address."),
		dMalformed: d("distributor_lanes_malformed_total", "Lanes closed because a record's length was zero or over the bound."),
		dRoutes:    d("distributor_routes", "(topic, host) routes now."),
		hConnected: d("distributor_host_connected", "1 when the stream to the host is open.", "host"),
		hTopics:    d("distributor_host_topics", "Topics routed to the host.", "host"),
		hSent:      d("distributor_host_records_sent_total", "Records written to the host.", "host"),
		hSentBytes: d("distributor_host_bytes_sent_total", "Bytes written to the host.", "host"),
		hShed: d("distributor_host_records_shed_total",
			"Records the host did not get: its queue was full, it could not be reached, or its stream failed. Delivery is at most once, as on the lane.", "host"),
		hDials:      d("distributor_host_dials_total", "Connection attempts to the host.", "host"),
		hDialErrors: d("distributor_host_dial_errors_total", "Connection attempts to the host that failed.", "host"),
		hQueued:     d("distributor_host_queued_bytes", "Bytes waiting for the host now.", "host"),
	}
}

func (c *collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{
		c.buildInfo, c.datagrams, c.ignored, c.tokens, c.messages, c.changes, c.refused, c.resyncs,
		c.senders, c.held, c.live, c.wanted, c.unserved, c.hops, c.ready,
		c.upReachable, c.upSynced, c.upHolding, c.upHeld, c.upTracked, c.upInFlight, c.upSent, c.upAcks, c.upEvents,
		c.deliveries, c.dLanes, c.dRecordsIn, c.dBytesIn, c.dUnrouted, c.dMalformed, c.dRoutes,
		c.hConnected, c.hTopics, c.hSent, c.hSentBytes, c.hShed, c.hDials, c.hDialErrors, c.hQueued,
	} {
		ch <- d
	}
}

// refusalReasons are exported at zero so that a rate over them is defined
// before the first refusal.
var refusalReasons = []interest.Status{
	interest.StatusNotServed, interest.StatusOutsideBound, interest.StatusOverCap,
	interest.StatusRateLimited, interest.StatusUnsupported, interest.StatusBadAddress,
}

func (c *collector) Collect(ch chan<- prometheus.Metric) {
	counter := func(d *prometheus.Desc, v uint64, labels ...string) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.CounterValue, float64(v), labels...)
	}
	gauge := func(d *prometheus.Desc, v float64, labels ...string) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, v, labels...)
	}
	flag := func(b bool) float64 {
		if b {
			return 1
		}
		return 0
	}

	gauge(c.buildInfo, 1, Version)
	st := c.r.Receiver().Stats()
	if dist := c.r.Distributor(); dist != nil {
		ds := dist.Stats()
		gauge(c.deliveries, float64(st.Deliveries))
		gauge(c.dLanes, float64(ds.Lanes))
		counter(c.dRecordsIn, ds.RecordsIn)
		counter(c.dBytesIn, ds.BytesIn)
		counter(c.dUnrouted, ds.Unrouted)
		counter(c.dMalformed, ds.Malformed)
		gauge(c.dRoutes, float64(ds.Routes))
		for _, h := range ds.Hosts {
			a := h.Addr.String()
			gauge(c.hConnected, flag(h.Connected), a)
			gauge(c.hTopics, float64(h.Topics), a)
			counter(c.hSent, h.Sent, a)
			counter(c.hSentBytes, h.SentBytes, a)
			counter(c.hShed, h.Shed, a)
			counter(c.hDials, h.Dials, a)
			counter(c.hDialErrors, h.DialErrors, a)
			gauge(c.hQueued, float64(h.Queued), a)
		}
	}
	counter(c.datagrams, st.Datagrams)
	counter(c.ignored, st.Malformed, "malformed")
	counter(c.ignored, st.Unidentified, "unidentified")
	counter(c.ignored, st.WrongVersion, "version")
	counter(c.tokens, st.TokensIssued)
	counter(c.messages, st.Announces, "applied")
	counter(c.messages, st.Digests, "matched")
	counter(c.messages, st.Mismatches, "mismatch")
	counter(c.messages, st.Stale, "stale")
	counter(c.messages, st.Busy, "busy")
	counter(c.messages, st.HopLimited, "hop_limit")
	counter(c.changes, st.Adds, "add")
	counter(c.changes, st.Refreshes, "refresh")
	counter(c.changes, st.Withdrawals, "withdraw")
	counter(c.changes, st.Expiries, "expire")
	counter(c.changes, st.Sweeps, "sweep")
	counter(c.changes, st.Reevaluated, "reevaluate")
	seen := map[interest.Status]bool{}
	for _, s := range refusalReasons {
		counter(c.refused, st.Refused[s], s.String())
		seen[s] = true
	}
	for s, n := range st.Refused {
		if !seen[s] {
			counter(c.refused, n, s.String())
		}
	}
	counter(c.resyncs, st.ResyncsQueued)
	gauge(c.senders, float64(st.Senders))
	gauge(c.held, float64(st.Topics))
	gauge(c.live, float64(st.LiveTopics))

	cli := c.r.Client()
	gauge(c.wanted, float64(cli.Wanted()))
	unserved := 0
	cli.Unserved(func(interest.TopicID, interest.Status) bool { unserved++; return true })
	gauge(c.unserved, float64(unserved))
	gauge(c.hops, float64(c.r.Hops()))
	gauge(c.ready, flag(c.r.Ready()))
	for _, u := range cli.Upstreams() {
		a := u.Addr.String()
		gauge(c.upReachable, flag(u.Reachable), a)
		gauge(c.upSynced, flag(u.Synced), a)
		gauge(c.upHolding, flag(u.Holding), a)
		gauge(c.upHeld, float64(u.Held), a)
		gauge(c.upTracked, float64(u.Tracked), a)
		gauge(c.upInFlight, float64(u.InFlight), a)
		counter(c.upSent, u.Stats.Announces, a, "announce")
		counter(c.upSent, u.Stats.Digests, a, "digest")
		counter(c.upAcks, u.Stats.Acks, a)
		counter(c.upEvents, u.Stats.Timeouts, a, "timeout")
		counter(c.upEvents, u.Stats.Syncs, a, "sync")
		counter(c.upEvents, u.Stats.Tokens, a, "token")
		counter(c.upEvents, u.Stats.Stale, a, "stale")
		counter(c.upEvents, u.Stats.Mismatches, a, "mismatch")
		counter(c.upEvents, u.Stats.Busy, a, "busy")
		counter(c.upEvents, u.Stats.Pokes, a, "poke")
		counter(c.upEvents, u.Stats.Ignored, a, "ignored")
	}
}

// State is the relay's readable table: who announces, and how each
// upstream stands. It is what /state serves.
type State struct {
	Version   string     `json:"version"`
	Started   time.Time  `json:"started"`
	Ready     bool       `json:"ready"`
	Hops      uint8      `json:"hops"`
	Live      int        `json:"topics_live"`
	Wanted    int        `json:"topics_wanted"`
	Senders   []Sender   `json:"senders"`
	Upstreams []Upstream `json:"upstreams"`
}

// Sender is one sender below the relay.
type Sender struct {
	Addr      string    `json:"addr"`
	SenderID  string    `json:"sender_id"`
	Topics    int       `json:"topics"`
	Hops      uint8     `json:"hops"`
	LastHeard time.Time `json:"last_heard"`
}

// Upstream is one upstream of the relay.
type Upstream struct {
	Addr      string `json:"addr"`
	Reachable bool   `json:"reachable"`
	Synced    bool   `json:"synced"`
	Holding   bool   `json:"holding"`
	Held      int    `json:"topics_held"`
	Tracked   int    `json:"topics_tracked"`
	// Scope is "unknown", "all", or the number of groups delivered.
	Scope     string `json:"scope"`
	ShardBits *uint8 `json:"shard_bits,omitempty"`
}

// Snapshot reads the relay's state.
func Snapshot(r *relay.Relay) State {
	st := State{
		Version: Version, Started: r.Started(), Ready: r.Ready(), Hops: r.Hops(),
		Live: r.Receiver().Stats().LiveTopics, Wanted: r.Client().Wanted(),
		Senders: []Sender{}, Upstreams: []Upstream{},
	}
	for _, s := range r.Receiver().Senders() {
		st.Senders = append(st.Senders, Sender{
			Addr: s.Addr.String(), SenderID: formatID(s.SenderID), Topics: s.Topics, Hops: s.Hops, LastHeard: s.LastHeard,
		})
	}
	for _, u := range r.Client().Upstreams() {
		up := Upstream{Addr: u.Addr.String(), Reachable: u.Reachable, Synced: u.Synced, Holding: u.Holding, Held: u.Held, Tracked: u.Tracked, Scope: "unknown"}
		if u.Scope != nil {
			bits := u.Scope.ShardBits()
			up.ShardBits = &bits
			if u.Scope.All() {
				up.Scope = "all"
			} else {
				up.Scope = formatCount(len(u.Scope.Groups()))
			}
		}
		st.Upstreams = append(st.Upstreams, up)
	}
	return st
}

func formatID(id uint64) string {
	const hexdigits = "0123456789abcdef"
	var b [16]byte
	for i := 15; i >= 0; i-- {
		b[i] = hexdigits[id&0xF]
		id >>= 4
	}
	return string(b[:])
}

func formatCount(n int) string {
	if n == 1 {
		return "1 group"
	}
	b, _ := json.Marshal(n)
	return string(b) + " groups"
}

// Handler returns the metrics and health endpoints:
//
//	/metrics   Prometheus exposition
//	/healthz   200 while the process runs
//	/readyz    200 when an upstream is reachable and holds what is wanted, else 503
//	/state     the relay's table as JSON
//	/loglevel  read or set the log level
func Handler(r *relay.Relay, level *slog.LevelVar) http.Handler { return HandlerWith(r, nil, level) }

// HandlerWith is Handler for a relay that also runs a publish point (nil:
// none), whose counters it adds to /metrics.
func HandlerWith(r *relay.Relay, pub *publish.Server, level *slog.LevelVar) http.Handler {
	reg := prometheus.NewRegistry()
	reg.MustRegister(NewCollector(r), collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	if pub != nil {
		reg.MustRegister(NewPublishCollector(pub))
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !r.Ready() {
			http.Error(w, "no upstream is reachable and holding what is wanted", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/state", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(Snapshot(r))
	})
	if level != nil {
		mux.Handle("/loglevel", logging.LevelHandler(level))
	}
	return mux
}

// Serve runs the endpoints on addr until ctx is done.
func Serve(ctx context.Context, addr string, h http.Handler) error {
	srv := &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 5 * time.Second}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = srv.Shutdown(sctx)
		case <-done:
		}
	}()
	err := srv.ListenAndServe()
	close(done)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// publishCollector reads a publish point at scrape time.
type publishCollector struct {
	p                                                    *publish.Server
	conns, records, bytes, accepted, confirmed, copies   *prometheus.Desc
	refused, upDepth, upShed, upFailed, upResent, upLost *prometheus.Desc
}

// NewPublishCollector returns a collector for the publish point.
func NewPublishCollector(p *publish.Server) prometheus.Collector {
	d := func(name, help string, labels ...string) *prometheus.Desc {
		return prometheus.NewDesc("bir_publish_"+name, help, labels, nil)
	}
	return &publishCollector{
		p:         p,
		conns:     d("connections", "Hosts' publishing connections open now."),
		records:   d("records_total", "Submission records read whole, within the object bound and with a BEEF marker."),
		bytes:     d("bytes_total", "Bytes of those records."),
		accepted:  d("accepted_total", "Records the up-tunnel queue took; only these are copied to siblings."),
		confirmed: d("confirmed_total", "Records the edge's ingress acknowledged."),
		copies:    d("copies_total", "Copies offered to sibling hosts (a record to three siblings is three)."),
		refused: d("refused_total",
			"Records or connections refused: source (a host outside -allow), grammar (not a submission-record stream), malformed (closed), oversize, not_beef, rate (over a host's or the relay's budget), queue_full (the up-tunnel queue had no room), conns.",
			"reason"),
		upDepth:  d("up_queue_depth", "Records waiting to go up the tunnel."),
		upShed:   d("up_shed_total", "Records the up-tunnel queue shed."),
		upFailed: d("up_failed_total", "Records the up-tunnel client failed to send."),
		upResent: d("up_resent_total", "Records sent again on a new connection because the last one died unconfirmed."),
		upLost:   d("up_unconfirmed_total", "Records given up on after their resends."),
	}
}

func (c *publishCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{c.conns, c.records, c.bytes, c.accepted, c.confirmed, c.copies, c.refused,
		c.upDepth, c.upShed, c.upFailed, c.upResent, c.upLost} {
		ch <- d
	}
}

func (c *publishCollector) Collect(ch chan<- prometheus.Metric) {
	st := c.p.Stats()
	counter := func(d *prometheus.Desc, v uint64, labels ...string) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.CounterValue, float64(v), labels...)
	}
	ch <- prometheus.MustNewConstMetric(c.conns, prometheus.GaugeValue, float64(st.Conns))
	counter(c.records, st.RecordsIn)
	counter(c.bytes, st.BytesIn)
	counter(c.accepted, st.Accepted)
	counter(c.confirmed, st.Confirmed)
	counter(c.copies, st.Copies)
	for i, name := range publish.ReasonNames {
		counter(c.refused, st.Refused[i], name)
	}
	ch <- prometheus.MustNewConstMetric(c.upDepth, prometheus.GaugeValue, float64(st.Up.Depth))
	counter(c.upShed, st.Up.Shed)
	counter(c.upFailed, st.Up.Failed)
	counter(c.upResent, st.Up.Resent)
	counter(c.upLost, st.Up.Unconfirmed)
}
