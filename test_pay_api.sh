#!/bin/bash

# Configuration
API_URL="https://your-bot-domain.com"
HMAC_SECRET="your-hmac-secret-here"
ENDPOINT="/api/v1/send"

# Payment data 
# - to: Telegram user ID (numeric only, not username)
# - amount: Amount in satoshis
# - memo: Optional memo text
# Note: 'from' user is now configured via environment variable from_user_id
PAYLOAD='{"to":"123456789","amount":1000,"memo":"Test payment"}'

# Generate timestamp
TIMESTAMP=$(date +%s)

# Create message to sign: METHOD + PATH + TIMESTAMP + BODY
MESSAGE="POST${ENDPOINT}${TIMESTAMP}${PAYLOAD}"

# Generate HMAC signature
SIGNATURE=$(echo -n "${MESSAGE}" | openssl dgst -sha256 -hmac "${HMAC_SECRET}" -hex | cut -d' ' -f2)

# Make the request
curl -X POST "${API_URL}${ENDPOINT}" \
  -H "Content-Type: application/json" \
  -H "X-HMAC-Signature: ${SIGNATURE}" \
  -H "X-Timestamp: ${TIMESTAMP}" \
  -d "${PAYLOAD}" \
  -v