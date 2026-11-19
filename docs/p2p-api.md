# Mini App & Web App P2P API

Lets the BitcoinDeepa Telegram Mini App, and the web app that uses
"Log In with Telegram", send sats to other bot users and pay Lightning invoices.

- **Base URL:** `https://bitcoindeepa.com/api/v1/p2p`
- **Off by default.** Enable with `api.p2p.enabled: true` (see Configuration).

## Authentication: who is sending

Every request carries **exactly one** credential signed by Telegram,
**unchanged**:

| From | Header | Value |
|---|---|---|
| Mini App | `X-Telegram-Init-Data` | `window.Telegram.WebApp.initData` |
| Web app | `X-Telegram-ID-Token` | the `id_token` from Log In with Telegram (OIDC) |

The bot checks Telegram's signature (the bot token for init data, Telegram's
published keys for ID tokens), and the sender is **always** the Telegram user
in that credential. Nothing else in a request can name or change the sender. A
backend that relays the request cannot send on anyone's behalf. A request with
both headers is rejected.

- Init data older than `init_data_max_age` (default 1 hour) is rejected: ask
  the user to reopen the Mini App.
- `/send` and `/invoice/pay` move sats, so they accept only init data younger
  than `spend_init_data_max_age` (default 10 minutes). Telegram sets
  `initData` when the Mini App opens, so a user who has had it open longer
  gets `401 unauthorized` on send: ask them to close and reopen the Mini App,
  then retry with the **same** `idempotency_key`. Check `auth_date` in
  `initDataUnsafe` to warn them before they tap Send.
- Pass `initData` (the raw string), not `initDataUnsafe`.

### Web app: Log In with Telegram (OIDC)

Turn it on with `api.p2p.oidc_enabled: true`. The bot then accepts ID tokens
from [Log In with Telegram](https://core.telegram.org/bots/telegram-login):

- Log in with **this bot** (@BitcoinDeepaBot) as the client. The token's `aud`
  must be the bot ID (`oidc_client_id`, by default the number before `:` in the
  bot token).
- Request the **`openid profile`** scopes. The bot reads the user from the
  `id` claim, which only comes with `profile`. (`sub` is not the Telegram ID.)
- The bot checks the signature against
  `https://oauth.telegram.org/.well-known/jwks.json` (RS256, ES256 or EdDSA),
  `iss` = `https://oauth.telegram.org`, `aud`, and `exp`.
- The same age limits apply to the token's login time (`iat`): up to
  `init_data_max_age` for reads, and up to `spend_init_data_max_age`
  (default **10 minutes**) for `/send` and `/invoice/pay`. ID tokens can't be
  refreshed, so if the login is older than that, ask the user to tap
  **Log In with Telegram** again before sending, then retry with the same
  `idempotency_key`. Check `iat` in the token to prompt them before they tap
  Send.
- Keep the ID token on the web app server (e.g. in the session), not in the
  browser's storage.

### Calling it from the Mini App or web app

Call it **from the DCA backend**, which serves both the Mini App and the web
app. Never call it from the browser. Set `api.p2p.allowed_networks`
to the DCA backend's IPs so the P2P routes answer only them (`403 forbidden`
otherwise). If the global `api.allowed_networks` is set too, it also covers
`/api/v1/p2p/*`, so the DCA backend must be in **both** lists:

```
Mini App ──(initData)──► DCA backend ──(X-Telegram-Init-Data + service signature)──► bot
web app  ──(session)───► DCA backend ──(X-Telegram-ID-Token  + service signature)──► bot
```

The server forwards the credential as-is and adds nothing that identifies a
user. One backend and one P2P service key serve both. This key is **separate** from the
DCA backend's send-wallet key (see `docs/dca-backend-integration.md`).

## Authentication: who is calling (service signature)

The DCA backend also signs **every** call with its own Ed25519 key, so only it
can call `/api/v1/p2p/*`, even if the IP allowlist is wrong or empty. The bot stores
only the public key (`service_public_key`). P2P stays off until it is set.

Create the key pair on the DCA backend:

```bash
openssl genpkey -algorithm ed25519 -out p2p-service.pem                        # keep secret, DCA backend only
openssl pkey -in p2p-service.pem -pubout -outform DER | tail -c 32 | base64    # → service_public_key
```

