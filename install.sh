#!/usr/bin/env bash
#
# groundcontrol installer — one command from zero to a running tower.
#
#   curl -fsSL https://raw.githubusercontent.com/connorbell133/groundcontrol/main/install.sh | bash
#
# What it does:
#   1. checks the prerequisites (macOS/Linux, git, Go 1.24+; warns if the
#      Claude Code CLI isn't logged in yet)
#   2. installs the binary — `go install …@latest`, or `go build` when run
#      from inside a clone
#   3. scaffolds ~/.groundcontrol/config.json with a generated auth token
#      (never overwrites an existing config)
#   4. offers to start the tower right away
#
# It never needs sudo and touches nothing outside Go's bin dir and
# ~/.groundcontrol.

set -euo pipefail

MODULE="github.com/connorbell133/groundcontrol"
APP_HOME="${HOME}/.groundcontrol"
CONFIG="${APP_HOME}/config.json"

say()  { printf '\033[1m▸ %s\033[0m\n' "$*"; }
note() { printf '  %s\n' "$*"; }
warn() { printf '\033[33m  ⚠ %s\033[0m\n' "$*"; }
die()  { printf '\033[31m✗ %s\033[0m\n' "$*" >&2; exit 1; }

# ── 1. prerequisites ─────────────────────────────────────────────────────────

case "$(uname -s)" in
  Darwin|Linux) ;;
  *) die "groundcontrol supports macOS and Linux only (the Go PTY layer has no Windows support)." ;;
esac

command -v git >/dev/null 2>&1 || die "git is required — install it and re-run."

command -v go >/dev/null 2>&1 || die "Go 1.24+ is required — install it from https://go.dev/dl (or 'brew install go') and re-run."

goversion="$(go env GOVERSION)"   # e.g. go1.24.7
gominor="$(printf '%s' "$goversion" | sed -E 's/^go1\.([0-9]+).*/\1/')"
case "$gominor" in
  ''|*[!0-9]*) warn "could not parse Go version '$goversion' — continuing, but 1.24+ is required." ;;
  *) [ "$gominor" -ge 24 ] || die "Go 1.24+ is required, found $goversion. Update Go and re-run." ;;
esac

if ! command -v claude >/dev/null 2>&1; then
  warn "the Claude Code CLI ('claude') isn't on PATH. groundcontrol needs it at runtime,"
  warn "logged in with full credentials (not an API key): https://claude.com/claude-code"
fi

# ── 2. install the binary ────────────────────────────────────────────────────

bindir="$(go env GOBIN)"
[ -n "$bindir" ] || bindir="$(go env GOPATH)/bin"

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]:-.}")" 2>/dev/null && pwd)"
if [ -f "${script_dir}/go.mod" ] && head -1 "${script_dir}/go.mod" | grep -q "$MODULE"; then
  say "Building from local clone (${script_dir})…"
  (cd "$script_dir" && go build -o "${bindir}/groundcontrol" ./cmd/groundcontrol)
else
  say "Installing ${MODULE}@latest…"
  go install "${MODULE}/cmd/groundcontrol@latest"
fi
note "installed ${bindir}/groundcontrol"

case ":${PATH}:" in
  *":${bindir}:"*) ;;
  *)
    warn "${bindir} is not on your PATH. Add this to your shell profile:"
    warn "  export PATH=\"\$PATH:${bindir}\""
    ;;
esac

# ── 3. scaffold the config ───────────────────────────────────────────────────

mkdir -p "$APP_HOME"

if [ -f "$CONFIG" ]; then
  say "Keeping existing config at ${CONFIG}"
  token=""
else
  if command -v openssl >/dev/null 2>&1; then
    token="$(openssl rand -hex 16)"
  else
    token="$(od -An -tx1 -N16 /dev/urandom | tr -d ' \n')"
  fi

  # Guess a starting root: the first conventional code directory that exists,
  # falling back to $HOME. Editable any time in the app's Settings sheet.
  root="$HOME"
  for candidate in "$HOME/repos" "$HOME/code" "$HOME/projects" "$HOME/dev" "$HOME/src"; do
    if [ -d "$candidate" ]; then root="$candidate"; break; fi
  done

  say "Writing ${CONFIG}"
  cat > "$CONFIG" <<EOF
{
  "port": 3020,
  "host": "0.0.0.0",
  "roots": [
    "${root}"
  ],
  "showHidden": false,
  "webhooks": [],
  "jobs": { "concurrency": 2, "timeoutMs": 900000 },
  "authToken": "${token}",
  "tokens": []
}
EOF
  chmod 600 "$CONFIG"
  note "browsable root: ${root} (change it in the app's Settings, or edit the file)"
fi

# Something else already on the port (a stale older groundcontrol, say) makes
# for a confusing half-working UI — call it out before offering to launch.
port="$(sed -nE 's/.*"port"[[:space:]]*:[[:space:]]*([0-9]+).*/\1/p' "$CONFIG" | head -1)"
[ -n "$port" ] || port=3020
if command -v lsof >/dev/null 2>&1 && lsof -iTCP:"$port" -sTCP:LISTEN >/dev/null 2>&1; then
  warn "something is already listening on port ${port}:"
  lsof -iTCP:"$port" -sTCP:LISTEN 2>/dev/null | tail -n +2 | awk '{printf "  ⚠   %s (pid %s)\n", $1, $2}' >&2
  warn "stop it first (or change \"port\" in ${CONFIG}), or the app will talk to the wrong server."
fi

# ── 4. done — run it ─────────────────────────────────────────────────────────

echo
say "groundcontrol is installed."
if [ -n "${token}" ]; then
  note "your auth token (the app asks for it on first open):"
  echo
  printf '      %s\n' "$token"
  echo
fi
note "to start the tower:    cd ${APP_HOME} && groundcontrol"
note "then open:             http://localhost:3020"
note "phone access (nice):   tailscale serve --bg 3020"
note "  (one option among several — plain LAN, another VPN, your own reverse proxy: see README#reaching-it-from-your-phone)"
echo

# Only offer to launch when a human is attached (curl | bash keeps stdin busy,
# so ask on /dev/tty).
if { : < /dev/tty; } 2>/dev/null; then
  printf '\033[1m▸ Start it now? [Y/n] \033[0m' > /dev/tty
  read -r reply < /dev/tty || reply="n"
  case "$reply" in
    n|N|no|No) note "later, then: cd ${APP_HOME} && groundcontrol" ;;
    *)
      say "Launching from ${APP_HOME} (Ctrl-C to stop)…"
      cd "$APP_HOME"
      exec "${bindir}/groundcontrol"
      ;;
  esac
fi
