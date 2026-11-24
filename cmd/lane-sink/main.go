// Command lane-sink is a host's lane, for trying a distributor: it accepts
// streams of delivery records and counts them by topic. With -submissions it
// is an edge's ingress instead, for trying a publish point: it accepts
// streams of submission records and counts them by their first topic.
//
//	lane-sink -listen [2001:db8::10]:9171 -names tm_a,tm_b
//
// Every -report it prints one line, "# records total=N tm_a=.. tm_b=..
// other=.. sources=A,B": the named topics, every record of a topic it was
// not told the name of, and the addresses the records came from.
package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/lightwebinc/shard-common/objfmt"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "lane-sink:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		listen = flag.String("listen", "", "TCP address to accept delivery records on (required)")
		names  = flag.String("names", "", "comma-separated topic names to count by name")
		report = flag.Duration("report", 500*time.Millisecond, "how often to print the counts")
		subs   = flag.Bool("submissions", false, "accept BRC-149 submission records, as an ingress, instead of delivery records")
	)
	flag.Parse()
	if *listen == "" {
		flag.Usage()
		return fmt.Errorf("-listen is required")
	}
	var order []string
	byID := map[[32]byte]string{}
	byName := map[string]bool{}
	for _, n := range strings.Split(*names, ",") {
		if n = strings.TrimSpace(n); n != "" {
			byID[objfmt.TopicID(n)] = n
			byName[n] = true
			order = append(order, n)
		}
	}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	go func() { <-ctx.Done(); _ = ln.Close() }()

	var mu sync.Mutex
	counts := map[string]int{}
	sources := map[string]bool{}
	total := 0
	summary := func() {
		mu.Lock()
		defer mu.Unlock()
		var b strings.Builder
		fmt.Fprintf(&b, "# records total=%d", total)
		for _, n := range order {
			fmt.Fprintf(&b, " %s=%d", n, counts[n])
		}
		fmt.Fprintf(&b, " other=%d", counts[""])
		var from []string
		for a := range sources {
			from = append(from, a)
		}
		sort.Strings(from)
		fmt.Fprintf(&b, " sources=%s", strings.Join(from, ","))
		fmt.Println(b.String())
	}
	go func() {
		t := time.NewTicker(*report)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				summary()
			}
		}
	}()
	for {
		c, err := ln.Accept()
		if err != nil {
			summary()
			return nil
		}
		go func() {
			defer func() { _ = c.Close() }()
			br := bufio.NewReader(c)
			src := c.RemoteAddr().(*net.TCPAddr).IP.String()
			for {
				if *subs {
					name, err := readSubmission(br)
					if err != nil {
						return
					}
					mu.Lock()
					if _, known := byName[name]; known {
						counts[name]++
					} else {
						counts[""]++
					}
					sources[src] = true
					total++
					mu.Unlock()
					continue
				}
				var hdr [objfmt.BEEFDeliveryHeaderSize]byte
				if _, err := io.ReadFull(br, hdr[:]); err != nil {
					return
				}
				n := int64(binary.BigEndian.Uint32(hdr[32:36]))
				if _, err := io.CopyN(io.Discard, br, n); err != nil {
					return
				}
				var id [32]byte
				copy(id[:], hdr[:32])
				mu.Lock()
				counts[byID[id]]++
				sources[src] = true
				total++
				mu.Unlock()
			}
		}()
	}
}

// readSubmission reads one submission record and returns its first topic.
func readSubmission(br *bufio.Reader) (string, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(br, hdr[:]); err != nil {
		return "", err
	}
	if binary.BigEndian.Uint16(hdr[0:2]) != objfmt.BEEFRecordTag || hdr[3] == 0 {
		return "", fmt.Errorf("not a submission record")
	}
	first := ""
	for i := 0; i < int(hdr[3]); i++ {
		n, err := br.ReadByte()
		if err != nil {
			return "", err
		}
		name := make([]byte, n)
		if _, err := io.ReadFull(br, name); err != nil {
			return "", err
		}
		if i == 0 {
			first = string(name)
		}
	}
	var l [4]byte
	if _, err := io.ReadFull(br, l[:]); err != nil {
		return "", err
	}
	_, err := io.CopyN(io.Discard, br, int64(binary.BigEndian.Uint32(l[:])))
	return first, err
}
