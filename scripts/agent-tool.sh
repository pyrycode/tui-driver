#!/bin/sh
set -eu
ROOT="$(cd -P "$(dirname "$0")/.." && pwd)"
AGENTS="${AGENTS_REPO_PATH:-$ROOT/../tui-driver-agents}"
if [ ! -x "$AGENTS/bin/tui-tool" ]; then
  echo "Install tui-driver-agents beside this checkout or set AGENTS_REPO_PATH." >&2
  exit 2
fi
cd "$ROOT"
exec "$AGENTS/bin/tui-tool" "$@"
