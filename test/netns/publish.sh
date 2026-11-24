#!/usr/bin/env bash
# End-to-end test of the relay as its domain's publish point, on one
# machine.
#
# It builds a small routed IPv6 network out of network namespaces and runs
# the real binaries in it:
#
#   edge ══ ce ── rtr ─┬─ h1
#                      ├─ h2
#                      └─ h3
#
#   edge  a stand-in for the delivery edge: interest-terminus for interest,
#         and lane-sink -submissions as its ingress, which records the
#         address each submission came from
#   ce    the relay at the customer edge, distributing and publishing: the
#         hosts publish to it, it sends each submission up to the ingress
#         from the consumer's own address and copies it at once to the
#         siblings that asked for its topic
#   rtr   a plain router; it knows nothing of interest
#   h1-3  hosts one IP hop past the relay: each a lane (lane-sink), an
#         announcer that names it as its delivery address, and a publisher
#         (lane-feed -submit) from the same address
#
# Everything lives in a private user, network and mount namespace and is
# gone when the script exits. It needs unprivileged user namespaces, or
# root. Addresses are from the documentation prefix.
#
#   test/netns/publish.sh            run the test
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
EDGE=2001:db8:f::1 CE_UP=2001:db8:f::2 CE=2001:db8:b::1 RTR=2001:db8:b::2
H1=2001:db8:c:1::10 H2=2001:db8:c:2::10 H3=2001:db8:c:3::10

for n in edge ce rtr h1 h2 h3; do
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
link ce c1 $CE/64 rtr r0 $RTR/64
link rtr r1 2001:db8:c:1::1/64 h1 eth0 $H1/64
link rtr r2 2001:db8:c:2::1/64 h2 eth0 $H2/64
link rtr r3 2001:db8:c:3::1/64 h3 eth0 $H3/64
ip netns exec rtr sysctl -qw net.ipv6.conf.all.forwarding=1
for i in 1 2 3; do ip -n "h$i" route add default via "2001:db8:c:$i::1"; done
ip -n ce route add 2001:db8:c::/48 via $RTR
for h in h1 h2 h3; do
	for try in 1 2 3 4 5; do
		ip netns exec "$h" ping -6 -c 1 -W 3 $CE >/dev/null 2>&1 && break
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
metric() { # name
	ip netns exec ce curl -fsS "http://[::1]:9600/metrics" | awk -v m="$1" '$1==m {print $2}'
}
field() { # log field   (from the sink's last summary line)
	grep '^# records' "$work/$1.log" | tail -1 | tr ' ' '\n' | awk -F= -v s="$2" '$1==s {print $2}' | grep . || echo 0
}
publish() { # host address records topic...
	local h=$1 a=$2 n=$3
	shift 3
	ip netns exec "$h" "$BIN/lane-feed" -submit -from "$a" -to "[$CE]:8725" -n "$n" -size 512 "$@" >>"$work/feed_$h.log"
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

echo "== start: edge with an ingress, a relay that distributes and publishes, three hosts"
start edge edge "$BIN/interest-terminus" -listen "[$EDGE]:8729" -http "[::1]:9700" "${L[@]}" -quiet
start ingress edge "$BIN/lane-sink" -submissions -listen "[$EDGE]:8725" -names tm_a,tm_b -report 200ms
start ce ce "$BIN/interest-relay" -listen "[$CE]:8729" -upstream "[$EDGE]:8729" -allow 2001:db8:c::/48 \
	-metrics-addr "[::1]:9600" -sender-label ce "${L[@]}" \
	-distribute-listen "[$CE_UP]:9171" -distribute-advertise "[$CE_UP]:9171" \
	-publish-listen "[$CE]:8725" -publish-ingress "[$EDGE]:8725" -publish-source "$CE_UP" -publish-max-object-mib 1
for i in 1 2 3; do
	a="H$i"
	start "sink_h$i" "h$i" "$BIN/lane-sink" -listen "[${!a}]:9171" -names tm_a,tm_b -report 200ms
done
start h1 h1 "$BIN/interest-announce" -upstream "[$CE]:8729" -sender-label h1 -deliver-to "[$H1]:9171" -quiet -report 500ms tm_a
start h2 h2 "$BIN/interest-announce" -upstream "[$CE]:8729" -sender-label h2 -deliver-to "[$H2]:9171" -quiet -report 500ms tm_a tm_b
start h3 h3 "$BIN/interest-announce" -upstream "[$CE]:8729" -sender-label h3 -deliver-to "[$H3]:9171" -quiet -report 500ms tm_b
check "relay: routes" "metric bir_distributor_routes" 4

echo "== h1 publishes 100 records of tm_a"
publish h1 $H1 100 tm_a
check "ingress: tm_a, sent up" "field ingress tm_a" 100
check "ingress: every record from the consumer's own address" "field ingress sources" $CE_UP
check "h2 (asked for tm_a): copied at once" "field sink_h2 tm_a" 100
check "h3 (asked for tm_b only): nothing yet" "field sink_h3 total" 0 3
check "relay: accepted" "metric bir_publish_accepted_total" 100
check "relay: confirmed by the ingress" "metric bir_publish_confirmed_total" 100 30
check "relay: copies" "metric bir_publish_copies_total" 100

echo "== h2 publishes 50 records of tm_b and 50 of tm_a"
publish h2 $H2 50 tm_b tm_a
check "ingress: tm_b" "field ingress tm_b" 50
check "ingress: tm_a" "field ingress tm_a" 150
check "h3 (asked for tm_b): tm_b" "field sink_h3 tm_b" 50
check "h1 (asked for tm_a): h2's tm_a" "field sink_h1 tm_a" 50
check "relay: copies" "metric bir_publish_copies_total" 200

echo "== an object over the bound, then a record, on one stream: the first is skipped, the stream goes on"
ip netns exec h1 "$BIN/lane-feed" -submit -from $H1 -to "[$CE]:8725" -n 2 -lead-size $((2 << 20)) -size 512 tm_a >>"$work/feed_h1.log"
check "relay: oversize refused" "metric 'bir_publish_refused_total{reason=\"oversize\"}'" 1
check "ingress: the next record on the same stream arrives" "field ingress tm_a" 151
check "h2: and is copied" "field sink_h2 tm_a" 101

echo "== settled: nothing anyone published came back to it"
sleep 2
check "h1, in all: h2's 50 tm_a, none of its own 101" "field sink_h1 total" 50 1
check "h2, in all: h1's 101 tm_a, none of its own 100" "field sink_h2 total" 101 1
check "h3, in all: h2's 50 tm_b, nothing of tm_a" "field sink_h3 total" 50 1
check "ingress: every record from the consumer's own address" "field ingress sources" $CE_UP 1
check "relay: still ready" "metric bir_ready" 1
if [ "$failed" = 0 ]; then echo PASS; else echo FAIL; fi
exit "$failed"
