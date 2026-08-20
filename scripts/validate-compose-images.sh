#!/bin/sh
set -eu

usage() {
	echo "usage: docker compose config --images | $0 <expected-service-count>" >&2
	exit 2
}

validate_digest() {
	value=$1
	if ! printf '%s\n' "$value" | grep -Eq '^[^[:space:]@]+@sha256:[[:xdigit:]]{64}$'; then
		echo "every Compose service image must use an immutable image@sha256:<64-hex-digest> reference" >&2
		exit 1
	fi
}

[ "$#" -eq 1 ] || usage
expected=$1
case "$expected" in
	*[!0-9]*|'') usage ;;
esac

count=0
while IFS= read -r image; do
	[ -n "$image" ] || continue
	validate_digest "$image"
	count=$((count + 1))
done

if [ "$count" -ne "$expected" ]; then
	echo "expected $expected Compose service images, found $count" >&2
	exit 1
fi
