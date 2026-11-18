# Mini App P2P API

Lets the BitcoinDeepa Telegram Mini App send sats to other bot users and pay
Lightning invoices.

- **Base URL:** `https://bitcoindeepa.com/api/v1/p2p`
- **Off by default.** Enable with `api.p2p.enabled: true` (see Configuration).

## Authentication: who is sending

Every request carries the Mini App's signed launch data, **unchanged**:

```
X-Telegram-Init-Data: <window.Telegram.WebApp.initData>
```

The bot checks Telegram's signature with its own token (the Mini App is opened
from @BitcoinDeepaBot), and the sender is **always** the user in that data.
Nothing else in a request can name or change the sender. A backend that relays
the request cannot send on anyone's behalf.

- Init data older than `init_data_max_age` (default 1 hour) is rejected: ask
  the user to reopen the Mini App.
- Pass `initData` (the raw string), not `initDataUnsafe`.

### Calling it from the Mini App

`/api/v1/` only answers allow-listed IPs (`api.allowed_networks`), so call it
**from the TMA server**, not the browser:

```
browser ──(initData)──► TMA server route ──(X-Telegram-Init-Data: same string)──► bot
```

The server route forwards the header as-is and adds nothing that identifies a
user.

## Rules

| Rule | Default |
|---|---|
| Max per transfer | 100,000 sats (`max_amount`) |
| Max per sender per UTC day | 500,000 sats (`daily_limit`) |
| Confirm in the bot chat above | 50,000 sats (`confirm_threshold`) |
| Sends + invoice payments per sender | 10 per minute (`rate_limit_per_minute`) |
| Resolves + invoice decodes per sender | 30 per minute |
| Memo | 280 characters (`max_memo_length`) |

- Recipients must have **started the bot**. Unknown users are rejected, so
  sats can't get stuck in an unclaimed wallet.
- No sending to yourself. Banned users can neither send nor receive.
- Sends go to the recipient's **numeric Telegram ID**, never a username.
- Above `confirm_threshold`, the bot messages the sender in Telegram with
  **Confirm / Cancel** buttons, valid for 10 minutes. Nothing moves until they
  press Confirm.
- **`idempotency_key`**: generate a new UUID per payment and **reuse it on
  retry**. The same key always returns the same transfer, so a retried request
  can never pay twice. Reusing a key for a different payment returns `409`.

## Endpoints

### `GET /me`
```json
{
  "telegram_id": 111, "username": "alice", "name": "Alice",
  "balance": 250000, "balance_lkr": "250,000 sats (~LKR 7,500)",
  "limits": { "max_amount": 100000, "daily_limit": 500000, "daily_used": 60000,
              "daily_remaining": 440000, "confirm_threshold": 50000 }
}
```

### `POST /resolve`
Find who you're sending to. Show `name` on the confirm screen.
```json
{ "to": "@bob" }            // or "bob" or "123456789"
```
```json
{ "telegram_id": 222, "username": "bob", "name": "Bob Silva" }
```

### `POST /send`
```json
{ "to_telegram_id": 222, "amount": 1000, "memo": "lunch", "idempotency_key": "0b9a…uuid" }
```
Returns a [transfer](#transfer).

### `POST /invoice/decode`
Show the invoice before paying.
```json
{ "invoice": "lnbc…" }
```
```json
{ "amount": 21000, "amount_lkr": "…", "description": "Coffee", "payment_hash": "…",
  "expires_at": "2026-11-19T10:00:00Z", "needs_confirmation": false, "over_limit": false }
```

### `POST /invoice/pay`
```json
{ "invoice": "lnbc…", "memo": "", "idempotency_key": "4f1e…uuid" }
```
Waits up to 15 seconds for the payment to settle. If it hasn't settled by
then, returns `202` with status `in_flight`: poll `GET /transfers/{id}`.
**Never retry the same invoice with a new idempotency key**: the bot refuses
to pay an invoice twice anyway.

### `GET /transfers/{id}`
Current state of one of **your** transfers (others get `404`). Poll it every
2–3 seconds while the status is `awaiting_confirmation` or `in_flight`.

## Transfer

```json
{
  "id": "p2p-…", "kind": "internal", "status": "completed",
  "amount": 1000, "amount_lkr": "…", "fee": 0, "memo": "lunch",
  "to": { "telegram_id": 222, "username": "bob", "name": "Bob Silva" },
  "payment_hash": "", "description": "", "preimage": "",
  "error": "", "confirm_by": null, "created_at": "…"
}
```
`kind` is `internal` or `invoice`. Invoice transfers have no `to` but have
`payment_hash`, `description` and, once paid, `fee` and `preimage`.

| `status` | HTTP | Meaning | Mini App shows |
|---|---|---|---|
| `completed` | 200 | Done | Success |
| `failed` | 200 | Nothing was sent; reason in `error` | `error` |
| `awaiting_confirmation` | 202 | Waiting for Confirm in the bot chat, until `confirm_by` | "Approve this in your Telegram chat" + poll |
| `processing` | 202 | Running | Spinner + poll |
| `in_flight` | 202 | Invoice sent, not settled yet | "Payment in progress" + poll |
| `cancelled` | 200 | Cancelled in the bot chat | Cancelled |
| `expired` | 200 | Not confirmed in time | Expired |

## Errors

```json
{ "code": "recipient_not_found", "message": "This user hasn't joined @BitcoinDeepaBot yet. …" }
```
`message` is written for the user and can be shown as-is.

| HTTP | `code` |
|---|---|
| 400 | `invalid_json`, `invalid_amount`, `amount_too_large`, `memo_too_long`, `invalid_idempotency_key`, `invalid_recipient`, `recipient_not_allowed`, `self_send`, `invalid_invoice`, `amountless_invoice`, `invoice_expired` |
| 401 | `unauthorized`: init data missing, invalid or expired. Reopen the Mini App. |
| 403 | `sender_not_found` (user must start the bot first), `sender_not_allowed` |
| 404 | `recipient_not_found`, `not_found` |
| 409 | `idempotency_conflict`, `in_progress` |
| 429 | `rate_limited` |

Insufficient balance and daily-limit failures come back as a transfer with
status `failed` and the reason in `error`.

## Configuration

```yaml
api:
  allowed_networks: ["<TMA server IP>"]   # /api/v1/ is IP-restricted
  p2p:
    enabled: true
    max_amount: 100000
    daily_limit: 500000
    confirm_threshold: 50000
    rate_limit_per_minute: 10
    init_data_max_age: 3600
    max_memo_length: 280
```
