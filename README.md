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

The relay does only this. It carries no objects.

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
| `cmd/interest-announce` | an announcer for the command line |
| `cmd/interest-terminus` | a stand-in for a delivery edge, for testing |
| `test/netns/chain.sh` | a multi-hop test on one machine, in network namespaces |

The protocol itself (the wire, the announcer and the receiver) lives in
[`shard-common/interest`](https://github.com/lightwebinc/shard-common).

## Documentation

- [How it works](docs/architecture.md): roles, soft state, scope, restarts, loops
- [Configuration](docs/configuration.md): every flag and environment variable
- [Operations](docs/operations.md): metrics, health, the state endpoint, what to alert on
- [Testing](docs/testing.md): the test tools and the namespace test

## License

Apache License 2.0; see [LICENSE](LICENSE) and [NOTICE](NOTICE). The
licenses of the modules the static binaries link are in
[LICENSE-THIRD-PARTY](LICENSE-THIRD-PARTY).
