#!/bin/sh
# WebDAV compatibility check (E7-5): litmus and rclone against a running
# server. Meant for a throwaway one (make test-webdav, CI job
# backend:webdav): it signs in as USERNAME, makes an app password and
# leaves what it uploads in that account, trash included.
#
#   BASE_URL=http://cloud:8080 USERNAME=webdav PASSWORD=… webdav-compat.sh
#
# Needs curl, litmus and rclone. Exits non-zero when litmus fails a test
# outside KNOWN_FAILURES, or rclone finds the copy differs from the source.
set -eu

: "${BASE_URL:?}" "${USERNAME:?}" "${PASSWORD:?}"
BASE_URL=${BASE_URL%/}
DAV="$BASE_URL/dav/files/$USERNAME"
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

# Tests litmus fails on purpose or by a known, harmless gap
# (docs/webdav-clients.md has why). One that starts passing is reported, so
# the list doesn't outlive the gap.
KNOWN_FAILURES="
props/propfind_invalid2
locks/lock_shared
"

for i in $(seq 60); do
	curl -fsS "$BASE_URL/readyz" >/dev/null 2>&1 && break
	[ "$i" = 60 ] && { echo "server not ready at $BASE_URL" >&2; exit 1; }
	sleep 1
done

# A sign-in session only to make the app password: WebDAV takes nothing else.
curl -fsS -c "$WORK/jar" -H 'Content-Type: application/json' \
	-d "{\"username\":\"$USERNAME\",\"password\":\"$PASSWORD\"}" "$BASE_URL/api/v1/auth/login" >/dev/null
APP_PASSWORD=$(curl -fsS -b "$WORK/jar" -H 'Content-Type: application/json' \
	-d '{"name":"webdav-compat"}' "$BASE_URL/api/v1/me/app-passwords" |
	sed -n 's/.*"password":"\([^"]*\)".*/\1/p')
[ -n "$APP_PASSWORD" ] || { echo "no app password in the answer" >&2; exit 1; }

status=0

echo "=== litmus"
# One suite per run: litmus stops at the first suite with a failure.
mkdir "$WORK/litmus"
for suite in basic copymove props locks http; do
	# Each run starts from a fresh litmus/ collection under the root.
	# Its progress redraws a line with \r: each redraw becomes a line.
	# In $WORK: litmus leaves debug.log and child.log where it runs.
	(cd "$WORK/litmus" && TESTS=$suite litmus "$DAV/" "$USERNAME" "$APP_PASSWORD" 2>&1) | tr '\r' '\n' >"$WORK/litmus/$suite.log" || true
	summary=$(grep -E '^<- summary for' "$WORK/litmus/$suite.log" || echo "<- no summary for $suite")
	echo "$summary"
	failed=$(sed -n 's/^ *[0-9][0-9]*\. \([a-z0-9_]*\)\.* *FAIL.*/\1/p' "$WORK/litmus/$suite.log")
	for t in $failed; do
		case "$KNOWN_FAILURES" in
		*"
$suite/$t
"*) echo "   known failure: $suite/$t" ;;
		*)
			echo "   NEW FAILURE: $suite/$t" >&2
			grep -E "^ *[0-9]+\. $t\.+" -A3 "$WORK/litmus/$suite.log" >&2 || true
			status=1
			;;
		esac
	done
	for k in $KNOWN_FAILURES; do
		case "$k" in "$suite/"*) ;; *) continue ;; esac
		echo "$failed" | grep -qx "${k#*/}" || echo "   now passes, drop from KNOWN_FAILURES: $k"
	done
	if ! grep -q '^<- summary for' "$WORK/litmus/$suite.log"; then
		cat "$WORK/litmus/$suite.log" >&2
		status=1
	fi
done

echo "=== rclone"
export RCLONE_CONFIG_CLOUD_TYPE=webdav RCLONE_CONFIG_CLOUD_URL="$DAV" RCLONE_CONFIG_CLOUD_VENDOR=other \
	RCLONE_CONFIG_CLOUD_USER="$USERNAME"
RCLONE_CONFIG_CLOUD_PASS=$(rclone obscure "$APP_PASSWORD")
export RCLONE_CONFIG_CLOUD_PASS
rc() { rclone --config /dev/null --retries 1 --low-level-retries 2 "$@"; }

# Names a sync client meets in real life, a nested tree and one file big
# enough to be streamed rather than fit in a buffer.
src="$WORK/src"
mkdir -p "$src/Документы/Отчёты 2026" "$src/emoji 🚀" "$src/deep/a/b/c/d"
for i in $(seq 1 50); do echo "file $i" >"$src/deep/a/b/c/d/f$i.txt"; done
echo "отчёт" >"$src/Документы/Отчёты 2026/итоги года.txt"
echo "space" >"$src/name with  spaces.txt"
echo "rocket" >"$src/emoji 🚀/🚀.txt"
echo "odd" >"$src/a#b%c&d+e.txt"
head -c 52428800 /dev/urandom >"$src/big.bin"

step() {
	name=$1
	shift
	if "$@" >"$WORK/rclone.log" 2>&1; then
		echo "ok   $name"
	else
		echo "FAIL $name" >&2
		cat "$WORK/rclone.log" >&2
		status=1
	fi
}

step "copy" rc copy "$src" cloud:rclone
step "check" rc check "$src" cloud:rclone
step "check --download" rc check --download "$src" cloud:rclone
echo "changed" >"$src/name with  spaces.txt"
echo "new" >"$src/Документы/новый.txt"
rm "$src/deep/a/b/c/d/f1.txt"
step "sync (change, add, delete)" rc sync "$src" cloud:rclone
step "check after sync" rc check --download "$src" cloud:rclone
step "moveto folder" rc moveto cloud:rclone/deep cloud:rclone/moved
mv "$src/deep" "$src/moved"
step "check after moveto" rc check "$src" cloud:rclone

exit $status