Headers, added by the DCA backend:

| Header        | Value |
|---------------|-------|
| `X-Timestamp` | Unix time in seconds (within ±300 s of server time) |
| `X-Nonce`     | 16–128 random chars `[A-Za-z0-9_-]`, new for every request |
| `X-Signature` | Base64 Ed25519 signature of the string below |

Signing string: eight lines joined with `\n`, no trailing newline:

```
BDB-P2P-ED25519-V1
<METHOD>
<PATH>                                  e.g. /api/v1/p2p/send
<QUERY>                                 raw query without "?", or empty
<TIMESTAMP>
<NONCE>
<hex sha256 of the credential>         the X-Telegram-Init-Data or X-Telegram-ID-Token value
<hex sha256 of the exact body bytes>    (sha256 of empty string for GET)
```

The credential is signed too, so a captured request can't be reused with
another user's credential. Each nonce works once.

```js
// DCA backend (Node.js)
const crypto = require("crypto");
const key = crypto.createPrivateKey(process.env.P2P_SERVICE_KEY_PEM);
const sha = (s) => crypto.createHash("sha256").update(s).digest("hex");

// cred: { initData } from the Mini App, or { idToken } from the web app session
function p2pHeaders(method, path, query, cred, body) {
  const ts = Math.floor(Date.now() / 1000).toString();
  const nonce = crypto.randomBytes(16).toString("hex");
  const value = cred.initData ?? cred.idToken;
  const msg = ["BDB-P2P-ED25519-V1", method, path, query, ts, nonce, sha(value), sha(body)].join("\n");
  return {
    "Content-Type": "application/json",
    [cred.initData ? "X-Telegram-Init-Data" : "X-Telegram-ID-Token"]: value,
    "X-Timestamp": ts,
    "X-Nonce": nonce,
    "X-Signature": crypto.sign(null, Buffer.from(msg), key).toString("base64"),
  };
}
// fetch(`${BOT}/api/v1/p2p/send`, { method: "POST", headers: p2pHeaders("POST", "/api/v1/p2p/send", "", { initData }, body), body })
```

## Rules

| Rule | Default |
|---|---|
| Max per transfer | 100,000 sats (`max_amount`) |
| Max per sender per UTC day | 500,000 sats (`daily_limit`) |
| Sends without confirmation per sender per UTC day | 50,000 sats in total (`confirm_threshold`) |
| Sends + invoice payments per sender | 10 per minute (`rate_limit_per_minute`) |
| Resolves + invoice decodes per sender | 30 per minute |
| Transfer status polls per sender | 30 per minute |
| Memo | 280 characters (`max_memo_length`) |

- Recipients must have **started the bot**. Unknown users are rejected, so
  sats can't get stuck in an unclaimed wallet.
- No sending to yourself. Banned users can neither send nor receive.
- Sends go to the recipient's **numeric Telegram ID**, never a username.
- **Confirmation:** each sender can send up to `confirm_threshold` sats per
  UTC day **in total** without confirming. Any transfer that would go over
  that total (including one transfer above it) needs confirmation: the bot
  messages the sender in Telegram with **Confirm / Cancel** buttons, valid for
  10 minutes. Nothing moves until they press Confirm. Splitting a payment into
  small parts doesn't skip this. `GET /me` shows what is left as
  `unconfirmed_remaining`; transfers that fail give it back.
- **`idempotency_key`**: generate a new UUID per payment and **reuse it when
  retrying the same request** (e.g. after a timeout or a dropped connection).
  The same key always returns the same transfer, whatever its status, so a
  retried request can never pay twice. Reusing a key for a different payment
  returns `409`.
- `failed`, `cancelled` and `expired` are **final for their key**. To try
  again after one of these, start a new payment with a **new** key.
- Don't start a new key while the previous transfer for the same payment is
  still `processing`, `awaiting_confirmation` or `in_flight`. Keep polling it.

## Endpoints

### `GET /me`
```json
{
  "telegram_id": 111, "username": "alice", "name": "Alice",
  "balance": 250000, "balance_lkr": "250,000 sats (~LKR 7,500)",
  "limits": { "max_amount": 100000, "daily_limit": 500000, "daily_used": 60000,
              "daily_remaining": 440000, "confirm_threshold": 50000,
              "unconfirmed_remaining": 30000 }
}
```

