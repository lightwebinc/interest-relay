# Testing

## Unit and loopback tests

```sh
make test
```

The relay's tests run a small network on one host. Every party gets its own
address in `127.0.0.0/8`, so that source addresses tell the parties apart as
they would on separate machines. They cover a chain of two relays, the
delivered scope reaching a host, an edge restart, a relay restart with and
without the hold, who may announce, a ring of three relays, and a
distributing relay. The distributor's own tests cover fan-out, a slow host
being shed, a host that restarts, a malformed lane, and that nothing
outlives a stop.

The protocol's own tests are in `shard-common/interest`. They run the two
halves of the protocol against each other on a simulated network with loss,
duplication and reordering, and fuzz the decoders and both state machines.

## The multi-hop test

```sh
make test-netns
```

`test/netns/chain.sh` builds a routed IPv6 network out of network namespaces
and runs the real binaries in it:

```text
 h1 ─┐
     ├─ rtr ── mid ── ce ══ edge
 h2 ─┘
```

| Node | Role |
|---|---|
| `h1`, `h2` | announcers, two IP hops from the nearest relay |
| `rtr` | a plain router that knows nothing of interest |
| `mid` | a relay inside the domain |
| `ce` | the relay at the customer edge; its upstream link stands in for the tunnel |
| `edge` | a stand-in for the delivery edge, delivering six of eight groups |

It checks, in order:

1. Each host is told which of its topics have a stream. The expected split
   is computed in the script from the definition (the group of a topic is
   the top bits of SHA-256 of its name), not from what the programs report.
2. The edge holds the union of what is served, from one sender, two relays
   deep.
3. Nothing on the way holds a topic that is not served.
4. A host that leaves in an orderly way has its topics withdrawn at once.
5. A relay restart changes nothing at the edge.
6. An edge restart is repaired at the relay's next refresh.
7. With a quarter of the datagrams lost on the tunnel link, the set still
   converges.
8. A host that dies without a word has its topics lapse with their
   lifetime.

Everything runs in a private user, network and mount namespace and is gone
when the script exits. It needs unprivileged user namespaces, or root.
`KEEP=1` leaves a shell open inside the namespace at the end.

## The distribution test

`test/netns/distribute.sh`, which `make test-netns` also runs, does the same
for a distributing relay:

```text
 edge ══ ce ── rtr ─┬─ h1
                    ├─ h2
                    └─ h3
```

| Node | Role |
|---|---|
| `edge` | a stand-in for the delivery edge, and the lane: `lane-feed` writes records to the relay as an edge does |
| `ce` | the relay at the customer edge, distributing |
| `rtr` | a plain router |
| `h1`, `h2` | hosts one IP hop past the relay, each with a lane (`lane-sink`) it names as its delivery address |
| `h3` | a host that names an address outside its prefix |

It checks, in order:

1. The relay honors the two hosts' addresses and refuses h3's
   (`bad_address`). The edge holds the union.
2. Each host gets exactly the records of its own topics. The records of a
   topic that nobody asked for are counted as unrouted.
3. A host that leaves gets nothing more, and the other host still gets
   everything it asked for.
4. A host whose lane goes down has its records shed, and the relay stays
   ready.

## The tools

`interest-terminus` stands in for a delivery edge. It can be given a
delivered scope and a bound, to produce the answers a real edge gives.

```sh
# Deliver groups 0 to 5 of an eight-group plane, and serve the state over HTTP.
interest-terminus -listen '[::1]:8729' -shard-bits 3 -groups 0,1,2,3,4,5 -http '[::1]:9700'

curl -s 'http://[::1]:9700/state'
```

| Flag | Meaning |
|---|---|
| `-shard-bits`, `-groups` | the delivered scope: the plane's width and the groups delivered; `-groups` empty means all |
| `-listed`, `-quota` | the bound: topic names that are free, and how many others may be live at once |
| `-allow` | prefixes that may announce |
| `-lifetime-min`, `-lifetime-default`, `-lifetime-max` | lifetimes granted |
| `-http` | where to serve `/state` |

`interest-announce` announces topics by name and prints what comes back:

```sh
interest-announce -upstream '[::1]:8729' tm_example tm_other tm_orders
```

```text
not_served  tm_orders
ok  tm_example
ok  tm_other
# upstream [::1]:8729 reachable=true synced=true held=2 scope=6 of 8 groups lifetime=300s
# topics 3: not_served=1 ok=2
```

`tm_orders` falls in group 6 of 8, which this terminus does not deliver.

| Answer | Meaning |
|---|---|
| `ok` | held, and served as far as the next hop knows |
| `not_served` | no upstream delivers the topic's group; no stream will come |
| `outside_bound` | the account's bound does not allow it |
| `over_cap` | a cap on live topics is reached; asked again at each refresh |
| `rate_limited` | refused for now; asked again shortly |
| `unserved` | held by a relay, not served above it |
| `upstream_refused` | held by a relay, refused above it |
| `address_not_honored` | held; the delivery address named is ignored |
| `bad_address` | refused: the delivery address named is not the sender's own and not inside its prefix |
| `pending` | no answer yet |

`-deliver-to host:port` names a delivery address with every topic: a
unicast stream of delivery records. A distributing relay sends the topics'
records there; an edge answers `address_not_honored` and delivers as
provisioned.

`lane-feed` writes delivery records to a lane, as an edge does, and
`lane-sink` is a host's lane, which counts what arrives by topic:

```sh
lane-sink -listen '[2001:db8:c:1::10]:9171' -names tm_example,tm_other
lane-feed -to '[2001:db8:f::2]:9171' -n 100 -size 512 tm_example tm_other
```

`lane-feed` writes `-n` records of `-size` bytes for each topic,
interleaved, and exits. `lane-sink` prints a line of counts every
`-report`, with `other` for the topics it was not given the names of.
