# Operations

## Endpoints

Served on `-metrics-addr` (default `:9600`).

| Path | What |
|---|---|
| `/metrics` | Prometheus exposition. Every series exists from the first scrape, at zero. |
| `/healthz` | 200 while the process runs. |
| `/readyz` | 200 when at least one upstream is reachable and holds everything the relay wants; 503 otherwise. |
| `/state` | The relay's table as JSON: each sender below, each upstream above. |
| `/loglevel` | `GET` reads the log level; `PUT ?level=debug` sets it. |

## Metrics

All names start with `bir_`.

### What the relay holds

| Metric | Meaning |
|---|---|
| `bir_senders` | Senders tracked now. |
| `bir_topics_held` | Topics held, summed over senders. A topic two senders announce counts twice. |
| `bir_topics_live` | Distinct topics held: the union announced upstream. |
| `bir_topics_wanted` | Topics the relay is asking its upstreams for. |
| `bir_topics_unserved` | Wanted topics that no upstream serves. |
| `bir_hops` | Hop count the relay announces with. |
| `bir_ready` | 1 when `/readyz` would answer 200. |

### What arrives from below

| Metric | Labels | Meaning |
|---|---|---|
| `bir_datagrams_total` | | Datagrams received on the listening socket. |
| `bir_datagrams_ignored_total` | `reason` | Not processed: `malformed`, `unidentified` (source not allowed, or an upstream), `version`. |
| `bir_tokens_issued_total` | | Messages answered with a token instead of applied. |
| `bir_messages_total` | `outcome` | `applied`, `matched`, `mismatch`, `stale`, `busy`, `hop_limit`. |
| `bir_topic_changes_total` | `change` | `add`, `refresh`, `withdraw`, `expire`, `sweep`, `reevaluate`. |
| `bir_topics_refused_total` | `reason` | Records refused, by reason. |
| `bir_resyncs_queued_total` | | Pokes queued to make senders check in early. |

### What goes on above

Each carries an `upstream` label.

| Metric | Meaning |
|---|---|
| `bir_upstream_reachable` | 1 when the upstream has answered since the last three consecutive losses. |
| `bir_upstream_synced` | 1 when the upstream holds exactly what the relay wants it to. 0 during the restart hold. |
| `bir_upstream_holding` | 1 during the restart hold: everything wanted has been said and answered, and the upstream still keeps what the previous run announced. |
| `bir_upstream_topics_held` | Topics the upstream holds for the relay. |
| `bir_upstream_topics_tracked` | Topics the relay keeps this upstream told about. Fewer than wanted when the upstream delivers only some groups. |
| `bir_upstream_in_flight` | Datagrams sent and not yet answered. |
| `bir_upstream_datagrams_sent_total{type}` | `announce` or `digest`. |
| `bir_upstream_acks_total` | Answers that matched a datagram in flight. |
| `bir_upstream_events_total{event}` | `timeout`, `sync`, `token`, `stale`, `mismatch`, `busy`, `poke`, `ignored`. |

## Reading the numbers

| You see | It means |
|---|---|
| `bir_upstream_reachable` 0 | The upstream does not answer. Check the path and the upstream's allow list: a sender that is not allowed gets no answer at all, which looks the same as a dead path. |
| `bir_upstream_synced` 0 and `bir_upstream_holding` 0 for longer than a few seconds | A replay is under way or keeps failing. Look at `bir_upstream_events_total{event="mismatch"}` and `{event="timeout"}`. |
| `bir_upstream_events_total{event="token"}` rises | The upstream restarted, or the address the relay announces from changed. One step per event is normal. |
| `bir_upstream_events_total{event="sync"}` rises steadily | Something is lost in a way acknowledgments do not show. With a lossy path a few are normal; a steady climb is not. |
| `bir_messages_total{outcome="hop_limit"}` above 0 | Relays are configured in a ring. Fix the upstreams. |
| `bir_datagrams_ignored_total{reason="unidentified"}` rises | Something outside `-allow` is announcing, or an upstream is pointed back at this relay. |
| `bir_topic_changes_total{change="expire"}` rises | Senders are going silent without withdrawing: they died, or their refreshes are not arriving. |
| `bir_topics_unserved` above 0 | Hosts want topics that no upstream delivers. The hosts have been told. |
| `bir_topics_refused_total{reason="rate_limited"}` rises | Senders add topics faster than `-change-rate`. They retry by themselves. |

## Restarting a relay

Restart it. With the default `-restart-hold` the upstream keeps delivering
what the previous run announced while the relay learns its set again, which
takes up to a third of `-lifetime-default`.

Do not change `-sender-label` or the host name across a restart: the relay
would look like a new sender to its upstream, and what the old one
announced would stay until its lifetime ran out.

To take a relay out of service for good, stop it with `-withdraw-on-exit`
set, so that its upstream stops delivering at once.

## The state endpoint

```sh
curl -s http://localhost:9600/state
```

```json
{
  "version": "dev",
  "started": "2026-01-01T00:00:00Z",
  "ready": true,
  "hops": 1,
  "topics_live": 4,
  "topics_wanted": 4,
  "senders": [
    {
      "addr": "[2001:db8:c:1::10]:41890",
      "sender_id": "2bddd592198dc558",
      "topics": 4,
      "hops": 0,
      "last_heard": "2026-01-01T00:00:02Z"
    }
  ],
  "upstreams": [
    {
      "addr": "[2001:db8:b::1]:8729",
      "reachable": true,
      "synced": false,
      "holding": true,
      "topics_held": 3,
      "topics_tracked": 4,
      "scope": "6 groups",
      "shard_bits": 3
    }
  ]
}
```

In this sample the relay started a moment ago and is in its restart hold
(`holding`), and its upstream holds three of the four topics: the fourth is
refused upstream and is asked for again at each refresh.
