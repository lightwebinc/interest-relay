#!/usr/bin/env bash
# End-to-end test of the relay across real network hops, on one machine.
#
# It builds a small routed IPv6 network out of network namespaces and runs
# the real binaries in it:
#
#   h1 ─┐
#       ├─ rtr ── mid ── ce ══ edge
#   h2 ─┘  (router) (relay) (relay)  (terminus)
#
#   h1, h2  announcers, two IP hops from the nearest relay
#   rtr     a plain router; it knows nothing of interest
#   mid     a relay inside the domain
#   ce      the relay at the customer edge; its upstream link stands in for
#           the consumer's tunnel
#   edge    a stand-in for the delivery edge, delivering six of eight groups
#
# Everything lives in a private user, network and mount namespace and is
# gone when the script exits. It needs unprivileged user namespaces, or
# root. Addresses are from the documentation prefix.
#
#   test/netns/chain.sh            run the test
#   KEEP=1 test/netns/chain.sh     leave a shell open in the namespace at the end
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
BIN=${BIN:-$root/build}

if [ "${IN_NS:-}" != 1 ]; then
	mkdir -p "$BIN"
	(cd "$root" && go build -o "$BIN/" . ./cmd/...)
	if [ "$(id -u)" = 0 ]; then
		exec env IN_NS=1 BIN="$BIN" unshare -nm --propagation private "$0" "$@"
	fi
	exec env IN_NS=1 BIN="$BIN" unshare -Urnm --propagation private "$0" "$@"
fi

