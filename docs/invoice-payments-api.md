# Bitcoin Deepa — Invoice Payments API (Provider Integration Guide)

This guide is for **payment providers** that integrate with Bitcoin Deepa to **pay bolt11 Lightning invoices** from a dedicated, operator-provisioned wallet.

It is intentionally scoped to the invoice‑payment use case. It covers authentication, the endpoints you need, the limits and constraints of the Send API, error handling, and end‑to‑end examples.

- **Base URL:** `https://bitcoindeepa.com`
- **Content type:** `application/json`
- **Auth:** HMAC‑SHA256 request signing (see below)
- **Amounts:** always in **satoshis** unless stated otherwise

> You will be issued two secrets by the Bitcoin Deepa operator:
> a **wallet ID** (identifies your provider account) and an **HMAC secret** (used to sign requests). Keep the HMAC secret server‑side only.

---

## 1. Authentication (HMAC request signing)

Every request must carry two headers:

| Header             | Description |
|--------------------|-------------|
| `X-Timestamp`      | Current Unix time in **seconds** |
| `X-HMAC-Signature` | Hex‑encoded HMAC‑SHA256 signature of the request (see algorithm) |

### Signing algorithm

Build the message to sign by concatenating, **in this exact order, with no separators**:

```
MESSAGE = HTTP_METHOD + REQUEST_PATH + TIMESTAMP + RAW_BODY
```

- `HTTP_METHOD` — uppercase, e.g. `POST` or `GET`
- `REQUEST_PATH` — path only, no host, no query string, e.g. `/api/v1/send`
- `TIMESTAMP` — the same value sent in `X-Timestamp`
- `RAW_BODY` — the exact JSON body bytes you send (empty string for `GET`)

Then:

```
X-HMAC-Signature = hex( HMAC_SHA256( key = HMAC_SECRET, message = MESSAGE ) )
```

### Rules

- **Replay window:** the request is rejected if `now - X-Timestamp` exceeds the configured tolerance (**default 300 seconds / 5 minutes**). Sign and send promptly; do not reuse timestamps/signatures.
- **Byte‑exact body:** the signature is computed over the literal bytes you transmit. Serialize the JSON once and sign/send the identical bytes — re‑serializing (whitespace, key order changes) will break verification.
- **Wallet identification:** you do not send your wallet ID. The server identifies your provider account by matching the signature against the registered secret. An unmatched signature returns `401`.

### Reference implementations

**Bash / OpenSSL**

```bash
TIMESTAMP=$(date +%s)
BODY='{"to":"lnbc10u1p3xyz..."}'
METHOD="POST"
PATH_ONLY="/api/v1/send"

MESSAGE="${METHOD}${PATH_ONLY}${TIMESTAMP}${BODY}"
SIGNATURE=$(printf '%s' "$MESSAGE" | openssl dgst -sha256 -hmac "$HMAC_SECRET" | awk '{print $2}')

curl -X POST "https://bitcoindeepa.com${PATH_ONLY}" \
  -H "Content-Type: application/json" \
  -H "X-Timestamp: ${TIMESTAMP}" \
  -H "X-HMAC-Signature: ${SIGNATURE}" \
  -d "$BODY"
```

**Node.js**

```js
const crypto = require("crypto");

function signedHeaders(method, path, body, secret) {
  const timestamp = Math.floor(Date.now() / 1000).toString();
  const message = method + path + timestamp + body;
  const signature = crypto.createHmac("sha256", secret).update(message).digest("hex");
  return { "X-Timestamp": timestamp, "X-HMAC-Signature": signature };
}

const body = JSON.stringify({ to: "lnbc10u1p3xyz..." });
const headers = {
  "Content-Type": "application/json",
  ...signedHeaders("POST", "/api/v1/send", body, process.env.HMAC_SECRET),
};
// send `body` unchanged with these headers
```

**Python**

