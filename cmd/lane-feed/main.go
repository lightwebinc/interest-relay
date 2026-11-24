// Command lane-feed writes delivery records to a lane, as an edge does, for
// trying a distributor; with -submit, it writes submission records to an
// ingress instead, as a host's bridge does, for trying a publish point.
//
//	lane-feed -to [2001:db8::2]:9171 -n 100 -size 512 tm_a tm_b
//	lane-feed -submit -from 2001:db8:c:1::10 -to [2001:db8:b::1]:8725 tm_a
//
// It writes -n records of -size bytes for each topic named, interleaved,
// then prints "# fed records=N bytes=M" and exits. A submitted object leads
// with a BEEF marker, so it passes an ingress's marker check.
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
		to     = flag.String("to", "", "TCP address of the lane (required)")
		n      = flag.Int("n", 100, "records per topic")
		size   = flag.Int("size", 256, "bytes of object per record")
		submit = flag.Bool("submit", false, "write BRC-149 submission records (to an ingress or a publish point) instead of delivery records")
		from   = flag.String("from", "", "local address to connect from (default: the kernel's choice)")
		lead   = flag.Int("lead-size", 0, "bytes of object of the first record only (0: -size), to put one odd record on the same stream as the rest")
	)
	flag.Parse()
	if *to == "" || flag.NArg() == 0 || *n < 1 || *size < 1 || *lead < 0 {
		flag.Usage()
		return fmt.Errorf("-to and at least one topic are required; -n and -size at least 1")
	}
	if *submit && (*size < 5 || (*lead > 0 && *lead < 5)) {
		return fmt.Errorf("-submit objects lead with a 4-byte BEEF marker and a counter: -size and -lead-size at least 5")
	}
	var src net.IP
	if *from != "" {
		if src = net.ParseIP(*from); src == nil {
			return fmt.Errorf("-from %q is not an address", *from)
		}
	}
	// The lane may not be listening yet: dial for a while, as an edge does.
	var c net.Conn
	var err error
	for deadline := time.Now().Add(10 * time.Second); ; {
		d := net.Dialer{Timeout: 2 * time.Second}
		if src != nil {
			d.LocalAddr = &net.TCPAddr{IP: src}
		}
		if c, err = d.Dial("tcp", *to); err == nil || time.Now().After(deadline) {
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
	object := func(n int) []byte {
		o := make([]byte, n)
		if *submit {
			copy(o, []byte{0x01, 0x00, 0xBE, 0xEF}) // BEEF V1's version word
		}
		return o
	}
	body := object(*size)
	records, bytes := 0, 0
	for i := 0; i < *n; i++ {
		for k, id := range ids {
			obj := body
			if i == 0 && k == 0 && *lead > 0 {
				obj = object(*lead)
			}
			obj[min(4, len(obj)-1)] = byte(i)
			var r []byte
			if *submit {
				if r, err = objfmt.EncodeBEEFRecord([]string{flag.Arg(k)}, obj); err != nil {
					return err
				}
			} else {
				r = objfmt.EncodeBEEFDelivery(id, obj)
			}
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
