#!/usr/bin/env bash
# analytics_api_test.sh
# Usage: ./analytics_api_test.sh [base_url] [private_key_pem] [key_id]
# Example: ./analytics_api_test.sh http://localhost:8080 analytics-ed25519.pem data-team
#
# Requests are signed with Ed25519 (see docs/send-api-ed25519.md, analytics
# section). The public key must be set as ed25519_public_key under
# api.analytics.api_keys.<key_id> in the bot config.
#
# Requirements: curl, OpenSSL 3+

set -euo pipefail

BASE_URL="${1:-http://localhost:8080}"
KEY="${2:-analytics-ed25519.pem}"
KEY_ID="${3:-data-team}"

# do_get PATH QUERY [TIMESTAMP]
do_get() {
  local path="$1" query="$2" ts="${3:-$(date +%s)}" nonce hash msg sig uri
  nonce=$(openssl rand -hex 16)
  hash=$(printf '' | openssl dgst -sha256 -r | cut -d' ' -f1)
  msg=$(mktemp)
  printf 'BDB-ANALYTICS-ED25519-V1\nGET\n%s\n%s\n%s\n%s\n%s' "$path" "$query" "$ts" "$nonce" "$hash" > "$msg"
  sig=$(openssl pkeyutl -sign -rawin -inkey "$KEY" -in "$msg" | base64 | tr -d '\r\n')
  rm -f "$msg"
  uri="${BASE_URL}${path}"
  [[ -n "$query" ]] && uri="${uri}?${query}"
  curl -sS \
    -H "X-Key-ID: $KEY_ID" \
    -H "X-Timestamp: $ts" \
    -H "X-Nonce: $nonce" \
    -H "X-Signature: $sig" \
    "$uri"
}

echo "### Test 1: /api/v1/analytics/transactions (example date range)"
do_get "/api/v1/analytics/transactions" "start_date=2026-01-01&end_date=2026-02-01&limit=10"
echo -e "\n\n"

echo "### Test 2: /api/v1/analytics/transactions (incoming only)"
do_get "/api/v1/analytics/transactions" "payment_type=incoming&include_external=true&include_internal=false&limit=10"
echo -e "\n\n"

echo "### Test 3: /api/v1/analytics/user/{user_id}/transactions"
USER_ID=123456789
do_get "/api/v1/analytics/user/${USER_ID}/transactions" "limit=10"
echo -e "\n\n"

echo "### Test 4: expired timestamp (expected 401)"
do_get "/api/v1/analytics/transactions" "start_date=2026-01-01" $(($(date +%s) - 1000)) || true
echo -e "\n\n"

echo "### Done."