```python
import hmac, hashlib, time, json, requests

def signed_headers(method, path, body, secret):
    ts = str(int(time.time()))
    msg = f"{method}{path}{ts}{body}"
    sig = hmac.new(secret.encode(), msg.encode(), hashlib.sha256).hexdigest()
    return {"X-Timestamp": ts, "X-HMAC-Signature": sig}

body = json.dumps({"to": "lnbc10u1p3xyz..."}, separators=(",", ":"))
headers = {"Content-Type": "application/json", **signed_headers("POST", "/api/v1/send", body, HMAC_SECRET)}
requests.post("https://bitcoindeepa.com/api/v1/send", data=body, headers=headers)
```

---

## 2. Endpoints

For invoice payments you need one primary endpoint and two supporting ones.

| Method | Path | Purpose |
|--------|------|---------|
| `POST` | `/api/v1/send` | **Pay a bolt11 invoice** (primary) |
| `GET`  | `/api/v1/send/status/{transaction_id}` | Poll the status of a payment held for admin approval |
| `POST` | `/api/v1/userbalance` | Optional pre‑flight balance check |

---

### 2.1 POST /api/v1/send — Pay a bolt11 invoice

Detects that `to` is a bolt11 invoice (`lnbc...`, optionally prefixed with `lightning:`) and pays it externally over Lightning.

#### Request body

```json
{
  "to": "lnbc10u1p3xyz...",
  "memo": "order-12345"
}
```

| Field    | Type   | Required | Description |
|----------|--------|----------|-------------|
| `to`     | string | Yes | The bolt11 invoice to pay. `lightning:` prefix is accepted and stripped. |
| `amount` | int64  | No  | **Ignored for invoices.** The amount is taken from the invoice. Amountless invoices are rejected. |
| `memo`   | string | No  | Optional. For invoice payments it is used only for your sender confirmation / logs; it is **not** attached to the Lightning payment. Max `max_memo_length` (default 280) chars. |

#### Success — `200 OK`

```json
{
  "success": true,
  "message": "Invoice paid successfully",
  "from_user": "yourprovider",
  "to_user": "lnbc10u1p3xyz...",
  "amount": 1000,
  "amount_lkr": "32.50",
  "payment_hash": "3d2f...e91a",
  "fee": 1
}
```

| Field          | Description |
|----------------|-------------|
| `success`      | `true` on a settled payment |
| `amount`       | Amount paid, in sats (from the invoice) |
| `payment_hash` | Payment hash of the settled invoice — use this as your reconciliation key |
| `fee`          | Routing fee actually paid, in sats (best effort; may be omitted if the post‑payment lookup fails) |
| `amount_lkr`   | LKR value of the amount, if a price is available |

#### Held for approval — `202 Accepted`

If the invoice amount exceeds the wallet's admin‑approval threshold, the payment is **not** made immediately. It is held pending and an approval prompt is sent to the operator via Telegram.

```json
{
  "success": false,
  "message": "Transaction requires admin approval (amount: 60000 > threshold: 50000). Approval request sent to you via Telegram. Transaction ID: pending-yourprovider-invoice-3d2f...-1699999999",
  "from_user": "yourprovider",
  "to_user": "lnbc10u1p3xyz...",
  "amount": 60000,
  "amount_lkr": "1,950.00"
}
```

Extract the transaction ID from `message` (or track it yourself) and poll `GET /api/v1/send/status/{transaction_id}` until it resolves. The invoice must still be valid (unexpired) at the time of approval, or the payment will fail when executed.

#### Error — `400 Bad Request`

```json
{ "error": "Invoice must specify an amount (amountless invoices are not supported)" }
```

