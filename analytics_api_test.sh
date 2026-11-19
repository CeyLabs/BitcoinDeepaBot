#!/usr/bin/env bash
# analytics_api_test.sh
# Usage: ./analytics_api_test.sh [base_url] [hmac_secret]
# Example: ./analytics_api_test.sh http://localhost:8080 myanalyticssecret

set -euo pipefail

BASE_URL="${1:-http://localhost:8080}"
HMAC_SECRET="${2:-your_analytics_hmac_secret}"

# Compute HMAC-SHA256 hex
hmac_hex() {
  local message="$1"
  printf '%s' "$message" | openssl dgst -sha256 -hmac "$HMAC_SECRET" -binary | xxd -p -c 256
}

# Build headers: X-Timestamp and X-HMAC-Signature
build_headers() {
  local method="$1"
  local path="$2"
  local query="$3"
  local timestamp
  timestamp=$(date +%s)

  local message="${method}${path}${timestamp}${query}"
  local signature
  signature=$(hmac_hex "$message")

  echo "$timestamp" "$signature"
}

# Send GET request
do_get() {
  local path="$1"
  local query="$2"

  local uri="${BASE_URL}${path}"
  if [[ -n "$query" ]]; then
    uri="${uri}?${query}"
  fi

  read -r ts sig < <(build_headers GET "$path" "$query")

  curl -sS \
    -H "X-Timestamp: $ts" \
    -H "X-HMAC-Signature: $sig" \
    "$uri"
}


echo "### Test 1: /api/v1/analytics/transactions (example date range)"
QUERY1="start_date=2026-01-01&end_date=2026-02-01&limit=10"
do_get "/api/v1/analytics/transactions" "$QUERY1"
echo -e "\n\n"

echo "### Test 2: /api/v1/analytics/transactions (incoming only)"
QUERY2="payment_type=incoming&include_external=true&include_internal=false&limit=10"
do_get "/api/v1/analytics/transactions" "$QUERY2"
echo -e "\n\n"

echo "### Test 3: /api/v1/analytics/user/{user_id}/transactions"
USER_ID=123456789
do_get "/api/v1/analytics/user/${USER_ID}/transactions" "limit=10"
echo -e "\n\n"

echo "### Test 4: invalid timestamp (expected 401)"
BAD_TS=$(($(date +%s) - 1000))
BAD_MESSAGE="GET/api/v1/analytics/transactions${BAD_TS}start_date=2026-01-01"
BAD_SIG=$(printf '%s' "$BAD_MESSAGE" | openssl dgst -sha256 -hmac "$HMAC_SECRET" -binary | xxd -p -c 256)
curl -sS \
  -H "X-Timestamp: $BAD_TS" \
  -H "X-HMAC-Signature: $BAD_SIG" \
  "${BASE_URL}/api/v1/analytics/transactions?start_date=2026-01-01" || true

echo -e "\n\n"

echo "### Done."
