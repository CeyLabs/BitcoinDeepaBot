#!/usr/bin/env bash
# send_api_test.sh — Tests for /api/v1/send, /api/v1/userbalance, /api/v1/referral/lookup
#
# Usage:
#   ./send_api_test.sh [base_url] [hmac_secret] [from_username] [to_username] [to_telegram_id]
#
# Example:
#   ./send_api_test.sh http://localhost:5454 your-hmac-secret myservice johndoe 123456789
#
# Requirements: curl, openssl, jq

set -uo pipefail

BASE_URL="${1:-http://localhost:5454}"
HMAC_SECRET="${2:-your-hmac-secret}"
FROM_USERNAME="${3:-myservice}"
TO_USERNAME="${4:-testuser}"
TO_TELEGRAM_ID="${5:-123456789}"

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

hmac_hex() {
  printf '%s' "$1" | openssl dgst -sha256 -hmac "$HMAC_SECRET" -binary | xxd -p -c 256
}

# do_post <path> <json_body>  → prints response body, sets LAST_STATUS
do_post() {
  local path="$1"
  local body="$2"
  local ts
  ts=$(date +%s)
  local sig
  sig=$(hmac_hex "POST${path}${ts}${body}")

  LAST_STATUS=$(curl -sS -o /tmp/api_resp.json -w "%{http_code}" \
    -X POST \
    -H "Content-Type: application/json" \
    -H "X-Timestamp: $ts" \
    -H "X-HMAC-Signature: $sig" \
    -d "$body" \
    "${BASE_URL}${path}")

  cat /tmp/api_resp.json
}

# do_get <path> <query_string>  → prints response body, sets LAST_STATUS
do_get() {
  local path="$1"
  local query="$2"
  local ts
  ts=$(date +%s)
  local sig
  sig=$(hmac_hex "GET${path}${ts}${query}")

  local uri="${BASE_URL}${path}"
  [[ -n "$query" ]] && uri="${uri}?${query}"

  LAST_STATUS=$(curl -sS -o /tmp/api_resp.json -w "%{http_code}" \
    -X GET \
    -H "X-Timestamp: $ts" \
    -H "X-HMAC-Signature: $sig" \
    "${uri}")

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

echo "► Invalid HMAC signature"
BAD_TS=$(date +%s)
BODY='{"to":"someone","amount":10}'
curl -sS -o /tmp/api_resp.json -w "" \
  -X POST \
  -H "Content-Type: application/json" \
  -H "X-Timestamp: $BAD_TS" \
  -H "X-HMAC-Signature: 0000000000000000000000000000000000000000000000000000000000000000" \
  -d "$BODY" \
  "${BASE_URL}/api/v1/send" > /dev/null 2>&1 || true
LAST_STATUS=$(curl -sS -o /dev/null -w "%{http_code}" \
  -X POST \
  -H "Content-Type: application/json" \
  -H "X-Timestamp: $BAD_TS" \
  -H "X-HMAC-Signature: 0000000000000000000000000000000000000000000000000000000000000000" \
  -d "$BODY" \
  "${BASE_URL}/api/v1/send" 2>/dev/null || echo "000")
check "bad HMAC → 401" "401"

echo "► Expired timestamp"
OLD_TS=$(($(date +%s) - 600))
OLD_BODY='{"to":"someone","amount":10}'
OLD_SIG=$(hmac_hex "POST/api/v1/send${OLD_TS}${OLD_BODY}")
LAST_STATUS=$(curl -sS -o /dev/null -w "%{http_code}" \
  -X POST \
  -H "Content-Type: application/json" \
  -H "X-Timestamp: $OLD_TS" \
  -H "X-HMAC-Signature: $OLD_SIG" \
  -d "$OLD_BODY" \
  "${BASE_URL}/api/v1/send" 2>/dev/null || echo "000")
check "expired timestamp → 401" "401"

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
_probe_status=$(curl -sS -o /dev/null -w "%{http_code}" \
  -H "X-Timestamp: $(date +%s)" \
  -H "X-HMAC-Signature: 0000000000000000000000000000000000000000000000000000000000000000" \
  "${BASE_URL}/api/v1/referral/lookup?code=probe" 2>/dev/null || echo "000")

if [[ "$_probe_status" == "404" ]]; then
  skip "referral lookup valid code — endpoint not deployed yet (deploy new build first)"
  skip "referral lookup empty code"
  skip "referral empty users array"
  skip "referral missing code → 400"
  skip "referral bad HMAC → 401"
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

  echo "► Bad HMAC on referral lookup"
  BAD_TS=$(date +%s)
  LAST_STATUS=$(curl -sS -o /dev/null -w "%{http_code}" \
    -X GET \
    -H "X-Timestamp: $BAD_TS" \
    -H "X-HMAC-Signature: 0000000000000000000000000000000000000000000000000000000000000000" \
    "${BASE_URL}/api/v1/referral/lookup?code=PROMO" 2>/dev/null || echo "000")
  check "bad HMAC on lookup → 401" "401"
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