See [Error reference](#4-error-reference) for the full list.

---

### 2.2 GET /api/v1/send/status/{transaction_id}

Returns the current status of a payment that was held for admin approval (`202` response).
The `{transaction_id}` is the ID from the `202` response `message`.

`GET` requests sign over an **empty body**: `MESSAGE = "GET" + "/api/v1/send/status/{transaction_id}" + TIMESTAMP + ""`.

#### Response — `200 OK`

```json
{
  "id": "pending-yourprovider-invoice-3d2f...-1699999999",
  "status": "executed",
  "from_user": "yourprovider",
  "to_user": "lnbc10u1p3xyz...",
  "amount": 60000,
  "amount_lkr": "1,950.00",
  "memo": "order-12345",
  "request_timestamp": "2026-07-17T10:00:00Z",
  "expiry_time": "2026-07-18T10:00:00Z",
  "approved_by": "operatoradmin",
  "approval_time": "2026-07-17T10:04:12Z"
}
```

#### Status values

| Status     | Meaning |
|------------|---------|
| `pending`  | Awaiting operator approval |
| `approved` | Approved, execution in progress |
| `executed` | Invoice paid successfully |
| `rejected` | Operator declined the payment |
| `expired`  | Not approved within the 24‑hour window |

Poll at a modest interval (e.g. every 5–15s). Pending transactions expire after **24 hours**.

---

### 2.3 POST /api/v1/userbalance — Optional pre‑flight check

Lets you check a wallet balance before attempting a payment. Provide the Telegram ID associated with your provider account (supplied by the operator).

#### Request body

```json
{ "telegram_id": 123456789 }
```

#### Response — `200 OK`

```json
{
  "success": true,
  "telegram_id": 123456789,
  "balance": 250000,
  "balance_lkr": "8,125.00",
  "username": "yourprovider",
  "created_at": "2025-01-01T00:00:00Z",
  "message": "Balance retrieved successfully"
}
```

Remember to leave headroom for the **routing‑fee reserve** (see limitations) — a balance exactly equal to the invoice amount will be rejected.

---

## 3. Send API limitations & constraints

These apply to invoice payments via `POST /api/v1/send`.

| Constraint | Detail |
|------------|--------|
| **Whitelisted wallets only** | Only provider accounts registered by the operator (with an HMAC secret) can call the API. |
| **HMAC signing required** | Every request must be signed; unsigned or mismatched requests get `401`. |
| **Replay window** | Requests older than the timestamp tolerance (**default 300s**) are rejected. |
| **Amount source** | Amount is taken **from the invoice**. Any `amount` field in the body is ignored. |
| **Amountless invoices** | **Not supported** — rejected with `400`. The invoice must encode an amount. |
| **Minimum amount** | Invoice amount must be greater than `min_amount` (default **1 sat**). |
| **Maximum amount** | Invoice amount must not exceed the wallet's `max_amount` (global default **1,000,000 sats**; may be set lower per wallet). |
| **Admin approval threshold** | Amounts above `admin_approval_threshold` (global default **50,000 sats**; may be per wallet) are held for operator approval and return `202` instead of paying immediately. |
| **Routing‑fee reserve** | Your available balance must cover the amount **plus ~2%** for routing fees. If `amount > balance * 0.98`, the request is rejected even if `balance >= amount`. |
| **Idempotency** | Requests are de‑duplicated on the invoice **payment hash**. Concurrent or retried attempts to pay the same invoice will not double‑pay. |
| **Irreversibility** | A settled Lightning payment cannot be reversed. Validate the invoice/amount before sending. |
| **Invoice validity** | Expired or malformed invoices are rejected (either at decode time with `400`, or by the Lightning backend at pay time). |
| **Memo** | Cosmetic for invoice payments; capped at `max_memo_length` (default 280). |
| **Rate limiting** | The API is rate limited; back off on `429`/repeated failures. |
| **Amounts unit** | All amounts are integer **satoshis**. |

> Exact numeric limits (min/max/threshold, fee reserve tolerance, timestamp tolerance) are set by the operator per deployment and may differ from the defaults above — confirm your provisioned values with the Bitcoin Deepa operator.

---

## 4. Error reference

Errors use a consistent JSON shape:

```json
{ "error": "human-readable reason" }
```

| HTTP | When | Example `error` |
|------|------|-----------------|
| `400` | Malformed request or business rule violation | `Invalid Lightning invoice` |
| `400` | Amountless invoice | `Invoice must specify an amount (amountless invoices are not supported)` |
| `400` | Below minimum | `Amount must be greater than 1 sat` |
| `400` | Above maximum | `Amount cannot exceed 1,000,000 sats` |
| `400` | Insufficient funds | `Insufficient balance: 500 sats available, 1,000 sats required` |
| `400` | Fee reserve not covered | `Insufficient balance to cover routing fees: 1,000 sats available, 1,000 sats required plus a fee reserve` |
| `400` | Duplicate in flight | `This invoice is already being processed` |
| `400` | Backend pay failure | `Invoice payment failed: <reason>` |
| `401` | Missing/expired timestamp, missing/invalid signature | `Invalid signature` / `Request expired` / `Missing timestamp` |
| `429` | Rate limited | — |

**Ambiguous failures:** if `POST /api/v1/send` times out or returns a network error, **do not blindly retry** — the payment may have gone through. Because idempotency is keyed on the invoice payment hash, retrying the *same invoice* is safe (it will not double‑pay), but a fresh/different invoice for the same order could double‑pay. Prefer to reconcile using the `payment_hash`.

---

## 5. End‑to‑end flow

```
                ┌─────────────────────────────┐
                │ 1. (optional) userbalance    │
                │    check headroom + fee       │
                └──────────────┬───────────────┘
                               ▼
                ┌─────────────────────────────┐
                │ 2. POST /api/v1/send         │
                │    { "to": "lnbc..." }        │
                └──────────────┬───────────────┘
                     ┌─────────┴──────────┐
              200 OK │                    │ 202 Accepted
                     ▼                    ▼
       ┌───────────────────┐   ┌──────────────────────────────┐
       │ paid — record      │   │ held for approval             │
       │ payment_hash, fee  │   │ poll /send/status/{id}         │
       └───────────────────┘   │  → executed | rejected | expired│
                               └──────────────────────────────┘
```

### Worked example — small payment (immediate)

```bash
HMAC_SECRET="your-provisioned-secret"
TIMESTAMP=$(date +%s)
BODY='{"to":"lnbc10u1p3xyz...","memo":"order-12345"}'
MESSAGE="POST/api/v1/send${TIMESTAMP}${BODY}"
SIGNATURE=$(printf '%s' "$MESSAGE" | openssl dgst -sha256 -hmac "$HMAC_SECRET" | awk '{print $2}')

curl -X POST "https://bitcoindeepa.com/api/v1/send" \
  -H "Content-Type: application/json" \
  -H "X-Timestamp: ${TIMESTAMP}" \
  -H "X-HMAC-Signature: ${SIGNATURE}" \
  -d "$BODY"
# → 200 { "success": true, "payment_hash": "…", "fee": 1, ... }
```

### Worked example — large payment (approval + poll)

```bash
# 1) Submit — returns 202 with a transaction ID in `message`
#    e.g. TX_ID="pending-yourprovider-invoice-3d2f...-1699999999"

# 2) Poll status (GET signs over an empty body)
TIMESTAMP=$(date +%s)
MESSAGE="GET/api/v1/send/status/${TX_ID}${TIMESTAMP}"
SIGNATURE=$(printf '%s' "$MESSAGE" | openssl dgst -sha256 -hmac "$HMAC_SECRET" | awk '{print $2}')

curl -X GET "https://bitcoindeepa.com/api/v1/send/status/${TX_ID}" \
  -H "X-Timestamp: ${TIMESTAMP}" \
  -H "X-HMAC-Signature: ${SIGNATURE}"
# → { "status": "executed" }  (repeat until executed | rejected | expired)
```

---

## 6. Integration checklist

- [ ] Store the wallet ID and HMAC secret server‑side (never ship the secret to clients).
- [ ] Sign over the **exact** transmitted body bytes; send them unchanged.
- [ ] Use fresh `X-Timestamp` per request (within the tolerance window).
- [ ] Validate the invoice amount against your own limits before sending.
- [ ] Keep a fee‑reserve buffer (~2%) above the invoice amount in the wallet.
- [ ] Persist the `payment_hash` for reconciliation; treat it as the source of truth.
- [ ] Handle `202` by polling `/send/status/{id}` until a terminal state.
- [ ] On network timeouts, reconcile by `payment_hash` rather than blind‑retrying with a new invoice.
- [ ] Back off on `429` and repeated `5xx`.

---

*Confirm your provisioned limits (min/max/threshold), fee‑reserve tolerance, and timestamp tolerance with the Bitcoin Deepa operator — deployment values may differ from the defaults noted here.*
