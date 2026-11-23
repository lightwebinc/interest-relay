# Configuration

Every parameter is a command-line flag with an environment variable as a
fallback. A flag wins over the environment.

## Required

| Flag | Environment | Meaning |
|---|---|---|
| `-upstream` | `UPSTREAM` | Where the relay announces to: one or more `host:port`, comma-separated. A name is resolved once, at start. |
| `-allow` | `ALLOW` | The prefixes senders may announce from, comma-separated. A bare address is a prefix of one. |

## Network

| Flag | Environment | Default | Meaning |
|---|---|---|---|
| `-listen` | `LISTEN` | `[::]:8729` | Where senders announce to. |
| `-upstream-bind` | `UPSTREAM_BIND` | `[::]:0` | The local address the relay announces from. Port 0 lets the kernel choose. |
| `-sender-label` | `SENDER_LABEL` | `interest-relay` | Distinguishes this relay from any other sender on the same host. Two relays on one host need different labels. |

## Lifetimes

Lifetimes are whole seconds.

| Flag | Environment | Default | Meaning |
|---|---|---|---|
| `-lifetime-min` | `LIFETIME_MIN` | `30s` | Shortest lifetime granted to a sender. |
| `-lifetime-default` | `LIFETIME_DEFAULT` | `300s` | Lifetime granted when a sender asks for the default. Senders refresh at a third of it. |
| `-lifetime-max` | `LIFETIME_MAX` | `3600s` | Longest lifetime granted to a sender. |
| `-upstream-lifetime` | `UPSTREAM_LIFETIME` | `0` | Lifetime the relay asks of its upstreams. 0 asks for each upstream's default. |

## Limits

Zero means no limit.

| Flag | Environment | Default | Meaning |
|---|---|---|---|
| `-max-topics-per-sender` | `MAX_TOPICS_PER_SENDER` | `0` | Topics held for one sender. Beyond it a topic is refused as over the cap. |
| `-max-senders` | `MAX_SENDERS` | `0` | Senders tracked at once. |
| `-change-rate` | `CHANGE_RATE` | `0` | New topics a sender may add per second. Beyond it a topic is refused for now and the sender tries again. |
| `-change-burst` | `CHANGE_BURST` | `0` | Burst allowed above `-change-rate`. |
| `-max-hops` | `MAX_HOPS` | `16` | Relays a message may have crossed before it is refused. |

## Restart and exit

| Flag | Environment | Default | Meaning |
|---|---|---|---|
| `-restart-hold` | `RESTART_HOLD` | automatic | After a start, how long the upstreams keep what the previous run announced. Automatic is a third of `-lifetime-default` plus five seconds, which is how long a sender below that uses the default lifetime takes to check in. A sender that asks a longer lifetime checks in less often (a third of what it asks): when senders below do that, set the hold to a third of the longest lifetime they ask, or what they announce stops after the hold until they next check in. `0s` drops at once. |
| `-withdraw-on-exit` | `WITHDRAW_ON_EXIT` | `false` | Withdraw everything from the upstreams on exit. Leave it off for a relay that restarts, or every topic stops until the hosts announce again. |

## Distribution

Off unless `-distribute-listen` is set. See
[Distribution](architecture.md#distribution).

| Flag | Environment | Default | Meaning |
|---|---|---|---|
| `-distribute-listen` | `DISTRIBUTE_LISTEN` | empty | The TCP address the consumer's lane is delivered to. Set, the relay ends the lane and sends each record on to the hosts below that asked for its topic at a delivery address. Empty, the relay is control only. |
| `-distribute-advertise` | `DISTRIBUTE_ADVERTISE` | empty | Required with `-distribute-listen`: the address and port the upstream reaches `-distribute-listen` at, announced upward as this relay's delivery address. The consumer's lane must be provisioned to it. |
| `-distribute-queue` | `DISTRIBUTE_QUEUE` | `1024` | Records that may wait for one host. Beyond it the host's records are shed. |
| `-distribute-queue-mib` | `DISTRIBUTE_QUEUE_MIB` | `64` | MiB that may wait for one host. Beyond it the host's records are shed, except that a larger record still goes to a host with nothing waiting. |
| `-distribute-max-object-mib` | `DISTRIBUTE_MAX_OBJECT_MIB` | `64` | The largest object on the lane, in MiB. A longer record means a broken lane, which is closed and counted. |

## Observability

| Flag | Environment | Default | Meaning |
|---|---|---|---|
| `-metrics-addr` | `METRICS_ADDR` | `:9600` | Metrics and health listener. Empty disables it. |
| `-instance-id` | `INSTANCE_ID` | host name | `service.instance.id` in logs. |
| `-log-level` | `LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error`. `SIGHUP` toggles debug. |
| `-log-format` | `LOG_FORMAT` | `text` | `text` on stderr, or `json` on stdout. |

## Examples

A relay inside a network, pointing at the relay on the customer edge:

```sh
interest-relay \
  -listen   '[2001:db8:a::1]:8729' \
  -upstream '[2001:db8:b::1]:8729' \
  -allow    '2001:db8:c::/48'
```

The relay on the customer edge, announcing through the tunnel to both
delivery edges it is peered with:

```sh
interest-relay \
  -listen   '[2001:db8:b::1]:8729' \
  -upstream '[2001:db8:f::a]:8729,[2001:db8:f::b]:8729' \
  -allow    '2001:db8:b::/64'
```

The relay on the customer edge, distributing: the consumer's lane is
provisioned to `[2001:db8:f::2]:9171`, and the relay sends it on to the
hosts in `2001:db8:c::/48` that name their own lanes:

```sh
interest-relay \
  -listen   '[2001:db8:b::1]:8729' \
  -upstream '[2001:db8:f::1]:8729' \
  -allow    '2001:db8:c::/48' \
  -distribute-listen    '[2001:db8:f::2]:9171' \
  -distribute-advertise '[2001:db8:f::2]:9171'

# On a host: two topics, delivered to this host's lane.
interest-announce -upstream '[2001:db8:b::1]:8729' \
  -deliver-to '[2001:db8:c:1::10]:9171' tm_example tm_other
```
