#!/usr/bin/env bash
# =============================================================================
# ARTEX Administrator Password Reset Script
#
# The login username is fixed to ARTEX; the password is stored as a bcrypt hash in the
# auth.password_hash key of the database settings table. After connecting the database, this script
# generates a bcrypt hash in the library with pgcrypto and writes back the key — fully compatible
# with backend login validation (golang.org/x/crypto/bcrypt).
#
# There are two deployments: local (default) – The host connects to the database directly using
# psql. Connection information is obtained with the following priority: Command line parameter >
# --dsn/$ARTEX_PG_DSN > database of config.json.* docker – Runs psql in a postgres container via
# `docker compose exec` (or `docker exec`) (compose does not expose the host by default 5432, so it
# goes inside the container).
#
# Use examples:./reset-password.sh # Locally, automatically read config.json/ environment,
# interactively enter new password./reset-password.sh -p 'NewPass!' # Locally, directly specify new
# password./reset-password.sh --dsn postgres://u:p@h:5432/artex./reset-password.sh -H 127.0.0.1 -P
# 5433 -U autopentest -W pass -d artex./reset-password.sh -m docker # Docker deployment (read
# POSTGRES_* from.env)./reset-password.sh -m docker -c pg container name --exec docker
#
# The new password is passed through the environment variable + psql \getenv (not into the argv
# process) and is automatically translated using :'var' (to prevent SQL injection); the database
# password is passed through PGPASSWORD, also not into the argv.
set -euo pipefail

PASS_KEY="auth.password_hash"
BCRYPT_COST=10

MODE=""            # local | docker (empty = auto-determined)
DSN=""
HOST="" PORT="" USER="" DBPASS="" DBNAME="" SSLMODE=""
CONFIG=""
CONTAINER=""       # The default name of the postgres service/container in docker mode (default postgres)
EXEC_KIND=""       # compose | docker (which exec to use in docker mode; empty = automatic)
NEWPASS=""
ASSUME_YES=0

die() { echo "错误：$*" >&2; exit 1; }
info() { echo "· $*" >&2; }

usage() { sed -n '2,40p' "$0" | sed 's/^# \{0,1\}//'; exit 0; }

# ---- Parameter analysis -------------------------------------------------------------
while [[ $# -gt 0 ]]; do
  case "$1" in
    -m|--mode)        MODE="${2:-}"; shift 2 ;;
    --dsn)            DSN="${2:-}"; shift 2 ;;
    -H|--host)        HOST="${2:-}"; shift 2 ;;
    -P|--port)        PORT="${2:-}"; shift 2 ;;
    -U|--user)        USER="${2:-}"; shift 2 ;;
    -W|--db-password) DBPASS="${2:-}"; shift 2 ;;
    -d|--dbname)      DBNAME="${2:-}"; shift 2 ;;
    --sslmode)        SSLMODE="${2:-}"; shift 2 ;;
    --config)         CONFIG="${2:-}"; shift 2 ;;
    -c|--container)   CONTAINER="${2:-}"; shift 2 ;;
    --exec)           EXEC_KIND="${2:-}"; shift 2 ;;
    -p|--new-password) NEWPASS="${2:-}"; shift 2 ;;
    -y|--yes)         ASSUME_YES=1; shift ;;
    -h|--help)        usage ;;
    *) die "未知参数：$1（-h 查看用法）" ;;
  esac
done

# ---- Read database.* from config.json (only in local mode, and when no explicit connection is
# given)----- Parse with python3 in preference (strong); return grep if python3 is missing
# (config.json is a partition field).
read_config_json() {
  local path="$1"
  [[ -f "$path" ]] || return 1
  if command -v python3 >/dev/null 2>&1; then
    python3 - "$path" <<'PY'
import json, sys
try:
    d = json.load(open(sys.argv[1])).get("database", {})
except Exception:
    sys.exit(1)
# Support for direct to dsn, or subfield
if d.get("dsn"):
    print("DSN\t" + d["dsn"]); sys.exit(0)
for k in ("host","port","user","password","dbname","sslmode"):
    if d.get(k) is not None:
        print(k.upper() + "\t" + str(d[k]))
PY
  else
    # Simple backup: key-by-key grep (value as string or number)
    local k
    for k in host port user password dbname sslmode; do
      local v
      v=$(grep -oE "\"$k\"[[:space:]]*:[[:space:]]*(\"[^\"]*\"|[0-9]+)" "$path" 2>/dev/null \
            | head -1 | sed -E "s/.*:[[:space:]]*//; s/^\"//; s/\"$//") || true
      [[ -n "$v" ]] && echo -e "${k^^}\t$v"
    done
  fi
}

