#!/usr/bin/env bash
# send_api_test.sh — Tests for /api/v1/send, /api/v1/userbalance, /api/v1/referral/lookup
#
# Usage:
#   ./send_api_test.sh [base_url] [private_key_pem] [wallet_id] [from_username] [to_username] [to_telegram_id]
#
# Example:
#   ./send_api_test.sh http://localhost:5454 send-ed25519.pem myservice myservice johndoe 123456789
#
# Requests are signed with Ed25519 (see docs/send-api-ed25519.md). The wallet's
# public key must be set as ed25519_public_key in the bot config.
#
# Requirements: curl, OpenSSL 3+

set -uo pipefail

BASE_URL="${1:-http://localhost:5454}"
KEY="${2:-send-ed25519.pem}"
WALLET_ID="${3:-myservice}"
FROM_USERNAME="${4:-myservice}"
TO_USERNAME="${5:-testuser}"
TO_TELEGRAM_ID="${6:-123456789}"
ZERO_SIG=$(head -c 64 /dev/zero | base64 | tr -d '\r\n')

# ── helpers ───────────────────────────────────────────────────────────────────

GREEN='\033[0;32m'
RED='\033[0;31m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
NC='\033[0m'

PASS=0
FAIL=0
SKIP=0

# JSON field check — grep-based, no jq/python needed
# Handles: string "key":"val", number "key":123, bool "key":true/false
json_field() {
  local file="$1"
  local key="$2"
  local expected="$3"
  # matches "key":"value" or "key":value (number/bool)
  grep -qE "\"${key}\"[[:space:]]*:[[:space:]]*\"?${expected}\"?" "$file" 2>/dev/null
}

# sign METHOD PATH QUERY BODY [TIMESTAMP] → sets SIG_TS, SIG_NONCE and SIG
sign() {
  local method="$1" path="$2" query="$3" body="$4" hash msg
  SIG_TS="${5:-$(date +%s)}"
  SIG_NONCE=$(openssl rand -hex 16)
  hash=$(printf '%s' "$body" | openssl dgst -sha256 -r | cut -d' ' -f1)
  msg=$(mktemp)
  printf 'BDB-SEND-ED25519-V1\n%s\n%s\n%s\n%s\n%s\n%s' "$method" "$path" "$query" "$SIG_TS" "$SIG_NONCE" "$hash" > "$msg"
  SIG=$(openssl pkeyutl -sign -rawin -inkey "$KEY" -in "$msg" | base64 | tr -d '\r\n')
  rm -f "$msg"
}

# send_signed METHOD URI BODY SIGNATURE → prints the status code (uses SIG_TS / SIG_NONCE)
send_signed() {
  curl -sS -o /tmp/api_resp.json -w "%{http_code}" \
    -X "$1" \
    -H "Content-Type: application/json" \
    -H "X-Wallet-ID: $WALLET_ID" \
    -H "X-Timestamp: $SIG_TS" \
    -H "X-Nonce: $SIG_NONCE" \
    -H "X-Signature: $4" \
    ${3:+--data-binary "$3"} \
    "$2" 2>/dev/null || echo "000"
}

# do_post <path> <json_body>  → prints response body, sets LAST_STATUS
do_post() {
  sign POST "$1" "" "$2"
  LAST_STATUS=$(send_signed POST "${BASE_URL}$1" "$2" "$SIG")
  cat /tmp/api_resp.json
}

# do_get <path> <query_string>  → prints response body, sets LAST_STATUS
do_get() {
  local uri="${BASE_URL}$1"
  [[ -n "$2" ]] && uri="${uri}?$2"
  sign GET "$1" "$2" ""
  LAST_STATUS=$(send_signed GET "$uri" "" "$SIG")
  cat /tmp/api_resp.json
}

check() {
  local label="$1"
  local expected="$2"
  local actual="$LAST_STATUS"

  if [[ "$actual" == "$expected" ]]; then
    echo -e " ${GREEN}✓ PASS${NC} — $label (HTTP $actual)"
    ((PASS++)) || true
  else
    echo -e " ${RED}✗ FAIL${NC} — $label (expected HTTP $expected, got $actual)"
    ((FAIL++)) || true
  fi
}

