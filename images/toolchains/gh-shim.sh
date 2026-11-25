#!/usr/bin/env bash
set -euo pipefail

# Use the github-auth sidecar's current token when present; otherwise keep GH_TOKEN.
if [ -r /tokens/token ]; then
    GH_TOKEN="$(cat /tokens/token)"
    export GH_TOKEN
fi
exec /usr/local/lib/gh/gh "$@"