apply_config_fields() {
  local line key val
  while IFS=$'\t' read -r key val; do
    [[ -z "$key" ]] && continue
    case "$key" in
      DSN)      [[ -z "$DSN" ]] && DSN="$val" ;;
      HOST)     [[ -z "$HOST" ]] && HOST="$val" ;;
      PORT)     [[ -z "$PORT" ]] && PORT="$val" ;;
      USER)     [[ -z "$USER" ]] && USER="$val" ;;
      PASSWORD) [[ -z "$DBPASS" ]] && DBPASS="$val" ;;
      DBNAME)   [[ -z "$DBNAME" ]] && DBNAME="$val" ;;
      SSLMODE)  [[ -z "$SSLMODE" ]] && SSLMODE="$val" ;;
    esac
  done
}

# ---- Automatic judgment mode ---------------------------------------------------------
if [[ -z "$MODE" ]]; then
  if [[ -n "$DSN$HOST$USER$DBNAME" || -n "${ARTEX_PG_DSN:-}" || -f "${CONFIG:-config.json}" ]]; then
    MODE="local"
  elif command -v docker >/dev/null 2>&1 && [[ -f docker-compose.yml ]]; then
    MODE="docker"
  else
    MODE="local"
  fi
fi
info "部署模式：$MODE"

# Password: Remember me New password
if [[ -z "$NEWPASS" ]]; then
  read -r -s -p "输入新密码（用户名固定为 ARTEX）：" NEWPASS; echo >&2
  [[ -n "$NEWPASS" ]] || die "密码不能为空"
  read -r -s -p "再次输入以确认：" NEWPASS2; echo >&2
  [[ "$NEWPASS" == "$NEWPASS2" ]] || die "两次输入不一致"
fi
[[ -n "$NEWPASS" ]] || die "密码不能为空"

# Pass the password to psql via an environment variable (read by \getenv, not entered in argv/ps)
export ARTEX_RESET_NEWPASS="$NEWPASS"

# Create bcrypt in the library and upsert; password is automatically translated to :'newpw'.
SQL=$(cat <<SQL
\\set ON_ERROR_STOP on
\\getenv newpw ARTEX_RESET_NEWPASS
CREATE EXTENSION IF NOT EXISTS pgcrypto;
INSERT INTO settings(key, value)
VALUES ('$PASS_KEY', crypt(:'newpw', gen_salt('bf', $BCRYPT_COST)))
ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now();
SQL
)

