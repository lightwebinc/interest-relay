#!/usr/bin/env bash
# End-to-end test of the relay's unicast distributor, on one machine.
#
# It builds a small routed IPv6 network out of network namespaces and runs
# the real binaries in it:
#
#   edge ══ ce ── rtr ─┬─ h1
#                      ├─ h2
#                      └─ h3
#
#   edge  a stand-in for the delivery edge (interest-terminus), and the
#         consumer's lane: lane-feed writes delivery records to the ce
#         relay's lane address, as the edge does to a provisioned lane
#   ce    the relay at the customer edge, distributing: it ends the lane and
#         sends each record on to the hosts that announced its topic
#   rtr   a plain router; it knows nothing of interest
#   h1-2  hosts one IP hop past the relay: each a lane (lane-sink) and an
#         announcer that names it as its delivery address
#   h3    an announcer that names an address it may not have delivered to
#
# Everything lives in a private user, network and mount namespace and is
# gone when the script exits. It needs unprivileged user namespaces, or
# root. Addresses are from the documentation prefix.
#
#   test/netns/distribute.sh            run the test
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
stop() { # name
	local v="pid_$1"
	kill -TERM "${!v}" 2>/dev/null || true
	wait "${!v}" 2>/dev/null || true
}
metric() { # name
	ip netns exec ce curl -fsS "http://[::1]:9600/metrics" | awk -v m="$1" '$1==m {print $2}'
}
edge_field() { ip netns exec edge curl -fsS "http://[::1]:9700/state" | python3 -c "import json,sys; d=json.load(sys.stdin); print($1)"; }
sink() { # host topic   (from the sink's last line)
	grep '^# records' "$work/sink_$1.log" | tail -1 | tr ' ' '\n' | awk -F= -v s="$2" '$1==s {print $2}' | grep . || echo 0
}
announced() { # name status   (from the announcer's last summary line)
	grep '^# topics' "$work/$1.log" | tail -1 | tr ' ' '\n' | awk -F= -v s="$2" '$1==s {print $2}' | grep . || echo 0
}
feed() { # records per topic
	ip netns exec edge "$BIN/lane-feed" -to "[$CE_UP]:9171" -n "$1" -size 512 tm_a tm_b tm_c tm_e >>"$work/feed.log"
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

echo "== start: edge, a distributing relay, two hosts with lanes, one with a foreign address"
start edge edge "$BIN/interest-terminus" -listen "[$EDGE]:8729" -http "[::1]:9700" "${L[@]}" -quiet
start ce ce "$BIN/interest-relay" -listen "[$CE]:8729" -upstream "[$EDGE]:8729" -allow 2001:db8:c::/48 \
	-metrics-addr "[::1]:9600" -sender-label ce "${L[@]}" \
	-distribute-listen "[$CE_UP]:9171" -distribute-advertise "[$CE_UP]:9171"
start sink_h1 h1 "$BIN/lane-sink" -listen "[$H1]:9171" -names tm_a,tm_b,tm_c,tm_e -report 200ms
start sink_h2 h2 "$BIN/lane-sink" -listen "[$H2]:9171" -names tm_a,tm_b,tm_c,tm_e -report 200ms
start h1 h1 "$BIN/interest-announce" -upstream "[$CE]:8729" -sender-label h1 -deliver-to "[$H1]:9171" -quiet -report 500ms tm_a tm_b
start h2 h2 "$BIN/interest-announce" -upstream "[$CE]:8729" -sender-label h2 -deliver-to "[$H2]:9171" -quiet -report 500ms tm_b tm_c
start h3 h3 "$BIN/interest-announce" -upstream "[$CE]:8729" -sender-label h3 -deliver-to "[2001:db8:dead::1]:9171" -quiet -report 500ms tm_d

check "relay: delivery addresses honored" "metric bir_delivery_addresses" 4
check "relay: routes" "metric bir_distributor_routes" 4
check "h1: topics announced and honored" "announced h1 ok" 2
check "h3: an address outside its prefix is refused" "announced h3 bad_address" 1
check "edge: the union, delivered as provisioned" "edge_field 'd[\"live\"]'" 3

echo "== the lane: 200 records each of tm_a, tm_b, tm_c and tm_e (which nobody asked for)"
feed 200
check "relay: records in" "metric bir_distributor_records_in_total" 800
check "relay: records nobody asked for" "metric bir_distributor_records_unrouted_total" 200
check "h1: tm_a" "sink h1 tm_a" 200
check "h1: tm_b" "sink h1 tm_b" 200
check "h1: nothing else" "echo \$(( \$(sink h1 tm_c) + \$(sink h1 tm_e) + \$(sink h1 other) ))" 0
check "h2: tm_b" "sink h2 tm_b" 200
check "h2: tm_c" "sink h2 tm_c" 200
check "h2: nothing else" "echo \$(( \$(sink h2 tm_a) + \$(sink h2 tm_e) + \$(sink h2 other) ))" 0

echo "== h1 leaves and says so: its routes go"
stop h1
check "relay: delivery addresses" "metric bir_delivery_addresses" 2
feed 200
check "relay: records in" "metric bir_distributor_records_in_total" 1600
check "h2: tm_b" "sink h2 tm_b" 400
check "h2: tm_c" "sink h2 tm_c" 400
check "h1: nothing new" "echo \$(( \$(sink h1 tm_a) + \$(sink h1 tm_b) ))" 400

echo "== h2's lane goes down: its records are shed, the relay carries on"
stop sink_h2
feed 50
check "relay: records in" "metric bir_distributor_records_in_total" 1800
shed=$(metric "bir_distributor_host_records_shed_total{host=\"[$H2]:9171\"}")
check "relay: records for h2 shed" "[ \"\$(metric 'bir_distributor_host_records_shed_total{host=\"[$H2]:9171\"}')\" -gt 0 ] && echo yes" yes
echo "      (shed for h2 at the first look: ${shed:-0})"
check "relay: still ready" "metric bir_ready" 1

if [ "$failed" = 0 ]; then echo PASS; else echo FAIL; fi
exit "$failed"
