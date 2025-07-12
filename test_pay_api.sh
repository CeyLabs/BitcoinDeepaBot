#!/bin/bash

# Configuration
API_URL="https://your-bot-domain.com"
HMAC_SECRET="your-hmac-secret-here"
ENDPOINT="/api/v1/send"

# Payment data
PAYLOAD='{"amount":1000,"destination":"lnbc10u1p3xnhl2pp5e6v94jmw....","memo":"Test payment"}'

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