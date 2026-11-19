# Init API: create a wallet from the Mini App or web app

`POST /api/v1/init` creates a bot wallet for a Telegram user who never pressed
**Start** in the bot, the same way `/start` does. Until a user has a wallet,
every `/api/v1/p2p/*` call answers `403 sender_not_found`, and nobody can send
to them or collect from them.

The user is always the one in the Telegram-signed credential, so a caller
can't create a wallet for anyone else.

Called by the **DCA backend**, which serves the Mini App and the web app.

## When to call it

- On first open of the Mini App or web app, or
- when any P2P call answers `403 sender_not_found`.

It's safe to call on every open: for an existing user it creates nothing
and returns `200`.

## Request

```
POST /api/v1/init
```

Note the path: it is **not** under `/p2p/`, but it is authenticated exactly
like the P2P endpoints (see [p2p-api.md](p2p-api.md)).

### Headers

| Header | Value |
|---|---|
| `Content-Type` | `application/json` |
| `X-Telegram-Init-Data` | Mini App: `window.Telegram.WebApp.initData`, unchanged |
| `X-Telegram-ID-Token` | Web app: the `id_token` from Log In with Telegram, unchanged |
| `X-Timestamp` | Unix time in seconds (±300 s) |
| `X-Nonce` | 16–128 random chars `[A-Za-z0-9_-]`, new for every request |
| `X-Signature` | Base64 Ed25519 signature with the DCA backend's **P2P key** |

Send exactly one of `X-Telegram-Init-Data` and `X-Telegram-ID-Token`.

The signing string has eight lines:

```
BDB-P2P-ED25519-V1
POST
/api/v1/init

<TIMESTAMP>
<NONCE>
<hex sha256 of the initData or id_token>
<hex sha256 of the exact body bytes>
```

The fourth line is the query string, which is empty here. For an empty body,
the last line is the SHA-256 of the empty string
(`e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855`).

### Body (optional)

Send no body, `{}`, or:

```json
{ "referral_code": "PROMO2024" }
```

| Field | Rules |
|---|---|
| `referral_code` | Optional. 1–64 characters: letters, digits, `_`, `-`. Recorded like a `/start` payload, for new users only. |

Any other field is refused with `400 invalid_json`.

## Response

`201 Created` for a new wallet, `200 OK` if the user already had one.

```json
{
  "telegram_id": 111,
  "username": "alice",
  "name": "Alice Perera",
  "balance": 0,
  "balance_lkr": "…",
  "limits": {
    "max_amount": 100000,
    "daily_limit": 500000,
    "daily_used": 0,
    "daily_remaining": 500000,
    "confirm_threshold": 50000,
    "unconfirmed_remaining": 50000
  },
  "created": true,
  "can_message": false
}
```

| Field | Meaning |
|---|---|
| `created` | `true` if this call created the wallet |
| `can_message` | Only for a new wallet. `false` means the bot can't message the user yet (see below) |
| everything else | Same as `GET /api/v1/p2p/me` |

## Let the bot message the user

Telegram doesn't let a bot message a user who never started it or allowed it
to. Without that, the user gets no **Confirm** buttons for larger sends, no
**collect** prompts and no receipts in the chat.

- **Mini App:** call `Telegram.WebApp.requestWriteAccess()` **before** `init`.
- **Web app:** request message permission in Log In with Telegram.
- **Either:** if `can_message` is `false`, show a button that opens
  `https://t.me/<bot username>?start=<referral code>`.

## Errors

Body: `{ "code": "…", "message": "…" }`. `message` can be shown to the user.

| HTTP | `code` | Cause |
|---|---|---|
| 400 | `invalid_json` | Bad body or an unknown field |
| 400 | `invalid_referral_code` | Wrong characters, or longer than 64 |
| 401 | `unauthorized` | Credential missing, invalid or expired, or both credentials sent |
| 401 | `bad_service_signature` | The DCA backend's signature is wrong, expired or replayed (a server bug) |
| 403 | `forbidden` | Caller's IP not in `api.p2p.allowed_networks` |
| 403 | `sender_not_allowed` | Banned user or a bot account |
| 429 | `rate_limited` | Too many calls for this user, or more than 30 new wallets per minute overall |
| 503 | `wallet_unavailable` | Wallet creation failed. Retry. |

## Example (Node.js)

Uses the `bdbP2P` helper from the
[DCA backend guide](dca-backend-integration.md) (§5.2).

```ts
// Mini App: ask for messages first, then create the wallet.
await new Promise((ok) => Telegram.WebApp.requestWriteAccess(ok)); // in the browser

// DCA backend:
const res = await bdbP2P("POST", "/api/v1/init", { initData }, { referral_code: "PROMO2024" });
const me = await res.json();
if (me.created && !me.can_message) {
  // show "Open the bot" with https://t.me/<bot>?start=PROMO2024
}
```

## Requirements on the bot

- `api.p2p.enabled: true` with the DCA backend's `service_public_key`.
- `api.p2p.oidc_enabled: true` for web app logins.
