#!/bin/sh
# Build helper and runtime entrypoint for the locally built ARTEX image.
set -u

prepare_binary() {
	target="${1:-/out/artex}"
	target_dir=$(dirname "$target")

	command -v go >/dev/null 2>&1 || {
		echo "[artex] Go is required to prepare the binary" >&2
		exit 1
	}
	mkdir -p "$target_dir"

	echo "[artex] building local binary at $target"
	CGO_ENABLED=0 go build -tags embedui -trimpath -o "$target" ./cmd/artex
	chmod 0755 "$target"
}

if [ "${1:-}" = "--prepare" ]; then
	shift
	prepare_binary "${1:-/out/artex}"
	exit 0
fi

APP_DIR="${ARTEX_APP_DIR:-/app}"
cd "$APP_DIR" || exit 1

BIN="$APP_DIR/artex"
[ -x "$BIN" ] || {
	echo "[artex] executable not found: $BIN" >&2
	exit 1
}

RESTART_CODE=75
MAX_DELAY=60
child=0
stopping=0

forward() {
	stopping=1
	if [ "$child" -ne 0 ]; then
		kill -TERM "$child" 2>/dev/null || true
	fi
}
trap forward INT TERM

delay=1
while :; do
	"$BIN" "$@" &
	child=$!

	wait "$child"
	code=$?
	if [ "$code" -gt 128 ]; then
		wait "$child"
		code=$?
	fi
	child=0

	if [ "$stopping" -eq 1 ]; then
		echo "[artex] stopped"
		exit 0
	fi

	case "$code" in
		0)
			echo "[artex] exited normally"
			exit 0
			;;
		"$RESTART_CODE")
			echo "[artex] restart requested"
			delay=1
			;;
		*)
			echo "[artex] exited with code $code; restarting in ${delay}s" >&2
			sleep "$delay"
			delay=$((delay * 2))
			[ "$delay" -gt "$MAX_DELAY" ] && delay=$MAX_DELAY
			;;
	esac
done
