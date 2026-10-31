#!/bin/sh
# ARTEX Guardian Boot Script (Linux / macOS / Docker ENTRYPOINT)
#
# Use:./start.sh Run in the foreground (Ctrl-C stops) nohup./start.sh >artex.log 2>&1 & Background
# persistent./start.sh -addr :9000 Additional parameters are passed to artex as they are
#
# It does only one thing: it runs artex, and after the process exits, it uses the exit code to
# decide whether to restart.
#
# 0 User stops normally → Exit loop 75 Program requests restart → Restart immediately (page clicked
# "refresh one-click" or "roll back") Other Crash → Restart after avoidance (1→2→4…up to 60 seconds)
#
# Intentionally not downloading, SHA256 validation or re-installing here: the logics are written in
# sh and bat, and they are precisely the least likely to fail — once a binary is changed that
# doesn't run, this script will faithfully pull it over and over again, leaving the user with only a
# manual rescue on the machine. So all the validation/re-installing is left in Go (selfupdate
# package), done by artex itself at startup, keeping the script dumbfounded.
set -u

cd "$(dirname "$0")" || exit 1

BIN=./artex
[ -x "$BIN" ] || { echo "[artex] 找不到可执行文件 $BIN" >&2; exit 1; }

RESTART_CODE=75
MAX_DELAY=60

child=0
stopping=0

# Forward the stop signal to the artex body.
#
# This is necessary in Docker: docker stop only sends SIGTERM to PID 1 (this script), not to the
# child process. If it is not forwarded, artex cannot receive the signal, fails to shut down
# elegantly, and is SIGKILL after 10 seconds, terminating the running task directly halfway.
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

	# The signal interrupts wait and returns >128. At this point the subprocess is actually still
	# doing an elegant shutdown, and must wait again to get its real exit code.
	wait "$child"
	code=$?
	if [ "$code" -gt 128 ]; then
		wait "$child"
		code=$?
	fi
	child=0

	if [ "$stopping" -eq 1 ]; then
		echo "[artex] 已停止"
		exit 0
	fi

	case "$code" in
		0)
			echo "[artex] 正常退出"
			exit 0
			;;
		"$RESTART_CODE")
			# Update/rollback ready: After rebooting, artex will complete the update at startup (see
			# selfupdate.Bootstrap).
			echo "[artex] 请求重启（应用新版本）…"
			delay=1
			;;
		*)
			echo "[artex] 异常退出 (code=$code)，${delay}s 后重启" >&2
			sleep "$delay"
			delay=$((delay * 2))
			[ "$delay" -gt "$MAX_DELAY" ] && delay=$MAX_DELAY
			;;
	esac
done
