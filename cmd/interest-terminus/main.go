// Command interest-terminus is a stand-in for a delivery edge, for testing
// relays and announcers without one. It accepts interest, keeps it as soft
// state, and shows what it holds. It delivers nothing.
//
// It can be given a delivered scope and a bound, to exercise the answers a
// real edge gives: not served here, outside the bound, over the cap.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/lightwebinc/shard-common/interest"
	"github.com/lightwebinc/shard-common/shard"
)

type policy struct {
	allow  []netip.Prefix
	scope  *interest.Scope
	listed map[interest.TopicID]bool
	quota  int
}

func (p *policy) Identify(src netip.AddrPort) (string, bool) {
	a := src.Addr().Unmap()
	for _, pfx := range p.allow {
		if pfx.Contains(a) {
			return "consumer", true
		}
	}
	return "", false
}
func (p *policy) Scope(string) *interest.Scope { return p.scope }
func (p *policy) Bound(_ string, t interest.TopicID) interest.Bound {
	if p.listed == nil || p.listed[t] {
		return interest.BoundFree
	}
	return interest.BoundQuota
}
func (p *policy) Quota(string) int { return p.quota }

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "interest-terminus:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		listen   = flag.String("listen", fmt.Sprintf("[::]:%d", interest.DefaultPort), "address and port to accept interest on")
		allow    = flag.String("allow", "::/0,0.0.0.0/0", "comma-separated prefixes that may announce")
		bits     = flag.Int("shard-bits", -1, "object plane width to advertise as the delivered scope; -1 advertises none")
		groups   = flag.String("groups", "", "comma-separated shard indexes delivered (0 .. 2^bits-1); empty with -shard-bits set means all")
		listed   = flag.String("listed", "", "comma-separated topic names that are in the bound; empty means every topic is")
		quota    = flag.Int("quota", 0, "topics outside -listed that may be live at once; needs -listed")
		shortest = flag.Duration("lifetime-min", 30*time.Second, "shortest lifetime granted")
		standard = flag.Duration("lifetime-default", 300*time.Second, "default lifetime granted")
		longest  = flag.Duration("lifetime-max", 3600*time.Second, "longest lifetime granted")
		httpAddr = flag.String("http", "", "address to serve /state on; empty disables it")
		quiet    = flag.Bool("quiet", false, "do not print each change")
	)
	flag.Parse()

	pol := &policy{quota: *quota}
	for _, f := range strings.Split(*allow, ",") {
		if f = strings.TrimSpace(f); f != "" {
			p, err := netip.ParsePrefix(f)
			if err != nil {
				return fmt.Errorf("-allow %q: %w", f, err)
			}
			pol.allow = append(pol.allow, p)
		}
	}
	if *bits >= 0 {
		var err error
		if *groups == "" {
			pol.scope, err = interest.AllScope(shard.DomainBEEF, uint8(*bits), [16]byte{})
		} else {
			var gs []uint16
			for _, f := range strings.Split(*groups, ",") {
				n, perr := strconv.ParseUint(strings.TrimSpace(f), 0, 16)
				if perr != nil {
					return fmt.Errorf("-groups %q: %w", f, perr)
				}
				gs = append(gs, shard.PlaneBase(shard.DomainBEEF)+uint16(n))
			}
			pol.scope, err = interest.NewScope(shard.DomainBEEF, uint8(*bits), [16]byte{}, gs)
		}
		if err != nil {
			return err
		}
	}
	if *listed != "" {
		pol.listed = map[interest.TopicID]bool{}
		for _, f := range strings.Split(*listed, ",") {
			if f = strings.TrimSpace(f); f != "" {
				pol.listed[interest.TopicID(sha256.Sum256([]byte(f)))] = true
			}
		}
	}

	var mu sync.Mutex
	changes := 0
	rcv := interest.NewReceiver(interest.ReceiverConfig{
		Policy: pol,
		Lifetimes: interest.Lifetimes{
			Min: uint32(*shortest / time.Second), Default: uint32(*standard / time.Second), Max: uint32(*longest / time.Second),
		},
		OnChange: func(_ string, t interest.TopicID, live bool) {
			mu.Lock()
			changes++
			mu.Unlock()
			if !*quiet {
				verb := "live"
				if !live {
					verb = "gone"
				}
				fmt.Printf("%s %x\n", verb, t[:8])
			}
		},
	})

	addr, err := netip.ParseAddrPort(*listen)
	if err != nil {
		return fmt.Errorf("-listen %q: %w", *listen, err)
	}
	conn, err := net.ListenUDP("udp", net.UDPAddrFromAddrPort(addr))
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if *httpAddr != "" {
		mux := http.NewServeMux()
		mux.HandleFunc("/state", func(w http.ResponseWriter, _ *http.Request) {
			st := rcv.Stats()
			type sender struct {
				Addr   string `json:"addr"`
				Topics int    `json:"topics"`
				Hops   uint8  `json:"hops"`
			}
			out := struct {
				Live     int               `json:"live"`
				Held     int               `json:"held"`
				Senders  []sender          `json:"senders"`
				Refused  map[string]uint64 `json:"refused"`
				Expiries uint64            `json:"expiries"`
				Sweeps   uint64            `json:"sweeps"`
				Changes  int               `json:"changes"`
			}{Live: st.LiveTopics, Held: st.Topics, Senders: []sender{}, Refused: map[string]uint64{}, Expiries: st.Expiries, Sweeps: st.Sweeps}
			for _, s := range rcv.Senders() {
				out.Senders = append(out.Senders, sender{Addr: s.Addr.String(), Topics: s.Topics, Hops: s.Hops})
			}
			for k, v := range st.Refused {
				out.Refused[k.String()] = v
			}
			mu.Lock()
			out.Changes = changes
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(out)
		})
		srv := &http.Server{Addr: *httpAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
		go func() {
			<-ctx.Done()
			_ = srv.Close()
		}()
		go func() {
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				fmt.Fprintln(os.Stderr, "interest-terminus: http:", err)
			}
		}()
	}

	fmt.Printf("# accepting interest on %s\n", conn.LocalAddr())
	return interest.ServeReceiver(ctx, conn, rcv, interest.ServeOptions{})
}
