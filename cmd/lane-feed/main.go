// Command lane-feed writes delivery records to a lane, as an edge does, for
// trying a distributor.
//
//	lane-feed -to [2001:db8::2]:9171 -n 100 -size 512 tm_a tm_b
//
// It writes -n records of -size bytes for each topic named, interleaved,
// then prints "# fed records=N bytes=M" and exits.
package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/lightwebinc/shard-common/objfmt"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "lane-feed:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		to   = flag.String("to", "", "TCP address of the lane (required)")
		n    = flag.Int("n", 100, "records per topic")
		size = flag.Int("size", 256, "bytes of object per record")
	)
	flag.Parse()
	if *to == "" || flag.NArg() == 0 || *n < 1 || *size < 1 {
		flag.Usage()
		return fmt.Errorf("-to and at least one topic are required; -n and -size at least 1")
	}
	// The lane may not be listening yet: dial for a while, as an edge does.
	var c net.Conn
	var err error
	for deadline := time.Now().Add(10 * time.Second); ; {
		if c, err = net.DialTimeout("tcp", *to, 2*time.Second); err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	ids := make([][32]byte, flag.NArg())
	for i, name := range flag.Args() {
		ids[i] = objfmt.TopicID(name)
	}
	obj := make([]byte, *size)
	records, bytes := 0, 0
	for i := 0; i < *n; i++ {
		for _, id := range ids {
			obj[0] = byte(i)
			r := objfmt.EncodeBEEFDelivery(id, obj)
			if _, err := c.Write(r); err != nil {
				return err
			}
			records++
			bytes += len(r)
		}
	}
	fmt.Printf("# fed records=%d bytes=%d\n", records, bytes)
	return nil
}
