package relay

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/lightwebinc/interest-relay/distributor"
	"github.com/lightwebinc/shard-common/interest"
	"github.com/lightwebinc/shard-common/objfmt"
)

// recordSink is a host's lane: a TCP listener that keeps each delivery
// record it reads.
type recordSink struct {
	ln  net.Listener
	mu  sync.Mutex
	got [][]byte
}

func newRecordSink(t *testing.T, ip string) *recordSink {
	t.Helper()
	ln, err := net.Listen("tcp", net.JoinHostPort(ip, "0"))
	if err != nil {
		t.Fatal(err)
	}
	s := &recordSink{ln: ln}
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
					r := make([]byte, objfmt.BEEFDeliveryHeaderSize+int(binary.BigEndian.Uint32(hdr[32:36])))
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
	}()
	return s
}

func (s *recordSink) addr() netip.AddrPort { return netip.MustParseAddrPort(s.ln.Addr().String()) }

func (s *recordSink) records() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]byte(nil), s.got...)
}

// deliverTo is the record extension a host announces its lane with.
func deliverTo(t *testing.T, a netip.AddrPort) []byte {
	t.Helper()
	d := interest.Delivery{Kind: interest.DeliverUnicast, Encoding: interest.EncodeRecords, Addr: a.Addr(), Port: a.Port()}
	v, err := d.AppendValue(nil)
	if err != nil {
		t.Fatal(err)
	}
	return interest.AppendTLV(nil, interest.ExtDelivery, v)
}

// A distributing relay sends each record of its lane to exactly the hosts
// that announced the record's topic at an address, and announces upward
// with its own.
func TestDistributor(t *testing.T) {
	tm := newTerminus(t, "127.0.0.10")
	relay := startRelay(t, "127.0.0.20", Config{
		Upstreams:  []netip.AddrPort{tm.addr},
		Distribute: &distributor.Config{Listen: "127.0.0.20:0"},
		Advertise:  netip.MustParseAddrPort("127.0.0.20:9171"),
	})
	dist := relay.r.Distributor()
	eventually(t, 5*time.Second, "the lane listens", func() bool { return dist.Addr() != nil })

	x, y, z := topic("x"), topic("y"), topic("z")
	s1, s2 := newRecordSink(t, "127.0.0.31"), newRecordSink(t, "127.0.0.32")
	h1 := startHost(t, "127.0.0.31", relay.addr)
	h2 := startHost(t, "127.0.0.32", relay.addr)
	h1.c.Set(x, deliverTo(t, s1.addr()))
	h2.c.Set(x, deliverTo(t, s2.addr()))
	h2.c.Set(y, deliverTo(t, s2.addr()))
	// An address the sender may not send to: outside the prefix it
	// announces from.
	h3 := startHost(t, "127.0.0.33", relay.addr)
	h3.c.Set(z, deliverTo(t, netip.MustParseAddrPort("192.0.2.1:9171")))

	eventually(t, 5*time.Second, "both hosts' addresses honored", func() bool {
		return len(relay.r.Receiver().Deliveries(x)) == 2 && len(relay.r.Receiver().Deliveries(y)) == 1
	})
	eventually(t, 5*time.Second, "refused outside the sender's prefix", func() bool {
		return h3.c.Status(z) == interest.StatusBadAddress
	})
	if h1.c.Status(x) != interest.StatusOK {
		t.Fatalf("h1 x %v, want ok (honored)", h1.c.Status(x))
	}
	// Upward the relay names its own lane, which an edge does not honor:
	// the topic is held there and delivered as provisioned.
	eventually(t, 5*time.Second, "the terminus holds the union", func() bool {
		rv := tm.receiver()
		return rv.IsLive("edge", x) && rv.IsLive("edge", y) && !rv.IsLive("edge", z)
	})
	eventually(t, 5*time.Second, "the relay sees its address not honored upstream", func() bool {
		return relay.r.Client().Status(x) == interest.StatusAddressNotHonored
	})

	lane, err := net.Dial("tcp", dist.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lane.Close() }()
	rec := func(name, payload string) []byte {
		return objfmt.EncodeBEEFDelivery(objfmt.TopicID(name), []byte(payload))
	}
	x1, y1 := rec("x", "x1"), rec("y", "y1")
	for _, r := range [][]byte{x1, y1} {
		if _, err := lane.Write(r); err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, 5*time.Second, "both hosts got their records", func() bool {
		return len(s1.records()) == 1 && len(s2.records()) == 2
	})
	if !bytes.Equal(s1.records()[0], x1) || !bytes.Equal(s2.records()[0], x1) || !bytes.Equal(s2.records()[1], y1) {
		t.Fatalf("s1 %q, s2 %q", s1.records(), s2.records())
	}

	// h1 withdraws x: only h2 gets it now.
	h1.c.Remove(x)
	eventually(t, 5*time.Second, "h1's route gone", func() bool { return len(relay.r.Receiver().Deliveries(x)) == 1 })
	x2 := rec("x", "x2")
	if _, err := lane.Write(x2); err != nil {
		t.Fatal(err)
	}
	eventually(t, 5*time.Second, "h2 got x2", func() bool { return len(s2.records()) == 3 })
	time.Sleep(100 * time.Millisecond)
	if len(s1.records()) != 1 {
		t.Fatalf("h1 got a record after withdrawing: %q", s1.records())
	}
}