check_json() {
  local label="$1"
  local key="$2"
  local expected="$3"
  if json_field /tmp/api_resp.json "$key" "$expected"; then
    echo -e " ${GREEN}✓ PASS${NC} — $label"
    ((PASS++)) || true
  else
    echo -e " ${RED}✗ FAIL${NC} — $label (field '$key' != '$expected')"
    ((FAIL++)) || true
  fi
}

skip() {
  echo -e " ${CYAN}~ SKIP${NC} — $1"
  ((SKIP++)) || true
}

header() {
  echo ""
  echo -e "${YELLOW}━━━ $1 ━━━${NC}"
}

UNIQUE_MEMO="test-memo-$(date +%s)"

# ── /api/v1/send ──────────────────────────────────────────────────────────────

header "/api/v1/send — happy paths"

if [[ "$FROM_USERNAME" == "$TO_USERNAME" ]]; then
  skip "send small amount → 200 (TO_USERNAME same as FROM_USERNAME — pass a different recipient as arg 4)"
  skip "send without memo → 200 (same reason)"
else
  echo "► Send small amount (valid)"
  do_post "/api/v1/send" "{\"to\":\"${TO_USERNAME}\",\"amount\":10,\"memo\":\"${UNIQUE_MEMO}\"}"
  echo ""
  check "send small amount → 200" "200"
  check_json "send response success:true" "success" "true"

  echo "► Send without memo"
  do_post "/api/v1/send" "{\"to\":\"${TO_USERNAME}\",\"amount\":5}"
  echo ""
  check "send without memo → 200" "200"
fi

echo "► Send with Telegram ID as recipient"
MEMO2="test-tid-$(date +%s)"
do_post "/api/v1/send" "{\"to\":\"${TO_TELEGRAM_ID}\",\"amount\":10,\"memo\":\"${MEMO2}\"}"
echo ""
if [[ "$LAST_STATUS" == "200" ]]; then
  check "send to telegram_id → 200" "200"
  check_json "send to telegram_id success:true" "success" "true"
elif [[ "$LAST_STATUS" == "400" ]]; then
  skip "send to telegram_id — user ID ${TO_TELEGRAM_ID} not in bot db (pass a real ID as arg 5)"
else
  check "send to telegram_id → 200" "200"
fi

header "/api/v1/send — error cases"

echo "► Missing 'to' field"
do_post "/api/v1/send" "{\"amount\":100}"
echo ""
check "missing to → 400" "400"

echo "► Amount = 0 (below minimum)"
do_post "/api/v1/send" "{\"to\":\"${TO_USERNAME}\",\"amount\":0}"
echo ""
check "amount 0 → 400" "400"

echo "► Negative amount"
do_post "/api/v1/send" "{\"to\":\"${TO_USERNAME}\",\"amount\":-1}"
echo ""
check "negative amount → 400" "400"

echo "► Amount above max (2000000 sats)"
do_post "/api/v1/send" "{\"to\":\"${TO_USERNAME}\",\"amount\":2000000}"
echo ""
check "amount above max → 400" "400"

echo "► Recipient does not exist"
do_post "/api/v1/send" "{\"to\":\"nonexistent_user_xyz_$(date +%s)\",\"amount\":10}"
echo ""
check "unknown recipient → 400" "400"

echo "► Memo too long (300 chars)"
LONG_MEMO=$(python3 -c "print('x'*300)" 2>/dev/null || printf '%0.s x' {1..300})
do_post "/api/v1/send" "{\"to\":\"${TO_USERNAME}\",\"amount\":10,\"memo\":\"${LONG_MEMO}\"}"
echo ""
check "memo too long → 400" "400"

echo "► Duplicate memo (resend same memo — only meaningful if first send succeeded)"
if [[ "$FROM_USERNAME" != "$TO_USERNAME" ]]; then
  do_post "/api/v1/send" "{\"to\":\"${TO_USERNAME}\",\"amount\":10,\"memo\":\"${UNIQUE_MEMO}\"}"
  echo ""
  check "duplicate memo → 400" "400"
else
  skip "duplicate memo — skipped because happy path send was skipped"
fi

echo "► Invalid signature"
BODY='{"to":"someone","amount":10}'
sign POST /api/v1/send "" "$BODY"
LAST_STATUS=$(send_signed POST "${BASE_URL}/api/v1/send" "$BODY" "$ZERO_SIG")
check "bad signature → 401" "401"

