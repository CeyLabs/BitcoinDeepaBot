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

All Send and Referral endpoints require an **Ed25519 request signature**. Each whitelisted wallet registers its public key in `config.yaml`; the caller keeps the private key.

Every request carries `X-Wallet-ID`, `X-Timestamp`, `X-Nonce` and `X-Signature`. The signing format, key setup and Node.js/Python/bash signers are in [send-api-ed25519.md](send-api-ed25519.md).

The bash examples below use its `bdb_request` helper:

```bash
bdb_request METHOD PATH[?QUERY] [JSON_BODY]
```

> Requests with a timestamp more than `timestamp_tolerance` seconds (default: 300) away from server time are rejected, and each nonce works only once.

---

## Send API

Enabled when `api.send.enabled: true` in `config.yaml`.

### POST /api/v1/send

Sends a Lightning payment from a whitelisted wallet to any bot user, Telegram ID, Lightning address, or bolt11 Lightning invoice.

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
| `to`     | string | Yes      | Recipient: Telegram username (no `@`), Telegram ID (numeric), Lightning address, or a bolt11 invoice (`lnbc...`, optionally prefixed with `lightning:`) |
| `amount` | int64  | Yes\*    | Amount in satoshis. **Ignored for bolt11 invoices** — the amount is taken from the invoice itself |
| `memo`   | string | No       | Optional payment memo (max `max_memo_length` chars, default 280). For invoices it is used only for the sender's confirmation/logging |

\* Not required when `to` is a bolt11 invoice.

#### Recipient resolution order

1. bolt11 Lightning invoice (`lnbc...` / `lightning:lnbc...`)
2. Lightning address (e.g. `user@domain.com`)
3. Telegram ID (5–15 digit number)
4. Telegram username

#### Paying a bolt11 invoice

When `to` is a bolt11 invoice the bot pays it **externally** over Lightning:

- The amount comes from the invoice. **Amountless invoices are rejected.**
- The invoice amount is validated against `min_amount`, `max_amount`, and `admin_approval_threshold` (same limits and approval flow as internal sends).
- A ~2% routing-fee reserve is required on top of the amount; if the balance cannot cover it the request is rejected.
- Idempotency is keyed on the invoice's payment hash, so retrying the same invoice will not double-pay.
- The success response additionally includes `payment_hash` and `fee` (routing fee in sats).

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

#### Invoice payment success response — `200 OK`

When `to` is a bolt11 invoice, `payment_hash` and `fee` (sats) are included:

```json
{
  "success": true,
  "message": "Invoice paid successfully",
  "from_user": "myservice",
  "to_user": "lnbc10u1p...",
  "amount": 1000,
  "amount_lkr": "32.50",
  "payment_hash": "abc123...",
  "fee": 1
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
bdb_request POST /api/v1/send '{"to":"johndoe","amount":1000,"memo":"promo-reward-001"}'
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
TX_ID="pending-myservice-johndoe-150000-1748563200"
bdb_request GET "/api/v1/send/status/${TX_ID}"
```


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
bdb_request POST /api/v1/userbalance '{"telegram_id":123456789}'
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
bdb_request GET "/api/v1/referral/lookup?code=PROMO2024"
```

#### Sending rewards to referral users

The `username` from the lookup response can be passed directly into `/api/v1/send`:

```bash
# 1. Look up who used the referral code
USERS=$(bdb_request GET "/api/v1/referral/lookup?code=PROMO2024")

# 2. Send a reward to each user
for USERNAME in $(echo "$USERS" | jq -r '.users[].username'); do
  bdb_request POST /api/v1/send "{\"to\":\"$USERNAME\",\"amount\":500,\"memo\":\"PROMO2024-reward-$USERNAME\"}"
done
```

---

## Configuration

```yaml
api:
  send:
    enabled: true

    # Optional: only these IPs/CIDRs can reach the send endpoints.
    # Empty means no network restriction (signatures still apply).
    # (internal_network was never enforced and is now ignored.)
    allowed_networks: ["127.0.0.1"]

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
        ed25519_public_key: "<base64 public key from the caller>"

database:
  referrals_path: "data/referrals.db"
```

**Generate a key pair** (caller side; send only the public key to the operator):
```bash
openssl genpkey -algorithm ed25519 -out send-ed25519.pem
openssl pkey -in send-ed25519.pem -pubout -outform DER | tail -c 32 | base64
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
| `Invalid timestamp` | `X-Timestamp` header absent or not a number |
| `Invalid nonce` | `X-Nonce` absent or not 16–128 chars of `[A-Za-z0-9_-]` |
| `Request already processed` | Nonce already used |
| `Request expired` | Timestamp older than `timestamp_tolerance` |
| `Invalid signature` | Missing signature, unknown wallet, or wrong key / signing string |
