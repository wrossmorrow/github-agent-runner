#!/usr/bin/env bash
set -euo pipefail

REPO_ARG="${GITHUB_REPO_NAME:-}"

trap 'exit 0' TERM INT
while true; do
    /usr/local/bin/github-token.sh "${REPO_ARG}" > /tokens/token.tmp
    chmod 640 /tokens/token.tmp
    mv /tokens/token.tmp /tokens/token
    sleep 2700 & wait $!
done