# A private /run, so that `ip netns` has somewhere to keep its namespaces.
mount -t tmpfs tmpfs /run
mkdir -p /run/netns
work=$(mktemp -d)
pids=()
cleanup() {
	for p in "${pids[@]:-}"; do kill "$p" 2>/dev/null || true; done
	wait 2>/dev/null || true
	if [ "${failed:-0}" != 0 ]; then
		echo "--- logs in $work"
		tail -n 15 "$work"/*.log 2>/dev/null || true
	else
		rm -rf "$work"
	fi
}
trap cleanup EXIT

L=(-lifetime-min 3s -lifetime-default 9s -lifetime-max 60s)
EDGE=2001:db8:f::1 CE_UP=2001:db8:f::2 CE=2001:db8:b::1 MID_UP=2001:db8:b::2 MID=2001:db8:a::1 RTR=2001:db8:a::2

for n in edge ce mid rtr h1 h2; do
	ip netns add "$n"
	ip -n "$n" link set lo up
done
link() { # ns-a if-a addr-a ns-b if-b addr-b
	ip link add "$2" netns "$1" type veth peer name "$5" netns "$4"
	ip -n "$1" addr add "$3" dev "$2" nodad
	ip -n "$4" addr add "$6" dev "$5" nodad
	ip -n "$1" link set "$2" up
	ip -n "$4" link set "$5" up
}
link edge e0 $EDGE/64 ce c0 $CE_UP/64
link ce c1 $CE/64 mid m0 $MID_UP/64
link mid m1 $MID/64 rtr r0 $RTR/64
link rtr r1 2001:db8:c:1::1/64 h1 eth0 2001:db8:c:1::10/64
link rtr r2 2001:db8:c:2::1/64 h2 eth0 2001:db8:c:2::10/64
ip netns exec rtr sysctl -qw net.ipv6.conf.all.forwarding=1
ip -n h1 route add default via 2001:db8:c:1::1
ip -n h2 route add default via 2001:db8:c:2::1
ip -n mid route add 2001:db8:c::/48 via $RTR
# The hosts are two IP hops from the relay. Wait for the path: the first
# packets across fresh links wait on neighbour discovery at every hop.
for h in h1 h2; do
	for try in 1 2 3 4 5; do
		ip netns exec "$h" ping -6 -c 1 -W 3 $MID >/dev/null 2>&1 && break
		[ "$try" = 5 ] && { echo "no path from $h to the relay"; exit 1; }
	done
done

start() { # name ns command...
	local name=$1 ns=$2
	shift 2
	ip netns exec "$ns" "$@" >"$work/$name.log" 2>&1 &
	pids+=($!)
	eval "pid_$name=$!"
}
stop() { # name [signal]
	local v="pid_$1"
	kill "-${2:-TERM}" "${!v}" 2>/dev/null || true
	wait "${!v}" 2>/dev/null || true
}
start_edge() {
	start edge edge "$BIN/interest-terminus" -listen "[$EDGE]:8729" -http "[::1]:9700" \
		-shard-bits 3 -groups 0,1,2,3,4,5 "${L[@]}" -quiet
}
start_ce() {
	start ce ce "$BIN/interest-relay" -listen "[$CE]:8729" -upstream "[$EDGE]:8729" \
		-allow 2001:db8:b::/64 -metrics-addr "[::1]:9600" -sender-label ce "${L[@]}"
}
start_mid() {
	start mid mid "$BIN/interest-relay" -listen "[$MID]:8729" -upstream "[$CE]:8729" \
		-allow 2001:db8:c::/48 -metrics-addr "[::1]:9600" -sender-label mid "${L[@]}"
}
start_host() { # name count
	start "$1" "$1" "$BIN/interest-announce" -upstream "[$MID]:8729" -sender-label "$1" \
		-generate "$2" -generate-prefix "tm_$1" -quiet -report 1s
}

edge_state() { ip netns exec edge curl -fsS "http://[::1]:9700/state"; }
edge_field() { edge_state | python3 -c "import json,sys; d=json.load(sys.stdin); print($1)"; }
metric() { # ns name
	ip netns exec "$1" curl -fsS "http://[::1]:9600/metrics" | awk -v m="$2" '$1==m {print $2}'
}
host_count() { # name status   (from the tool's last summary line)
	grep '^# topics' "$work/$1.log" | tail -1 | tr ' ' '\n' | awk -F= -v s="$2" '$1==s {print $2}' | grep . || echo 0
}
# served: how many of <prefix>-0 .. <prefix>-(n-1) fall in the groups the
# edge delivers, worked out here from the definition and not from anything
# the programs under test say: the group of a topic is the top three bits
# of SHA-256 of its name, and the edge delivers groups 0 to 5.
served() { # prefix count
	python3 -c "
import hashlib, sys
print(sum(1 for i in range(int(sys.argv[2])) if hashlib.sha256(f'{sys.argv[1]}-{i}'.encode()).digest()[0] >> 5 <= 5))" "$1" "$2"
}
failed=0
check() { # description, command that must print the expected value, expected
	local what=$1 want=$3 got
	local deadline=$((SECONDS + ${4:-20}))
	while :; do
		got=$(eval "$2" 2>/dev/null || true)
		[ "$got" = "$want" ] && break
		if [ $SECONDS -ge $deadline ]; then
			echo "FAIL  $what: got '$got', want '$want'"
			failed=1
			return 0
		fi
		sleep 0.2
	done
	echo "ok    $what ($got)"
}

echo "== start: edge, two relays, two hosts"
start_edge
start_ce
start_mid
start_host h1 400
start_host h2 300
ok1=$(served tm_h1 400) ok2=$(served tm_h2 300)
echo "      by the definition, the edge serves $ok1 of h1's 400 topics and $ok2 of h2's 300"
[ "$ok1" -gt 0 ] && [ "$ok1" -lt 400 ] && [ "$ok2" -gt 0 ] && [ "$ok2" -lt 300 ] ||
	{ echo "FAIL  the test needs topics on both sides of the scope"; failed=1; }

echo "== each host is told, three hops from the edge, which of its topics have a stream"
check "h1: topics served" "host_count h1 ok" "$ok1" 30
check "h1: topics not served" "host_count h1 not_served" $((400 - ok1)) 30
check "h2: topics served" "host_count h2 ok" "$ok2" 30
check "h2: topics not served" "host_count h2 not_served" $((300 - ok2)) 30

echo "== the edge holds the union of what is served, from one sender, two relays deep"
check "topics live at the edge" "edge_field 'd[\"live\"]'" $((ok1 + ok2))
check "topics held at the edge" "edge_field 'd[\"held\"]'" $((ok1 + ok2))
check "senders at the edge" "edge_field 'len(d[\"senders\"])'" 1
check "hops of that sender" "edge_field 'd[\"senders\"][0][\"hops\"]'" 2
check "the edge sees the ce relay's address" "edge_field 'd[\"senders\"][0][\"addr\"].rsplit(\":\",1)[0]'" "[$CE_UP]"

echo "== nothing on the way holds what is not served"
check "ce relay: union" "metric ce bir_topics_live" $((ok1 + ok2))
check "mid relay: union" "metric mid bir_topics_live" $((ok1 + ok2))
check "mid relay: senders" "metric mid bir_senders" 2
check "ce relay: in step with the edge" "metric ce 'bir_upstream_synced{upstream=\"[$EDGE]:8729\"}'" 1
check "mid relay: ready" "metric mid bir_ready" 1

echo "== a host leaves and says so: its topics go at once"
stop h2 TERM
check "topics live at the edge" "edge_field 'd[\"live\"]'" "$ok1" 5

echo "== the mid relay restarts: nothing that is wanted stops at the edge"
before=$(edge_field 'd["changes"]')
stop mid KILL
start_mid
check "mid relay relearned its set" "metric mid bir_topics_live" "$ok1" 20
sleep 4
check "changes at the edge during the restart" "echo \$(( \$(edge_field 'd[\"changes\"]') - $before ))" 0 5
check "topics live at the edge" "edge_field 'd[\"live\"]'" "$ok1" 5

echo "== the edge restarts empty: the relay restores it at its next refresh"
stop edge KILL
start_edge
check "topics live at the restarted edge" "edge_field 'd[\"live\"]'" "$ok1" 20

echo "== the tunnel loses a quarter of its datagrams: the set still converges"
if ip netns exec ce tc qdisc add dev c0 root netem loss 25% 2>/dev/null &&
	ip netns exec edge tc qdisc add dev e0 root netem loss 25% 2>/dev/null; then
	start_host h2 300
	check "topics live at the edge under loss" "edge_field 'd[\"live\"]'" $((ok1 + ok2)) 60
	ip netns exec ce tc qdisc del dev c0 root
	ip netns exec edge tc qdisc del dev e0 root
else
	echo "skip  no netem in this kernel namespace; loss is covered by the library's own tests"
	start_host h2 300
	check "topics live at the edge" "edge_field 'd[\"live\"]'" $((ok1 + ok2)) 20
fi

echo "== a host dies without a word: its topics lapse with their lifetime"
stop h1 KILL
check "topics live at the edge" "edge_field 'd[\"live\"]'" "$ok2" 40
check "expiries at the mid relay" "test \$(metric mid 'bir_topic_changes_total{change=\"expire\"}') -ge $ok1 && echo yes" yes 5

if [ "${KEEP:-}" = 1 ]; then
	echo "== namespaces: edge ce mid rtr h1 h2 (ip netns exec <name> ...); logs in $work; exit to tear down"
	failed=1 bash -i || true
fi
[ "$failed" = 0 ] && echo "PASS" || { echo "FAILED"; exit 1; }
