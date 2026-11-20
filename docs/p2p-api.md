# Mini App & Web App P2P API

Lets the BitcoinDeepa Telegram Mini App, and the web app that uses
"Log In with Telegram", send sats to other bot users and pay Lightning invoices.
All calls come from the **DCA backend**, which serves both apps.

- **Base URL:** `https://bitcoindeepa.com/api/v1/p2p` (plus `POST /api/v1/init`)
- **Off by default.** Enable with `api.p2p.enabled: true` (see Configuration).

## Authentication

Two parts, on every call:

1. **Who is calling:** the DCA backend signs the request with its P2P key
   (Ed25519). Only it can call these endpoints.
2. **Who the user is:**
   - **Login, `POST /api/v1/init` only:** the user's Telegram login, which the
     bot checks with Telegram. This links the user.
   - **Every other call:** `X-BDB-User: <telegram_id>`. The bot trusts the DCA
     backend's signature for it.

```
Mini App ──(initData)──► DCA backend ──POST /api/v1/init + X-Telegram-Init-Data──► bot   (login, once)
web app  ──(id_token)──► DCA backend ──POST /api/v1/init + X-Telegram-ID-Token───► bot   (login, once)
app      ──(session)───► DCA backend ──/api/v1/p2p/* + X-BDB-User──────────────────► bot   (every other call)
```

The DCA backend keeps its own session for the user (cookies, refresh tokens).
The Telegram login is needed again only to log in on a new device, or after a
link expires (one year without a login).

### Login: the Telegram credential (`POST /api/v1/init` only)

Send **exactly one** credential signed by Telegram, **unchanged**:

| From | Header | Value |
|---|---|---|
| Mini App | `X-Telegram-Init-Data` | `window.Telegram.WebApp.initData` (the raw string, not `initDataUnsafe`) |
| Web app | `X-Telegram-ID-Token` | the `id_token` from Log In with Telegram (OIDC) |

- The bot checks Telegram's signature: the bot token for init data, and
  Telegram's published keys for ID tokens.
- The user is **always** the Telegram user in that credential.
- The login can be at most `init_data_max_age` old (default 1 hour). If it's
  older, ask the user to reopen the Mini App or log in again.
- Sending both headers, or `X-BDB-User` on `/api/v1/init`, is rejected.

**Web app (OIDC).** Turn it on with `api.p2p.oidc_enabled: true`:
- Log in with **this bot** as the client. The token's `aud` must be the bot ID
  (`oidc_client_id`, by default the number before `:` in the bot token).
- Request the **`openid profile`** scopes. The user comes from the `id` claim,
  which only comes with `profile`. (`sub` is not the Telegram ID.)
- The bot checks the signature against
  `https://oauth.telegram.org/.well-known/jwks.json` (RS256, ES256 or EdDSA),
  plus `iss` = `https://oauth.telegram.org`, `aud` and `exp`.

### Every other call: `X-BDB-User`

- Header `X-BDB-User: <numeric telegram_id>`, and **no** Telegram credential.
  A call that sends a credential instead gets `401`.
- **Any user with a bot wallet** can be named. A user with no wallet gets
  `403 sender_not_found`: create it with `POST /api/v1/init`.
- **Security:** the bot trusts the DCA backend about who the user is,
  **payments included**. If the P2P key leaks, it can pay from any
  user's wallet, up to `daily_limit`. Passkeys will add per-payment approval
  ([session-passkey-spec.md](session-passkey-spec.md)). Keep the P2P key in a
  secret store and the user sessions server-side.

### The service signature

Call the bot **from the DCA backend**, never from the browser. Set
`api.p2p.allowed_networks` to the DCA backend's IPs so these routes answer
only them (`403 forbidden` otherwise). If the global `api.allowed_networks`
is set too, the DCA backend must be in **both** lists.

The bot stores only the public key (`service_public_key`). P2P stays off until
it is set. Create the key pair on the DCA backend:

```bash
openssl genpkey -algorithm ed25519 -out p2p-service.pem                        # keep secret, DCA backend only
openssl pkey -in p2p-service.pem -pubout -outform DER | tail -c 32 | base64    # → service_public_key
```

This key is **separate** from the DCA backend's send-wallet key (see
`docs/dca-backend-integration.md`).

| Header        | Value |
|---------------|-------|
| `X-Timestamp` | Unix time in seconds (within ±300 s of server time) |
| `X-Nonce`     | 16–128 random chars `[A-Za-z0-9_-]`, new for every request |
| `X-Signature` | Base64 Ed25519 signature of the string below |

Signing string: eight lines joined with `\n`, no trailing newline. The first
line and the seventh line depend on the call:

