// Command lane-sink is a host's lane, for trying a distributor: it accepts
// streams of delivery records and counts them by topic.
//
//	lane-sink -listen [2001:db8::10]:9171 -names tm_a,tm_b
//
// Every -report it prints one line, "# records total=N tm_a=.. tm_b=..
// other=..": the named topics, and every record of a topic it was not told
// the name of.
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
	)
	flag.Parse()
	if *listen == "" {
		flag.Usage()
		return fmt.Errorf("-listen is required")
	}
	var order []string
	byID := map[[32]byte]string{}
	for _, n := range strings.Split(*names, ",") {
		if n = strings.TrimSpace(n); n != "" {
			byID[objfmt.TopicID(n)] = n
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
			for {
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
				total++
				mu.Unlock()
			}
		}()
	}
}
