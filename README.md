# interest-relay

A small daemon that relays topic interest.

A host that wants an overlay topic says so to the nearest relay. The relay
holds the union of what the hosts below it want and announces that union to
its own upstream: another relay, or the multicast delivery edge at the far
end of the consumer's tunnel. Interest stops at the delivery edge.

```text
 host ─┐
 host ─┼─▶ relay ─▶ relay at the customer edge ══▶ delivery edge
 host ─┘
```

By default the relay does only this and carries no objects. Started with
`-distribute-listen`, it also ends the consumer's lane and sends each object
on to the hosts that asked for its topic: see
[Distribution](docs/architecture.md#distribution). With `-publish-listen`
as well, it is the domain's publish point: the hosts publish to it, it sends
each submission up the tunnel from the consumer's own address and copies it
at once to the siblings that asked for its topics, so nothing of a host's
own comes back from the edge: see [Publishing](docs/architecture.md#publishing).

The protocol is a draft and its wire may still change.

## Quick start

```sh
make all

# A relay that takes interest from one prefix and announces to one upstream.
build/interest-relay \
  -upstream '[2001:db8:f::1]:8729' \
  -allow    '2001:db8:c::/48'

# From a host inside that prefix: ask for two topics and watch the answers.
build/interest-announce -upstream '[2001:db8:a::1]:8729' tm_example tm_other
```

## What is in the box

| Path | What |
|---|---|
| `.` | the daemon |
| `relay/` | the relay as a package, to embed in another program |
| `distributor/` | the unicast distributor: ends a lane, sends each record to the hosts that asked |
| `publish/` | the publish point: takes the hosts' submissions, sends them up the tunnel, copies them to siblings |
| `cmd/interest-announce` | an announcer for the command line |
| `cmd/interest-terminus` | a stand-in for a delivery edge, for testing |
| `cmd/lane-feed`, `cmd/lane-sink` | a lane writer and a host's lane, for testing distribution |
| `test/netns/chain.sh` | a multi-hop test on one machine, in network namespaces |
| `test/netns/distribute.sh` | distribution end to end, in network namespaces |

The protocol itself (the wire, the announcer and the receiver) lives in
[`shard-common/interest`](https://github.com/lightwebinc/shard-common).

## Documentation

- [How it works](docs/architecture.md): roles, soft state, scope, restarts, loops, distribution, publishing
- [Configuration](docs/configuration.md): every flag and environment variable
- [Operations](docs/operations.md): metrics, health, the state endpoint, what to alert on
- [Testing](docs/testing.md): the test tools and the namespace test

## License

Apache License 2.0; see [LICENSE](LICENSE) and [NOTICE](NOTICE). The
licenses of the modules the static binaries link are in
[LICENSE-THIRD-PARTY](LICENSE-THIRD-PARTY).
