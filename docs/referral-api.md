# Send API & Referral Lookup API

## Table of Contents

1. [Authentication](#authentication)
2. [Send API](#send-api)
   - [POST /api/v1/send](#post-apiv1send)
   - [GET /api/v1/send/status/{transaction_id}](#get-apiv1sendstatustransaction_id)
   - [POST /api/v1/userbalance](#post-apiv1userbalance)
3. [Referral API](#referral-api)
   - [How referral codes are captured](#how-referral-codes-are-captured)
   - [GET /api/v1/referral/lookup](#get-apiv1referrallookup)
4. [Configuration](#configuration)
5. [Error Reference](#error-reference)

---

## Authentication

All Send and Referral endpoints use **wallet-based HMAC-SHA256** signing. Each whitelisted wallet has its own secret configured in `config.yaml`.

### Signing a request

Build the message string:
```
METHOD + PATH + TIMESTAMP + BODY
```

For `GET` requests with no body, use the raw query string in place of `BODY`.

| Part        | Example value                      |
|-------------|-------------------------------------|
| `METHOD`    | `POST`                              |
| `PATH`      | `/api/v1/send`                      |
| `TIMESTAMP` | `1748563200` (Unix seconds)         |
| `BODY`      | Raw JSON string (or query string)   |

**Example (POST):**
```
POST/api/v1/send1748563200{"to":"johndoe","amount":1000,"memo":"reward"}
```

**Example (GET):**
```
GET/api/v1/referral/lookup1748563200code=PROMO2024
```

Sign with HMAC-SHA256 using your wallet's `hmac_secret`:

```bash
TIMESTAMP=$(date +%s)
BODY='{"to":"johndoe","amount":1000,"memo":"reward"}'
MESSAGE="POST/api/v1/send${TIMESTAMP}${BODY}"
SIGNATURE=$(echo -n "$MESSAGE" | openssl dgst -sha256 -hmac "your-hmac-secret" | awk '{print $2}')
```

### Required headers

| Header             | Value                        |
|--------------------|------------------------------|
| `Content-Type`     | `application/json`           |
| `X-Timestamp`      | Unix timestamp (seconds)     |
| `X-HMAC-Signature` | Hex-encoded HMAC-SHA256      |

> Requests older than `timestamp_tolerance` seconds (default: 300) are rejected to prevent replay attacks.

---

## Send API

Enabled when `api.send.enabled: true` in `config.yaml`.

### POST /api/v1/send

Sends a Lightning payment from a whitelisted wallet to any bot user, Telegram ID, or Lightning address.

#### Request body

```json
{
  "to": "johndoe",
  "amount": 1000,
  "memo": "optional memo"
}
```

| Field    | Type   | Required | Description |
|----------|--------|----------|-------------|
| `to`     | string | Yes      | Recipient: Telegram username (no `@`), Telegram ID (numeric), or Lightning address |
| `amount` | int64  | Yes      | Amount in satoshis |
| `memo`   | string | No       | Optional payment memo (max `max_memo_length` chars, default 280) |

#### Recipient resolution order

1. Lightning address (e.g. `user@domain.com`)
2. Telegram ID (5–15 digit number)
3. Telegram username

#### Amount limits

| Limit                    | Default   | Config key                  |
|--------------------------|-----------|-----------------------------|
| Minimum                  | 1 sat     | `min_amount`                |
| Maximum                  | 1,000,000 sats | `max_amount`           |
| Admin approval threshold | 50,000 sats | `admin_approval_threshold` |

Amounts **above the threshold** are held as pending and an approval request is sent to the sender via Telegram. The API returns HTTP `202 Accepted` in this case.

#### Memo deduplication

If a `memo` is provided, the API checks whether a transaction with that exact memo already completed. Duplicate memos are rejected with a `400` error. This can be used as an idempotency key.

#### Success response — `200 OK`

```json
{
  "success": true,
  "transaction_hash": "abc123...",
  "message": "Payment sent successfully",
  "from_user": "myservice",
  "to_user": "johndoe",
  "amount": 1000,
  "amount_lkr": "32.50",
  "memo": "optional memo"
}
```

#### Pending approval response — `202 Accepted`

```json
{
  "success": false,
  "message": "Transaction requires admin approval (amount: 60000 > threshold: 50000). Approval request sent to you via Telegram. Transaction ID: txn_abc123",
  "from_user": "myservice",
  "to_user": "johndoe",
  "amount": 60000,
  "amount_lkr": "1,950.00"
}
```

#### Example request

```bash
TIMESTAMP=$(date +%s)
BODY='{"to":"johndoe","amount":1000,"memo":"promo-reward-001"}'
MESSAGE="POST/api/v1/send${TIMESTAMP}${BODY}"
SIGNATURE=$(echo -n "$MESSAGE" | openssl dgst -sha256 -hmac "your-hmac-secret" | awk '{print $2}')

curl -X POST "https://bitcoindeepa.com/api/v1/send" \
  -H "Content-Type: application/json" \
  -H "X-Timestamp: ${TIMESTAMP}" \
  -H "X-HMAC-Signature: ${SIGNATURE}" \
  -d "${BODY}"
```

---

### GET /api/v1/send/status/{transaction_id}

Returns the current approval status of a pending transaction created by `POST /api/v1/send` when the amount exceeded the `admin_approval_threshold`.

Use the `id` value from the `202 Accepted` response as the `{transaction_id}` path parameter.

#### Path parameter

| Parameter        | Description                                   |
|------------------|-----------------------------------------------|
| `transaction_id` | The `id` returned in the `202` send response  |

#### Success response — `200 OK`

```json
{
  "id": "pending-myservice-johndoe-150000-1748563200",
  "status": "pending",
  "from_user": "myservice",
  "to_user": "johndoe",
  "amount": 150000,
  "amount_lkr": "4,875.00",
  "memo": "invoice-001",
  "request_timestamp": "2026-05-31T10:00:00Z",
  "expiry_time": "2026-06-01T10:00:00Z"
}
```

When approved or rejected, additional fields appear:

```json
{
  "id": "pending-myservice-johndoe-150000-1748563200",
  "status": "approved",
  "from_user": "myservice",
  "to_user": "johndoe",
  "amount": 150000,
  "amount_lkr": "4,875.00",
  "memo": "invoice-001",
  "request_timestamp": "2026-05-31T10:00:00Z",
  "expiry_time": "2026-06-01T10:00:00Z",
  "approved_by": "admin:127.0.0.1",
  "approval_time": "2026-05-31T10:05:00Z"
}
```

#### Possible `status` values

| Status     | Meaning                                              |
|------------|------------------------------------------------------|
| `pending`  | Awaiting admin approval via Telegram                 |
| `approved` | Admin approved — payment is being executed           |
| `executed` | Payment executed successfully                        |
| `rejected` | Admin rejected the transaction                       |
| `expired`  | Not acted on within 24 hours — transaction discarded |

#### Error responses

| HTTP | `error`                         | Cause                            |
|------|---------------------------------|----------------------------------|
| `400` | `Missing transaction_id`       | Path parameter not provided      |
| `400` | `Transaction 'X' not found`    | ID does not exist in database    |

#### Example request

```bash
TIMESTAMP=$(date +%s)
TX_ID="pending-myservice-johndoe-150000-1748563200"
MESSAGE="GET/api/v1/send/status/${TX_ID}${TIMESTAMP}"
SIGNATURE=$(echo -n "$MESSAGE" | openssl dgst -sha256 -hmac "your-hmac-secret" | awk '{print $2}')

curl -X GET "https://bitcoindeepa.com/api/v1/send/status/${TX_ID}" \
  -H "X-Timestamp: ${TIMESTAMP}" \
  -H "X-HMAC-Signature: ${SIGNATURE}"
```

> The HMAC message for this endpoint uses an empty query string (no body, no query params):
> `GET` + `/api/v1/send/status/{id}` + `{timestamp}` + `` (empty)

---

### POST /api/v1/userbalance

Returns the wallet balance of any bot user by Telegram ID.

#### Request body

```json
{
  "telegram_id": 123456789
}
```

| Field         | Type  | Required | Description       |
|---------------|-------|----------|-------------------|
| `telegram_id` | int64 | Yes      | Telegram user ID  |

#### Success response — `200 OK`

```json
{
  "success": true,
  "telegram_id": 123456789,
  "balance": 5000,
  "balance_lkr": "162.50",
  "first_name": "John",
  "last_name": "Doe",
  "username": "johndoe",
  "created_at": "2026-01-15T10:00:00Z",
  "message": "Balance retrieved successfully"
}
```

#### Example request

```bash
TIMESTAMP=$(date +%s)
BODY='{"telegram_id":123456789}'
MESSAGE="POST/api/v1/userbalance${TIMESTAMP}${BODY}"
SIGNATURE=$(echo -n "$MESSAGE" | openssl dgst -sha256 -hmac "your-hmac-secret" | awk '{print $2}')

curl -X POST "https://bitcoindeepa.com/api/v1/userbalance" \
  -H "Content-Type: application/json" \
  -H "X-Timestamp: ${TIMESTAMP}" \
  -H "X-HMAC-Signature: ${SIGNATURE}" \
  -d "${BODY}"
```

---

## Referral API

### How referral codes are captured

When a user opens the bot via a Telegram deep link, the payload after `?start=` is stored as their referral code:

```
https://t.me/YourBotUsername?start=PROMO2024
```

The bot captures this on every `/start` command and writes to `data/referrals.db`.

**Stored fields:**

| Field           | Description                               |
|-----------------|-------------------------------------------|
| `telegram_id`   | User's Telegram ID                        |
| `username`      | User's Telegram username (no `@`)         |
| `referral_code` | The payload from the deep link            |
| `created_at`    | Timestamp when `/start` was received      |

> If the user opens `/start` with no deep link payload, nothing is written.

---

### GET /api/v1/referral/lookup

Returns all users who joined using a given referral code, sorted by `joined_at` ascending.

#### Query parameters

| Parameter | Required | Description                    |
|-----------|----------|--------------------------------|
| `code`    | Yes      | The referral code to look up   |

#### Success response — `200 OK`

```json
{
  "code": "PROMO2024",
  "count": 2,
  "users": [
    {
      "telegram_id": 123456789,
      "username": "johndoe",
      "referral_code": "PROMO2024",
      "joined_at": "2026-05-30T10:00:00Z"
    },
    {
      "telegram_id": 987654321,
      "username": "janedoe",
      "referral_code": "PROMO2024",
      "joined_at": "2026-05-30T11:30:00Z"
    }
  ]
}
```

If no users are found for the code, `count` is `0` and `users` is an empty array — still `200 OK`.

#### Example request

```bash
TIMESTAMP=$(date +%s)
CODE="PROMO2024"
QUERY="code=${CODE}"
MESSAGE="GET/api/v1/referral/lookup${TIMESTAMP}${QUERY}"
SIGNATURE=$(echo -n "$MESSAGE" | openssl dgst -sha256 -hmac "your-hmac-secret" | awk '{print $2}')

curl -X GET "https://bitcoindeepa.com/api/v1/referral/lookup?${QUERY}" \
  -H "X-Timestamp: ${TIMESTAMP}" \
  -H "X-HMAC-Signature: ${SIGNATURE}"
```

#### Sending rewards to referral users

The `username` from the lookup response can be passed directly into `/api/v1/send`:

```bash
# 1. Look up who used the referral code
USERS=$(curl ... /api/v1/referral/lookup?code=PROMO2024)

# 2. Send a reward to each user
for USERNAME in $(echo $USERS | jq -r '.users[].username'); do
  curl -X POST .../api/v1/send \
    -d "{\"to\":\"$USERNAME\",\"amount\":500,\"memo\":\"PROMO2024-reward\"}"
done
```

---

## Configuration

```yaml
api:
  send:
    enabled: true

    # Only IPs in this CIDR can reach the send endpoints
    internal_network: "127.0.0.0/24"

    # Satoshi limits
    max_amount: 1000000       # 1M sats
    min_amount: 1
    admin_approval_threshold: 50000   # amounts above this need manual approval

    # Memo length cap
    max_memo_length: 280

    # Replay attack window (seconds)
    timestamp_tolerance: 10

    # One entry per service that calls the API
    whitelisted_wallets:
      my-service:
        username: "the_bot_telegram_username"   # sender wallet
        hmac_secret: "generate with: openssl rand -hex 32"

database:
  referrals_path: "data/referrals.db"
```

**Generate a secret:**
```bash
openssl rand -hex 32
```

---

## Error Reference

All errors return `400 Bad Request` (or `401`/`403` for auth failures) with the body:

```json
{ "error": "human-readable message" }
```

| Error message | Cause |
|---|---|
| `Missing 'to' field` | `to` not provided in send request |
| `Amount must be greater than X` | Amount at or below `min_amount` |
| `Amount cannot exceed X` | Amount above `max_amount` |
| `Memo cannot exceed N characters` | Memo too long |
| `Transaction with memo '...' already completed` | Duplicate memo detected |
| `Transaction with memo '...' is already processing` | Concurrent request with same memo |
| `Insufficient balance: X available, Y required` | Sender wallet has insufficient funds |
| `Recipient '@...' not found or has no wallet` | `to` user not found in bot database |
| `Sender '@...' not found or has no wallet` | Wallet username in config has no bot account |
| `missing 'code' query parameter` | `code` not provided in referral lookup |
| `Missing timestamp` | `X-Timestamp` header absent |
| `Request expired` | Timestamp older than `timestamp_tolerance` |
| `Invalid signature` | HMAC mismatch — wrong secret or message format |
