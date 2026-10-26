#!/bin/sh
# Fails if tracked files contain private-looking infrastructure details
# (SECURITY.md §7, §8): Tailscale names, private IP ranges outside tests,
# or strings shaped like real Telegram bot tokens.
set -u
cd "$(git rev-parse --show-toplevel)"
status=0

check() {
	description=$1
	pattern=$2
	shift 2
	if git grep -n -I -E "$pattern" -- "$@"; then
		echo "error: found $description (see SECURITY.md §7)" >&2
		status=1
	fi
}

self=':!scripts/check-infra.sh'

check "a Tailscale name" '[A-Za-z0-9-]+\.ts\.net' . "$self"
check "a real-looking bot token" '[0-9]{8,10}:[A-Za-z0-9_-]{35}' . "$self"
check "a private IP address in non-test code" \
	'(^|[^0-9.])(10\.[0-9]{1,3}|192\.168|172\.(1[6-9]|2[0-9]|3[01])|100\.(6[4-9]|[7-9][0-9]|1[01][0-9]|12[0-7]))\.[0-9]{1,3}\.[0-9]{1,3}([^0-9.]|$)' \
	. "$self" ':!*_test.go' ':!**/testdata/**' ':!go.sum'

exit $status