```
BDB-P2P-ED25519-V1  (login)            BDB-P2P-ED25519-V2  (every other call)
<METHOD>                               <METHOD>
<PATH>                                 <PATH>
<QUERY>                                <QUERY>
<TIMESTAMP>                            <TIMESTAMP>
<NONCE>                                <NONCE>
<hex sha256 of the credential>         <telegram_id, exactly as in X-BDB-User>
<hex sha256 of the exact body bytes>   <hex sha256 of the exact body bytes>
```

- `<PATH>` is exactly as sent, still percent-encoded, e.g. `/api/v1/p2p/send`.
  `<QUERY>` is the raw query without `?`, or empty.
- The body hash for an empty body is the SHA-256 of the empty string.
- The credential and the user ID are signed, so neither can be swapped in
  transit. Each nonce works once.

```js
// DCA backend (Node.js)
const crypto = require("crypto");
const key = crypto.createPrivateKey(process.env.P2P_SERVICE_KEY_PEM);
const sha = (s) => crypto.createHash("sha256").update(s).digest("hex");

// who: { initData } or { idToken } for POST /api/v1/init, { telegramId } for everything else
function p2pHeaders(method, path, query, who, body) {
  const ts = Math.floor(Date.now() / 1000).toString();
  const nonce = crypto.randomBytes(16).toString("hex");
  const h = { "Content-Type": "application/json", "X-Timestamp": ts, "X-Nonce": nonce };
  let lines;
  if (who.telegramId) {
    h["X-BDB-User"] = String(who.telegramId);
    lines = ["BDB-P2P-ED25519-V2", method, path, query, ts, nonce, String(who.telegramId), sha(body)];
  } else {
    const value = who.initData ?? who.idToken;
    h[who.initData ? "X-Telegram-Init-Data" : "X-Telegram-ID-Token"] = value;
    lines = ["BDB-P2P-ED25519-V1", method, path, query, ts, nonce, sha(value), sha(body)];
  }
  h["X-Signature"] = crypto.sign(null, Buffer.from(lines.join("\n")), key).toString("base64");
  return h;
}
// login: fetch(`${BOT}/api/v1/init`, { method: "POST", headers: p2pHeaders("POST", "/api/v1/init", "", { initData }, "{}"), body: "{}" })
// send:  fetch(`${BOT}/api/v1/p2p/send`, { method: "POST", headers: p2pHeaders("POST", "/api/v1/p2p/send", "", { telegramId: 111 }, body), body })
```

## Rules

| Rule | Default |
|---|---|
| Max per transfer | 100,000 sats (`max_amount`) |
| Max per sender per UTC day | 500,000 sats (`daily_limit`) |
| Sends + invoice payments per sender | 10 per minute (`rate_limit_per_minute`) |
| Resolves + invoice decodes per sender | 30 per minute |
| Transfer status polls per sender | 30 per minute |
| Memo | 280 characters (`max_memo_length`) |

- Recipients must have **started the bot**. Unknown users are rejected, so
  sats can't get stuck in an unclaimed wallet.
- No sending to yourself. Banned users can neither send nor receive.
- Sends go to the recipient's **numeric Telegram ID**, never a username.
- **No bot-chat confirmation.** The signed request (DCA backend signature plus
  the user's fresh Telegram credential) is the approval, so the Mini App must
  show its own review screen before calling `/send` or `/invoice/pay`. `max_amount` and `daily_limit` still apply. Passkey
  (WebAuthn) approval through the DCA backend is planned.
- **`idempotency_key`**: generate a new UUID per payment and **reuse it when
  retrying the same request** (e.g. after a timeout or a dropped connection).
  The same key always returns the same transfer, whatever its status, so a
  retried request can never pay twice. Reusing a key for a different payment
  returns `409`.
- `failed` is **final for its key** (as are `cancelled` and `expired` on older
  transfers). To try
  again after one of these, start a new payment with a **new** key.
- Don't start a new key while the previous transfer for the same payment is
  still `processing` or `in_flight`. Keep polling it.

## Endpoints

### `POST /api/v1/init`
The **login** call, and the only one that carries the Telegram credential (see
Authentication). It creates the user's wallet if they never pressed Start in
the bot, the same way `/start` does, and links the user for `X-BDB-User`. It's
safe to call on every login. Note the path: it is not under `/p2p/`, but the
P2P service signature and `api.p2p.allowed_networks` apply.

```json
{ "referral_code": "PROMO2024" }
```
The body is optional. `referral_code` (letters, digits, `_`, `-`, up to 64) is
recorded like a `/start` payload, for new users only.

Returns `201` for a new wallet and `200` if the user already had one, with the
same fields as `/me` plus `created`. A new wallet also has `can_message`:
`false` means the bot can't message the user yet, so they won't get receipts
in the chat. Ask for it first with
`Telegram.WebApp.requestWriteAccess()` in the Mini App, or the "allow messages"
option of Log In with Telegram, or send them to the bot with a Start link.

Limits: 30 new wallets per minute across all users; banned users get `403`.

### `GET /me`
```json
{
  "telegram_id": 111, "username": "alice", "name": "Alice",
  "balance": 250000, "balance_lkr": "250,000 sats (~LKR 7,500)",
  "limits": { "max_amount": 100000, "daily_limit": 500000, "daily_used": 60000,
              "daily_remaining": 440000 }
}
```

### `POST /resolve`
Find who you're sending to. Show `name` on the review screen. Users who
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
  "expires_at": "2026-11-19T10:00:00Z", "over_limit": false }
