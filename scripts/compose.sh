#!/bin/sh
set -eu

if [ "$#" -lt 2 ]; then
	echo "usage: $0 sqlite|postgres <docker compose arguments...>" >&2
	exit 2
fi

mode=$1
shift
script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
project_dir=$(dirname -- "$script_dir")
case "$mode" in
	sqlite)
		compose_file=$project_dir/docker-compose.example.yml
		expected_images=2
		;;
	postgres)
		compose_file=$project_dir/docker-compose.postgres.example.yml
		expected_images=3
		;;
	*)
		echo "usage: $0 sqlite|postgres <docker compose arguments...>" >&2
		exit 2
		;;
esac

images=$(docker compose -f "$compose_file" config --images)
printf '%s\n' "$images" | "$script_dir/validate-compose-images.sh" "$expected_images"
exec docker compose -f "$compose_file" "$@"
