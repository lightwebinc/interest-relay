# How the relay works

## The problem it solves

A consumer of overlay topics is served by a multicast delivery edge, at the
far end of a tunnel. The edge delivers a topic only while someone has asked
for it. The process that wants the topic is often not the machine that holds
the tunnel: it may be several network hops away, inside the consumer's own
network. Something has to carry its interest to the edge.

That is the relay. Each relay is configured with an upstream, in the manner
of a default route, and accepts interest from a set of prefixes below it.

## Roles

| Role | What it does | Program |
|---|---|---|
| Announcer | wants topics; tells its upstream | any program using the client; `interest-announce` |
| Relay | holds the union of what is below it; tells its own upstream | `interest-relay` |
| Terminus | where interest ends: the delivery edge | the edge; `interest-terminus` for tests |

The same four datagrams are used at every hop. An announcer whose own
machine holds the tunnel talks to the edge directly, and a relay is never
required.

## What a relay keeps

| State | Per | Lifetime |
|---|---|---|
| the topics a sender announced | sender (source address and sender identity) | soft: each topic lapses unless refreshed |
| the union of those topics | relay | derived |
| how each upstream answered | upstream | for as long as the topic is wanted |

A topic that two senders want is announced upstream once, and stays until
the second sender lets go.

The upstream sees one sender: the relay. It does not learn how many hosts
stand behind it.

## Soft state

A sender is granted a lifetime for what it announces and refreshes at a
third of it. A refresh is one small datagram that carries a digest of the
sender's whole set, whatever its size. If the digest matches what the relay
holds, every topic is refreshed at once.

If a sender goes silent, what it announced lapses when the lifetime runs
out. If it leaves in an orderly way, it can withdraw everything in one
datagram.

## Loss and reordering

Nothing is applied out of order: a relay applies a message only if its
sequence number is above the last one it applied for that sender. A sender
does not retransmit a datagram. What was not acknowledged is still to be
said, and is said again as it stands now.

Every acknowledgment carries a digest of the set the relay holds for the
sender. If it is not the set the sender expects, the sender replays its
whole set and ends with a digest that removes whatever the replay did not
mention.

## Delivered scope

An upstream may deliver only some groups of the object plane. A topic that
maps to a group it does not deliver has no stream behind it.

The edge says which groups it delivers. Each relay passes that down, so a
relay refuses a topic that nothing above it serves, and the host that asked
is told. A host that knows the scope does not send such topics at all.

With several upstreams, a topic is announced to every upstream that serves
its group and to no other. A topic that all of them serve goes to all of
them, so a standby already holds it when it takes over.

A topic the relay holds but cannot get served (refused upstream for another
reason, or announced before the scope was known) is kept and asked for
again at each refresh. The sender is told it is held but unserved, and has
nothing to do.

## Restarts

| What restarts | What happens |
|---|---|
| The upstream | It holds nothing. The relay sees a new token in the next answer and replays its set. An upstream that knows the relay's address can ask at once instead of waiting for the refresh. |
| The relay | It starts empty and learns its set again as the senders below check in, which takes up to one refresh interval. Until then its upstream keeps what the previous run announced: the relay does not let the upstream drop anything for the length of `-restart-hold`. Nothing that is still wanted stops being delivered, as long as the hold covers the refresh interval of every sender below (see `-restart-hold`). An upstream that pokes the relay during the hold, to say that what it serves changed, is asked for its scope at once. |
| A sender | It replays its set. What it no longer wants is removed when the replay ends. |

The relay's identity toward its upstream is derived from the host name and
`-sender-label`, so it is the same after a restart. That is what lets the
upstream match the new run to what the old one announced.

## Who may announce

A relay accepts interest only from the prefixes in `-allow`. Anything else
is ignored without an answer.

A sender must also echo a token that the relay issues to its source
address. The first message from a new sender is answered with the token and
nothing else, and is not applied. This proves the sender can receive at the
address it claims, and it keeps the answer to an unproven source no larger
than the request.

There is no signature. Inside a network domain the relay trusts the
prefixes it was given.

## Loops

A relay never accepts interest from the address of one of its own
upstreams, so two relays pointed at each other do not feed each other.

Every message carries a hop count: zero from an announcer, and from a relay
one more than the deepest sender below it. In a longer ring the count
climbs until it passes `-max-hops` and the message is refused, which stops
a withdrawn topic from living on in the ring. The refusal is counted
(`bir_messages_total{outcome="hop_limit"}`); any value above zero means the
relays are configured in a ring.

## What the relay does not do

- It carries no objects. Delivery reaches the consumer where the
  consumer's lane ends, as it does without a relay.
- It does not honor a delivery address. A sender that names one is told so
  and its interest is still held.
- It does not discover its upstream. The upstream is configuration.
- It keeps nothing on disk.
