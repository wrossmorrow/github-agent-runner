#!/usr/bin/env bash
set -euo pipefail

GITHUB_APP_ID="${GITHUB_APP_ID:?}"
GITHUB_APP_INSTALLATION_ID="${GITHUB_APP_INSTALLATION_ID:?}"
GITHUB_REPO_NAME="${1:-}"
KEY="${GITHUB_APP_PRIVATE_KEY_PATH:-/run/secrets/gh-app/private-key.pem}"

b64() {
    openssl base64 -A | tr '+/' '-_' | tr -d '='
}

now=$(date +%s)
header=$(printf '{"alg":"RS256","typ":"JWT"}' | b64)
payload=$(printf '{"iat":%d,"exp":%d,"iss":"%s"}' $((now - 60)) $((now + 540)) "${GITHUB_APP_ID}" | b64)
sig=$(printf '%s.%s' "$header" "$payload" | openssl dgst -sha256 -sign "${KEY}" -binary | b64)

body='{}'
if [ -n "${GITHUB_REPO_NAME}" ]; then
    just_repo="${GITHUB_REPO_NAME##*/}"
    body=$(printf '{"repositories":["%s"]}' "${just_repo}")
fi

curl -fsS -X POST \
    -H "Authorization: Bearer ${header}.${payload}.${sig}" \
    -H "Accept: application/vnd.github+json" \
    -d "${body}" \
    "https://api.github.com/app/installations/${GITHUB_APP_INSTALLATION_ID}/access_tokens" \
    | jq -r '.token'
