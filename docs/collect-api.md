# Collect API

A whitelisted wallet (for example the DCA backend's) can ask a user to **pay
it**: for example, a user who sells bitcoin. The API only creates the
request. **Nothing moves until the user approves it** in the Mini App or web
app, through the DCA backend with the user's own Telegram credential (see
[P2P API](p2p-api.md)). A leaked wallet key alone can't take sats.

The bot sends no message about the request: the DCA backend shows it in the
Mini App or web app.

Approving creates a normal P2P transfer from the user to the wallet, so
balance checks, settlement and status work the same as for `/p2p/send`.

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
request. The Mini App routes also need `api.p2p.enabled`.

## Wallet endpoints (DCA backend)

Signed with the wallet's send key, exactly like `/api/v1/send` (see
[send-api-ed25519.md](send-api-ed25519.md)). A wallet without `can_collect`
gets `403 collect_not_allowed`.

### `POST /api/v1/collect`

```json
{ "to": "@alice", "amount": 50000, "memo": "Sell order #812", "external_id": "6b1f…uuid" }
```

- `to`: a Telegram `@username` or numeric ID. Prefer the ID: usernames can
  change hands. The user must have started the bot.
- `external_id`: 8–128 characters, unique per request (use your order ID or a
  UUID). Retrying with the same `external_id` returns the same request (`200`
  instead of `201`). The same `external_id` with a different user, amount or
  memo is refused with `409 idempotency_conflict`.

Returns `201` with a [request](#request). It expires after **15 minutes**.

At most 10 requests per wallet per user per hour (`429 rate_limited`).

### `GET /api/v1/collect/{id}`

The current state of one of your requests. Poll every 3–5 seconds while
`status` is `pending`, and while `approved` but `paid` is `false` and the
transfer is still `processing` or `in_flight`.

### `POST /api/v1/collect/{id}/cancel`

Withdraws a request that is still `pending`. After that it can't be approved.

## Mini App / web app endpoints

Called by the DCA backend with its P2P signature and the user's credential,
the same way as the other `/api/v1/p2p/*` endpoints. Only the user the
request is for can see or answer it; anyone else gets `404`.

| Endpoint | Purpose |
|---|---|
| `GET /api/v1/p2p/collect/{id}` | Show the request (amount, who asks, memo, expiry) |
| `POST /api/v1/p2p/collect/{id}/approve` | Pay it. Needs a fresh login, like `/p2p/send`. Returns the [transfer](p2p-api.md#transfer). Repeats return the same transfer. |
| `POST /api/v1/p2p/collect/{id}/decline` | Decline it |

Both bodies are empty (`{}` is fine). The request ID reaches the Mini App
from the DCA backend, which created it.

`approve` pays straight away; there is no confirmation in the bot chat, so
show the user a review screen first. The P2P daily limit does not apply to
collect requests, since each one is approved by the user; the wallet's
`max_amount` does.

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
| `pending` | Waiting for the user, until `expires_at` |
| `approved` | The user approved. `transfer` shows the payment; `paid` is `true` once it completed. A `failed` transfer (for example, low balance) is final: create a new request. |
| `declined` | The user declined |
| `cancelled` | You cancelled it |
| `expired` | Not answered in 15 minutes |

Only one of these answers ever counts: if the user presses Pay and the
request expires at the same moment, exactly one wins, and a request can
never pay twice.

## Errors

Same shape as the P2P API: `{ "code": "…", "message": "…" }`.

| HTTP | `code` |
|---|---|
| 400 | `invalid_json`, `invalid_amount`, `amount_too_large`, `memo_too_long`, `invalid_external_id`, `invalid_recipient`, `self_collect` |
| 403 | `collect_not_allowed`, `collector_not_allowed` (the wallet's bot user is missing or banned) |
| 404 | `recipient_not_found`, `not_found` |
| 409 | `idempotency_conflict`, `in_progress`, `request_closed` (already answered, cancelled or expired) |
| 429 | `rate_limited` |