echo "► Expired timestamp"
sign POST /api/v1/send "" "$BODY" $(($(date +%s) - 600))
LAST_STATUS=$(send_signed POST "${BASE_URL}/api/v1/send" "$BODY" "$SIG")
check "expired timestamp → 401" "401"

echo "► Replayed request (same nonce twice)"
REPLAY_BODY="{\"telegram_id\":${TO_TELEGRAM_ID}}"
sign POST /api/v1/userbalance "" "$REPLAY_BODY"
send_signed POST "${BASE_URL}/api/v1/userbalance" "$REPLAY_BODY" "$SIG" > /dev/null
LAST_STATUS=$(send_signed POST "${BASE_URL}/api/v1/userbalance" "$REPLAY_BODY" "$SIG")
check "replay → 401" "401"

# ── /api/v1/userbalance ───────────────────────────────────────────────────────

header "/api/v1/userbalance — happy paths"

echo "► Get balance for known user"
do_post "/api/v1/userbalance" "{\"telegram_id\":${TO_TELEGRAM_ID}}"
echo ""
check "userbalance known user → 200" "200"

header "/api/v1/userbalance — error cases"

echo "► Missing telegram_id"
do_post "/api/v1/userbalance" "{}"
echo ""
check "missing telegram_id → 400" "400"

echo "► telegram_id = 0"
do_post "/api/v1/userbalance" "{\"telegram_id\":0}"
echo ""
check "telegram_id 0 → 400" "400"

echo "► Unknown user (should return success:false but still 200)"
do_post "/api/v1/userbalance" "{\"telegram_id\":9999999999}"
echo ""
check "unknown user balance → 200" "200"
check_json "unknown user has success:false" "success" "false"

# ── /api/v1/referral/lookup ───────────────────────────────────────────────────

header "/api/v1/referral/lookup — happy paths"

# Probe whether the referral endpoint is deployed
sign GET /api/v1/referral/lookup "code=probe" ""
_probe_status=$(send_signed GET "${BASE_URL}/api/v1/referral/lookup?code=probe" "" "$ZERO_SIG")

if [[ "$_probe_status" == "404" ]]; then
  skip "referral lookup valid code — endpoint not deployed yet (deploy new build first)"
  skip "referral lookup empty code"
  skip "referral empty users array"
  skip "referral missing code → 400"
  skip "referral bad signature → 401"
else
  echo "► Lookup existing code"
  do_get "/api/v1/referral/lookup" "code=TESTCODE"
  echo ""
  check "referral lookup valid code → 200" "200"
  check_json "referral response has 'code' field" "code" "TESTCODE"

  echo "► Lookup code with no users (returns empty list, not error)"
  do_get "/api/v1/referral/lookup" "code=NONEXISTENT_$(date +%s)"
  echo ""
  check "referral lookup empty code → 200" "200"
  check_json "empty code returns count:0" "count" "0"

  header "/api/v1/referral/lookup — error cases"

  echo "► Missing code parameter"
  do_get "/api/v1/referral/lookup" ""
  echo ""
  check "missing code → 400" "400"

  echo "► Bad signature on referral lookup"
  sign GET /api/v1/referral/lookup "code=PROMO" ""
  LAST_STATUS=$(send_signed GET "${BASE_URL}/api/v1/referral/lookup?code=PROMO" "" "$ZERO_SIG")
  check "bad signature on lookup → 401" "401"
fi

# ── summary ───────────────────────────────────────────────────────────────────

echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
TOTAL=$((PASS + FAIL))
echo -e "Results: ${GREEN}${PASS} passed${NC}, ${RED}${FAIL} failed${NC}, ${CYAN}${SKIP} skipped${NC} / ${TOTAL} run"
if [[ $SKIP -gt 0 ]]; then
  echo ""
  echo "Skipped tests can be enabled by:"
  echo "  • Pass a different recipient as arg 4 (not the same as the wallet username)"
  echo "  • Pass a real bot user's Telegram ID as arg 5"
  echo "  • Deploy the updated build so the referral endpoint is live"
fi
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"

[[ $FAIL -eq 0 ]]