### `POST /resolve`
Find who you're sending to. Show `name` on the confirm screen. Users who
haven't started the bot, or can't receive, all get `404 recipient_not_found`.
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
`needs_confirmation` is true when the amount is more than the sender's
`unconfirmed_remaining`.

### `POST /invoice/pay`
```json
{ "invoice": "lnbc…", "memo": "", "idempotency_key": "4f1e…uuid" }
```
Waits up to 15 seconds for the payment to settle. If it hasn't settled by
then, returns `202` with status `in_flight`: poll `GET /transfers/{id}`.
While that transfer is `processing`, `awaiting_confirmation` or `in_flight`,
don't start the same invoice with a new key: it is refused with
"This invoice is already being paid." After it ends `failed`, `cancelled` or
`expired`, a new key can pay the invoice. The bot never pays an invoice twice.

### `GET /transfers/{id}`
Current state of one of **your** transfers (others get `404`). Poll it every
2–3 seconds while the status is `processing` or `awaiting_confirmation`, and
every **15–30 seconds** while it is `in_flight`: a payment can stay in flight
for a long time, and the bot checks it with lnbits at most once every 20
seconds anyway. Polls are limited to 30 per minute per sender.

### Collect requests
`GET /collect/{id}`, `POST /collect/{id}/approve` and `POST /collect/{id}/decline`
let the user answer a request from a wallet to pay it. `approve` needs a fresh
login, like `/send`. See [collect-api.md](collect-api.md).

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
| `failed` | 200 | Nothing was sent; reason in `error`. Final for this key. | `error` |
| `awaiting_confirmation` | 202 | Waiting for Confirm in the bot chat, until `confirm_by` | "Approve this in your Telegram chat" + poll |
| `processing` | 202 | Running | Spinner + poll |
| `in_flight` | 202 | Payment sent, not settled yet. It may still complete or fail. | "Payment in progress" + slow poll |
| `cancelled` | 200 | Cancelled in the bot chat | Cancelled |
| `expired` | 200 | Not confirmed in time | Expired |

## Errors

```json
{ "code": "recipient_not_found", "message": "This user hasn't joined @BitcoinDeepaBot yet. …" }
```
`message` is written for the user and can be shown as-is.

| HTTP | `code` |
|---|---|
| 400 | `invalid_json`, `invalid_amount`, `amount_too_large`, `memo_too_long`, `invalid_idempotency_key`, `invalid_recipient`, `self_send`, `invalid_invoice`, `amountless_invoice`, `invoice_expired` |
| 401 | `unauthorized`: credential missing, invalid or expired, both credentials sent, or (for spends) the login is older than `spend_init_data_max_age`. Mini App: reopen it. Web app: log in with Telegram again. |
| 401 | `bad_service_signature`: the DCA backend's P2P signature is missing, wrong, expired or replayed. A server bug, not a user error. |
| 403 | `forbidden` (caller's IP not in `api.p2p.allowed_networks`), `sender_not_found` (user must start the bot first), `sender_not_allowed` |
| 404 | `recipient_not_found`, `not_found` |
| 409 | `idempotency_conflict`, `in_progress` |
| 429 | `rate_limited` |

Insufficient balance and daily-limit failures come back as a transfer with
status `failed` and the reason in `error`.

## Configuration

```yaml
api:
  p2p:
    enabled: true
    allowed_networks: ["<DCA backend IP>"]   # only these IPs can call /api/v1/p2p/*; empty = any IP
    max_amount: 100000
    daily_limit: 500000
    confirm_threshold: 50000      # daily total a sender can send without confirming
    rate_limit_per_minute: 10
    init_data_max_age: 3600
    spend_init_data_max_age: 600   # /send and /invoice/pay
    max_memo_length: 280
    service_public_key: "<base64 Ed25519 P2P public key of the DCA backend>"   # required
    oidc_enabled: true    # accept Log In with Telegram ID tokens from the web app
    # oidc_client_id: "1234567890"   # bot ID the web app logs in with; default: this bot
```
