#!/bin/sh
set -eu

version=2.0.4
expected=e209dda5c8235479f3166defc7750e1dbcd5a5c1808b7792fc2e6733768fb447
destination=internal/web/assets/htmx.min.js
temporary="${destination}.tmp"

checksum() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | cut -d ' ' -f 1
  else
    shasum -a 256 "$1" | cut -d ' ' -f 1
  fi
}

if [ -f "$destination" ] && [ "$(checksum "$destination")" = "$expected" ]; then
  exit 0
fi

trap 'rm -f "$temporary"' EXIT
curl -fsSL "https://unpkg.com/htmx.org@${version}/dist/htmx.min.js" -o "$temporary"
actual=$(checksum "$temporary")
if [ "$actual" != "$expected" ]; then
  echo "HTMX checksum mismatch: expected $expected, got $actual" >&2
  exit 1
fi
mv "$temporary" "$destination"
trap - EXIT
