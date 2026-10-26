#!/bin/sh
# Emulates the link in front of the client, then runs the command.
# RATE (default 1gbit, empty = unlimited) and RTT (e.g. 20ms, default 0)
# go on this container's egress only: data packets are delayed, ACKs are
# not, so the round trip is RTT. netem's default queue of 1000 packets is
# smaller than 1 Gbit/s × 50 ms, and drops from it would be measured as a
# slow link, hence the large limit.
set -eu
RATE=${RATE-1gbit}
RTT=${RTT:-0}
args=""
[ "$RTT" != 0 ] && args="delay $RTT"
[ -n "$RATE" ] && args="$args rate $RATE"
if [ -n "$args" ]; then
	# shellcheck disable=SC2086
	tc qdisc replace dev eth0 root netem $args limit 200000
fi
echo "link: $(tc qdisc show dev eth0 | head -1)" >&2
ping -c 3 -q cloud 2>&1 | tail -1 >&2 || true
exec "$@"
