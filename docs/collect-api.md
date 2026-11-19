# Collect API

A whitelisted wallet (for example the DCA backend's main wallet) can
**charge a user**: the sats move from the user's bot wallet to the wallet at
once. There is **no approval step**: the wallet's signed call is enough.

The bot sends no message about it. Paying creates a normal P2P transfer from
the user to the wallet, so balance checks, settlement and status work the
same as for `/p2p/send`.

> **Security:** anyone with the wallet's key can charge any logged-in user,
> up to `max_amount` per charge and 10 charges per user per hour. Keep the
> key in a secret store, and only call this after the user agreed in the app.

## Setup

Turn it on per wallet in `api.send.whitelisted_wallets`:

```yaml
whitelisted_wallets:
  BiccoindeepaDSA:
    username: "BiccoindeepaDSA"
    ed25519_public_key: "…"
    can_collect: true
```

The wallet's `max_amount` (or the global `api.send.max_amount`) caps each
charge. The money goes to the wallet's bot user (`username`). Users must have
logged in through `POST /api/v1/init`, so `api.p2p.enabled` is needed too.

## Wallet endpoints (DCA backend)

Signed with the wallet's send key, with the same headers and rules as
`/api/v1/send` (see [send-api-ed25519.md](send-api-ed25519.md)). A wallet
without `can_collect` gets `403 collect_not_allowed`.

### `POST /api/v1/collect`

The user to charge goes in the **`X-BDB-User: <telegram_id>`** header, not in
the body. The signed text has its own first line and adds that ID as the
seventh line, so it can't be changed in transit:

```
BDB-COLLECT-ED25519-V1
POST
/api/v1/collect
                                  (empty query line)
<TIMESTAMP>
<NONCE>
<telegram_id>                     exactly as in X-BDB-User
<hex sha256 of the exact body bytes>
```

A send signature (`BDB-SEND-ED25519-V1`) is refused here.

```json
{ "amount": 50000, "memo": "Sell order #812", "external_id": "6b1f…uuid" }
```

- `X-BDB-User`: the payer's numeric Telegram ID. They must have logged in
  through `POST /api/v1/init`, otherwise `403 payer_not_linked`. Missing or
  not a number → `401`. A `to` field in the body is refused (`400 invalid_json`).
- `external_id`: 8–128 characters, unique per charge (use your order ID or a
  UUID). Retrying with the same `external_id` returns the same request (`200`
  instead of `201`) and **never charges twice**. The same `external_id` with
  a different user, amount or memo is refused with `409 idempotency_conflict`.

Returns `201` with a [request](#request) whose `status` is `approved` and
whose `transfer` is the payment. `paid` is `true` once it completed.

At most 10 charges per wallet per user per hour (`429 rate_limited`).

### `GET /api/v1/collect/{id}`

Signed exactly like `/api/v1/send` (no `X-BDB-User`). The current state of one of your requests. Poll every 3–5 seconds while
`paid` is `false` and the transfer is `processing` or `in_flight`.

## Mini App / web app endpoint

| Endpoint | Purpose |
|---|---|
| `GET /api/v1/p2p/collect/{id}` | Show a charge to the user it was taken from (amount, who charged, memo) |

Called by the DCA backend with its P2P signature and `X-BDB-User`, like the
other `/api/v1/p2p/*` endpoints. Anyone else gets `404`.

## Request

```json
{
  "id": "col-…", "status": "approved", "paid": true,
  "amount": 50000, "amount_lkr": "…", "memo": "Sell order #812", "external_id": "6b1f…uuid",
  "payer": { "telegram_id": 111, "username": "alice" },
  "collector": { "telegram_id": 999, "username": "BiccoindeepaDSA", "name": "…" },
  "expires_at": "…", "created_at": "…",
  "transfer": { "id": "p2p-col-…", "status": "completed", "…": "…" }
}
```

`external_id` is only returned to the wallet.

| `status` | Meaning |
|---|---|
| `approved` | Charged. `transfer` shows the payment; `paid` is `true` once it completed. A `failed` transfer (for example, low balance) is final: use a new `external_id` to try again. |
| `pending` | Rare: the bot stopped before paying. Retry with the same `external_id` to pay it. |
| `expired` | A `pending` request that wasn't retried within 15 minutes. Nothing was paid. |

A request can never pay twice.

## Errors

Same shape as the P2P API: `{ "code": "…", "message": "…" }`.

| HTTP | `code` |
|---|---|
| 400 | `invalid_json`, `invalid_amount`, `amount_too_large`, `memo_too_long`, `invalid_external_id`, `invalid_recipient`, `self_collect` |
| 403 | `collect_not_allowed`, `collector_not_allowed` (the wallet's bot user is missing or banned), `payer_not_linked` |
| 404 | `recipient_not_found`, `not_found` |
| 409 | `idempotency_conflict`, `in_progress` |
| 429 | `rate_limited` |