```

### `POST /invoice/pay`
```json
{ "invoice": "lnbc…", "memo": "", "idempotency_key": "4f1e…uuid" }
```
Waits up to 15 seconds for the payment to settle. If it hasn't settled by
then, returns `202` with status `in_flight`: poll `GET /transfers/{id}`.
While that transfer is `processing` or `in_flight`, don't start the same
invoice with a new key: it is refused with "This invoice is already being
paid." After it ends `failed`, a new key can pay the invoice. The bot never pays an invoice twice.

### `GET /transfers/{id}`
Current state of one of **your** transfers (others get `404`). Poll it every
2–3 seconds while the status is `processing`, and
every **15–30 seconds** while it is `in_flight`: a payment can stay in flight
for a long time, and the bot checks it with lnbits at most once every 20
seconds anyway. Polls are limited to 30 per minute per sender.

### Collect requests
`GET /collect/{id}` shows the user a charge a wallet took from them. There is
no approval step. See [collect-api.md](collect-api.md).

## Transfer

```json
{
  "id": "p2p-…", "kind": "internal", "status": "completed",
  "amount": 1000, "amount_lkr": "…", "fee": 0, "memo": "lunch",
  "to": { "telegram_id": 222, "username": "bob", "name": "Bob Silva" },
  "payment_hash": "", "description": "", "preimage": "",
  "error": "", "created_at": "…"
}
```
`kind` is `internal` or `invoice`. Invoice transfers have no `to` but have
`payment_hash`, `description` and, once paid, `fee` and `preimage`.

| `status` | HTTP | Meaning | Mini App shows |
|---|---|---|---|
| `completed` | 200 | Done | Success |
| `failed` | 200 | Nothing was sent; reason in `error`. Final for this key. | `error` |
| `processing` | 202 | Running | Spinner + poll |
| `in_flight` | 202 | Payment sent, not settled yet. It may still complete or fail. | "Payment in progress" + slow poll |
| `cancelled`, `expired` | 200 | Only on transfers from before bot-chat confirmations were removed | Cancelled / Expired |

## Errors

```json
{ "code": "recipient_not_found", "message": "This user hasn't joined @BitcoinDeepaBot yet. …" }
```
`message` is written for the user and can be shown as-is.

| HTTP | `code` |
|---|---|
| 400 | `invalid_json`, `invalid_amount`, `amount_too_large`, `memo_too_long`, `invalid_idempotency_key`, `invalid_recipient`, `self_send`, `invalid_invoice`, `amountless_invoice`, `invoice_expired` |
| 401 | `unauthorized`: on `/api/v1/init`, the Telegram login is missing, invalid, too old or sent twice (Mini App: reopen it; web app: log in again). On other calls, `X-BDB-User` is missing or not a number, or a Telegram credential was sent instead. |
| 401 | `login_required`: `X-BDB-User` sent to `/api/v1/init`. |
| 401 | `bad_service_signature`: the DCA backend's P2P signature is missing, wrong, expired or replayed. A server bug, not a user error. |
| 403 | `forbidden` (caller's IP not in `api.p2p.allowed_networks`), `sender_not_found` (no wallet yet: call `POST /api/v1/init`), `sender_not_allowed` |
| 503 | `wallet_unavailable` (wallet creation failed; retry) |
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
    rate_limit_per_minute: 10
    init_data_max_age: 3600        # max age of the Telegram login sent to /api/v1/init
    max_memo_length: 280
    service_public_key: "<base64 Ed25519 P2P public key of the DCA backend>"   # required
    oidc_enabled: true    # accept Log In with Telegram ID tokens from the web app
    # oidc_client_id: "1234567890"   # bot ID the web app logs in with; default: this bot
```