# ---- Implementation ------------------------------------------------------------------
if [[ "$MODE" == "local" ]]; then
  # Connection information priority: Command line > --dsn/$ARTEX_PG_DSN > config.json
  if [[ -z "$DSN" && -z "$HOST$USER$DBNAME" ]]; then
    [[ -n "${ARTEX_PG_DSN:-}" ]] && DSN="$ARTEX_PG_DSN"
  fi
  if [[ -z "$DSN" && -z "$HOST$USER$DBNAME" ]]; then
    cfg="${CONFIG:-config.json}"
    if [[ -f "$cfg" ]]; then
      info "从 $cfg 读取数据库配置"
      apply_config_fields < <(read_config_json "$cfg")
    fi
  fi

  command -v psql >/dev/null 2>&1 || die "本机未找到 psql（请安装 postgresql-client，或改用 -m docker）"

  declare -a PSQL_ARGS=()
  if [[ -n "$DSN" ]]; then
    PSQL_ARGS=("$DSN")
    target="$DSN"
  else
    [[ -n "$USER"   ]] || die "缺少数据库用户（-U）或有效的 config.json/DSN"
    [[ -n "$DBNAME" ]] || die "缺少数据库名（-d）或有效的 config.json/DSN"
    HOST="${HOST:-127.0.0.1}"; PORT="${PORT:-5432}"; SSLMODE="${SSLMODE:-disable}"
    PSQL_ARGS=(-h "$HOST" -p "$PORT" -U "$USER" -d "$DBNAME")
    [[ -n "$SSLMODE" ]] && export PGSSLMODE="$SSLMODE"
    [[ -n "$DBPASS" ]] && export PGPASSWORD="$DBPASS"
    target="$USER@$HOST:$PORT/$DBNAME"
  fi

  info "目标数据库：$target"
  if [[ "$ASSUME_YES" -ne 1 ]]; then
    read -r -p "确认在该库重置 ARTEX 密码？[y/N] " ans
    [[ "$ans" == "y" || "$ans" == "Y" ]] || die "已取消"
  fi

  if ! printf '%s\n' "$SQL" | psql "${PSQL_ARGS[@]}" -v ON_ERROR_STOP=1 -q >/dev/null; then
    die "写入失败。若报 pgcrypto 权限/缺失，请用具备建扩展权限的角色，或先手动执行 CREATE EXTENSION pgcrypto。"
  fi

else
  # ---- docker ----
  command -v docker >/dev/null 2>&1 || die "未找到 docker"
  CONTAINER="${CONTAINER:-postgres}"

  # Choose exec mode: docker compose exec (service name) is preferred, otherwise docker exec
  # (container name)
  if [[ -z "$EXEC_KIND" ]]; then
    if docker compose version >/dev/null 2>&1 && [[ -f docker-compose.yml ]]; then
      EXEC_KIND="compose"
    else
      EXEC_KIND="docker"
    fi
  fi

  # The psql credentials in the container: first the command line, then.env's POSTGRES_*, then
  # return compose default (artex)
  if [[ -f .env ]]; then
    # shellcheck disable=SC1091
    set -a; . ./.env; set +a
  fi
  DUSER="${USER:-${POSTGRES_USER:-artex}}"
  DNAME="${DBNAME:-${POSTGRES_DB:-artex}}"
  [[ -n "$DBPASS" ]] && export PGPASSWORD="$DBPASS"
  [[ -z "${PGPASSWORD:-}" && -n "${POSTGRES_PASSWORD:-}" ]] && export PGPASSWORD="$POSTGRES_PASSWORD"

  info "目标：容器 $CONTAINER 内 psql -U $DUSER -d $DNAME（exec=$EXEC_KIND）"
  if [[ "$ASSUME_YES" -ne 1 ]]; then
    read -r -p "确认在该容器数据库重置 ARTEX 密码？[y/N] " ans
    [[ "$ans" == "y" || "$ans" == "Y" ]] || die "已取消"
  fi

  # -e Name only, no values → Inherits from the current environment, password does not appear in the
  # docker command argv.
  declare -a EXEC_CMD
  if [[ "$EXEC_KIND" == "compose" ]]; then
    EXEC_CMD=(docker compose exec -T -e ARTEX_RESET_NEWPASS -e PGPASSWORD "$CONTAINER"
              psql -U "$DUSER" -d "$DNAME" -v ON_ERROR_STOP=1 -q)
  else
    EXEC_CMD=(docker exec -i -e ARTEX_RESET_NEWPASS -e PGPASSWORD "$CONTAINER"
              psql -U "$DUSER" -d "$DNAME" -v ON_ERROR_STOP=1 -q)
  fi

  if ! printf '%s\n' "$SQL" | "${EXEC_CMD[@]}" >/dev/null; then
    die "写入失败。请确认容器名（-c）、数据库账号（.env 的 POSTGRES_*），以及角色有 pgcrypto 权限。"
  fi
fi

unset ARTEX_RESET_NEWPASS
echo "✓ 已重置 ARTEX 管理员密码。请用用户名 ARTEX + 新密码登录（无需重启服务）。"
