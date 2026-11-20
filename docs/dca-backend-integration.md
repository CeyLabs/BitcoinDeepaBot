# DCA Backend ↔ BitcoinDeepa Bot — Complete Integration Guide

Everything the DCA backend (DCA BE) needs to talk to the BitcoinDeepa bot, in one place:
- **Setup and keys** (§1–§2).
- **DCA's own wallet:** payouts and collect, for buying from sellers (§3–§4).
- **Mini App and web app relay:** user wallets, `init` and P2P (§5–§6).
- **The callback the bot makes** (§7).
- **Errors and recommended flows** (§8–§9), and a **checklist** (§10).

Detailed references, if you need more: [send-api-ed25519.md](send-api-ed25519.md), [p2p-api.md](p2p-api.md), [init-api.md](init-api.md), [collect-api.md](collect-api.md), [session-passkey-spec.md](session-passkey-spec.md).

> **Breaking change:** HMAC signing (`X-HMAC-Signature`) is removed. Every call from DCA BE must be signed with Ed25519, or it gets `401`.

---

## 1. Overview

```
             (1) DCA's own wallet: payouts + collect     — signed with the SEND key
 DCA BE ─────────────────────────────────────────────►  Bot  /api/v1/send, /collect, /userbalance, ...
   │
   │         (2) user wallets: init, P2P                   — signed with the P2P key
   ├─────────────────────────────────────────────────►  Bot  /api/v1/init, /api/v1/p2p/*
   │  ▲
   │  └── Mini App (initData) / web app (Log In with Telegram)
   ▲
   │         (3) callback: POST /transaction/reset-retry-counts
   └──────────────────────────────────────────────────  Bot  (when the DCA wallet receives sats)
```

1. **DCA's own wallet.** DCA BE sends sats from its whitelisted wallet, charges users (collect), checks balances and polls payment status. It signs with the **send key** (§3–§4).
2. **User wallets.** Mini App and web app users send sats and pay invoices from **their own** bot wallets. DCA BE relays these calls, signed with the **P2P key**. The Telegram login is checked once, at login (`/api/v1/init`); after that DCA BE's own session names the user with `X-BDB-User` (§5–§6).
3. **Callback.** When the DCA wallet receives a deposit, the bot calls DCA BE so it can retry failed buys (§7).

### 1.1 The two keys

| Key | Signs | Bot config | If it leaks |
|---|---|---|---|
| **Send key** | (1) DCA's wallet: payouts and collect requests | `api.send.whitelisted_wallets.<id>.ed25519_public_key` | Someone can send sats **from the DCA wallet**, and **charge any logged-in user** (collect), up to its limits |
| **P2P key** | (2) user-wallet relay | `api.p2p.service_public_key` | Can act as, and pay from, **any bot user**, up to `daily_limit`, until passkeys exist. Guard it like the send key. |

Keep the keys **separate**, so either can be rotated without touching the other. A signature made with one key is useless on the other API: each signs a different first line (`BDB-SEND-…` vs `BDB-P2P-…`).

| | Dev | Production |
|---|---|---|
| Bot base URL | `<dev bot URL>` | `https://bitcoindeepa.com` |
| Wallet ID (`X-Wallet-ID`) | `BiccoindeepaDSATest` | given by the operator |
| Telegram bot ID (OIDC client) | `8219648988` | given by the operator |

---

## 2. One-time setup

### 2.1 Create the two key pairs (DCA BE)

```bash
# send key
openssl genpkey -algorithm ed25519 -out dca-send.pem
openssl pkey -in dca-send.pem -pubout -outform DER | tail -c 32 | base64    # → wallet ed25519_public_key
# P2P key
openssl genpkey -algorithm ed25519 -out dca-p2p.pem
openssl pkey -in dca-p2p.pem -pubout -outform DER | tail -c 32 | base64     # → p2p service_public_key
```

- Store both **private keys** in DCA BE's secret store, for example Railway variables `BDB_SEND_PRIVATE_KEY_PEM` and `BDB_P2P_PRIVATE_KEY_PEM` holding the PEM text. Never commit them.
- Send only the two **public keys** (44-character base64 each) to the bot operator, saying which is which.
- **Use a separate key pair for each environment** (dev and prod, four keys in total). The host isn't signed, so a request captured on one environment would be accepted on another that shares the key.
- The bot refuses placeholder or weak public keys (for example 32 zero bytes) at startup.

### 2.2 Bot config (bot operator)

```yaml
api:
  allowed_networks: []          # optional: if set, DCA BE's egress IP must be listed
  send:
    enabled: true
    allowed_networks: []        # optional: DCA BE's IPs for the send API
    whitelisted_wallets:
      BiccoindeepaDSATest:                 # = X-Wallet-ID
        username: "<telegram username that owns the DCA wallet>"
        ed25519_public_key: "<DCA BE send public key>"
        can_collect: true                  # allow /api/v1/collect (§4.7)
        # no hmac_secret: a leftover hmac_secret stops the send API
  p2p:
    enabled: true
    service_public_key: "<DCA BE P2P public key>"
    allowed_networks: []        # optional: DCA BE's IPs for /api/v1/init and /api/v1/p2p/*
    oidc_enabled: true          # accept the web app's Log In with Telegram
    init_data_max_age: 3600     # max age of the Telegram login sent to /api/v1/init
    max_amount: 100000          # sats per transfer
    daily_limit: 500000         # sats per user per UTC day
dca:
  wallet_username: "<DCA wallet telegram username>"   # deposits to this user trigger the callback
  api_url: "<DCA BE base URL>"
```

> **IP allowlists and Railway:** Railway's outbound IPs change unless you use static egress IPs. Without a fixed IP, leave the `allowed_networks` lists empty. The Ed25519 signature still protects every call.

---

## 3. Signing DCA wallet calls (send key)

Used for every endpoint in §4.

### 3.1 Headers

| Header | Value |
|---|---|
| `Content-Type` | `application/json` |
| `X-Wallet-ID` | Your wallet ID, e.g. `BiccoindeepaDSATest` |
| `X-Timestamp` | Unix time in seconds. Must be within ±300 s of the bot's clock |
| `X-Nonce` | 16–128 random chars `[A-Za-z0-9_-]`, **new for every request** |
| `X-Signature` | Base64 Ed25519 signature of the signing string below |

### 3.2 Signing string

Seven lines joined with `\n` (no trailing newline):

```
BDB-SEND-ED25519-V1
<METHOD>                 POST or GET
<PATH>                   exactly as sent, still percent-encoded, e.g. /api/v1/send
<QUERY>                  raw query without "?", exactly as sent, or empty
<TIMESTAMP>              same as X-Timestamp
<NONCE>                  same as X-Nonce
<hex sha256 of the exact body bytes>    (sha256 of "" for GET)
```

- **Sign the exact bytes you send.** Serialize the JSON once, hash it, and send that same string.
- **Each nonce works once.** A repeat gets `401 Request already processed`. Sign every retry again, with a new nonce and timestamp.
- **Keep the clock in sync** (NTP). More than 5 minutes of drift gives `401 Request expired`.
- The maximum body size is 1 MiB.

### 3.3 Reference client (Node.js / TypeScript)

```ts
import crypto from "crypto";

const BASE = process.env.BDB_BASE_URL!;                  // e.g. https://bitcoindeepa.com
const WALLET_ID = process.env.BDB_WALLET_ID!;            // e.g. BiccoindeepaDSATest
const SEND_KEY = crypto.createPrivateKey(process.env.BDB_SEND_PRIVATE_KEY_PEM!);

export const sha256hex = (b: string) => crypto.createHash("sha256").update(b).digest("hex");

export async function bdb(method: "GET" | "POST", pathWithQuery: string, body?: object) {
  const [path, query = ""] = pathWithQuery.split("?");
  const raw = body ? JSON.stringify(body) : "";           // serialize ONCE
  const ts = Math.floor(Date.now() / 1000).toString();
  const nonce = crypto.randomBytes(16).toString("hex");
  const msg = ["BDB-SEND-ED25519-V1", method, path, query, ts, nonce, sha256hex(raw)].join("\n");
  const res = await fetch(BASE + pathWithQuery, {
    method,
    headers: {
      "Content-Type": "application/json",
      "X-Wallet-ID": WALLET_ID, "X-Timestamp": ts, "X-Nonce": nonce,
      "X-Signature": crypto.sign(null, Buffer.from(msg), SEND_KEY).toString("base64"),
    },
    body: body ? raw : undefined,                         // send the SAME bytes
  });
  const text = await res.text();
  let json: any; try { json = JSON.parse(text); } catch { json = { raw: text }; }
  return { status: res.status, body: json };
}
```

### 3.4 Reference client (Python)

```python
import base64, hashlib, json, os, time, requests
from cryptography.hazmat.primitives.serialization import load_pem_private_key

BASE = os.environ["BDB_BASE_URL"]
WALLET_ID = os.environ["BDB_WALLET_ID"]
KEY = load_pem_private_key(os.environ["BDB_SEND_PRIVATE_KEY_PEM"].encode(), password=None)

def bdb(method, path_with_query, body=None):
    path, _, query = path_with_query.partition("?")
    raw = json.dumps(body, separators=(",", ":")).encode() if body is not None else b""
    ts, nonce = str(int(time.time())), os.urandom(16).hex()
    msg = "\n".join(["BDB-SEND-ED25519-V1", method, path, query, ts, nonce, hashlib.sha256(raw).hexdigest()])
    headers = {
        "Content-Type": "application/json", "X-Wallet-ID": WALLET_ID, "X-Timestamp": ts,
        "X-Nonce": nonce, "X-Signature": base64.b64encode(KEY.sign(msg.encode())).decode(),
    }
    r = requests.request(method, BASE + path_with_query, data=raw or None, headers=headers, timeout=100)
    return r.status_code, r.json()
```

> Use an HTTP timeout of **at least 95 s** for `POST /api/v1/send`. An invoice payment can wait up to 60 s for settlement before it answers.

---

## 4. DCA wallet endpoints

All of these are signed as in §3 and only work while `api.send.enabled` is true.

### 4.1 `POST /api/v1/send` — send sats

```json
{ "to": "johndoe", "amount": 1000, "memo": "dca-order-8f2c" }
```

| Field | Required | Notes |
|---|---|---|
| `to` | yes | Telegram username (no `@`), numeric Telegram ID, Lightning address, or bolt11 invoice (`lnbc…`) |
| `amount` | yes\* | Sats. \*Ignored for bolt11 invoices, which carry their own amount |
| `memo` | no | **Use a unique memo per order.** A memo that already completed is rejected, so it works as your idempotency key |

| HTTP | Body | Meaning | DCA BE should |
|---|---|---|---|
| `200` | `success: true` | Sent. Invoices also return `payment_hash`, `fee`, `preimage` | Mark the order paid |
| `202` + `"status": "pending"` | `payment_hash` | Invoice payment still routing | Poll §4.3. **Don't** retry with a new invoice |
| `202` + `"status": "awaiting_approval"` | `message` ends with `Transaction ID: <id>` | Above `admin_approval_threshold`: waiting for an operator in Telegram | Poll §4.2 with that ID |
| `400` | `success: false`, `message` | Rejected, nothing sent (bad recipient, limits, duplicate memo, low balance, invoice rejected) | Fix or fail the order |
| `401` / `403` | — | Signature, timestamp, nonce or IP problem (§8.1) | Alert; don't retry blindly |

Default limits: `min_amount` 1, `max_amount` 1,000,000, `admin_approval_threshold` 100,000 sats, each with per-wallet overrides. Invoices also need about a 2% routing-fee reserve.

### 4.2 `GET /api/v1/send/status/{transaction_id}` — approval status

```json
{ "id": "pending-…", "status": "pending", "amount": 150000, "memo": "dca-order-8f2c",
  "request_timestamp": "…", "expiry_time": "…" }
```

`status` goes `pending` → `approved` → `executed`, or ends `rejected` / `expired` (after 24 h). An approved invoice payment can report `in_flight` with a `payment_hash`; continue with §4.3.

### 4.3 `GET /api/v1/send/payment/{payment_hash}` — invoice settlement

```json
{ "success": true, "status": "settled", "payment_hash": "…", "preimage": "…", "fee": 1 }
```

| `status` | Meaning | DCA BE should |
|---|---|---|
| `settled` | Paid | Mark paid; store `payment_hash` and `preimage` |
| `failed` | Not paid; the sats are back | Safe to retry with a new invoice |
| `pending` / `unknown` | Still routing, or can't be looked up yet | Poll with backoff (5 s, 10 s, 30 s, then every 60 s). **Not** a failure |

### 4.4 `POST /api/v1/userbalance` — wallet balance

```json
{ "telegram_id": 123456789 }
```
→ `{ "success": true, "balance": 5000, "balance_lkr": "…", "username": "…", ... }`

### 4.5 `POST /api/v1/usertransactions` — a user's history

```json
{ "telegram_id": 123456789, "limit": 100, "offset": 0 }
```
→ `{ "success": true, "count": 2, "transactions": [ … ] }`, newest first. **Every entry has the same fields**, with `""` or `0` when a value is unknown:

```json
{ "id": 812, "time": "2026-11-20T03:30:00Z", "direction": "incoming", "from_id": 222, "to_id": 123456789, "from_user": "@bob", "to_user": "@alice", "type": "p2p_send", "amount": 1000, "amount_lkr": "…", "memo": "lunch", "success": true, "fee": 0, "status": "success", "payment_hash": "9c1f…" }
{ "id": 0, "time": "2026-11-20T03:41:12Z", "direction": "incoming", "from_id": 0, "to_id": 123456789, "from_user": "", "to_user": "", "type": "lightning", "amount": 10, "amount_lkr": "…", "memo": "thanks!", "success": true, "fee": 0, "status": "success", "payment_hash": "a3f1…" }
```

| Field | Internal transfer | Lightning payment (`type: "lightning"`) |
|---|---|---|
| `id` | Row ID | `0` |
| `status` | `success` or `failed` | `success`, `pending` or `failed` (from LNbits) |
| `fee` | `0` | LNbits fee, in sats |
| `payment_hash` | The internal invoice's hash, or `""` | The LNbits payment hash |
| `memo` | The transfer's memo | LNbits' memo; if empty, the payer's comment on a Lightning-address payment |
| `from_user` / `to_user`, `from_id` / `to_id` | Both sides | The user's side; the other side is `""` / `0` |

- **Internal transfers** (tips, sends, P2P, API sends, collect, …) come from the bot's table. **Lightning payments** (`/pay`, `/invoice`, Lightning-address receipts) come from the user's LNbits wallet. Internal transfers that also show in LNbits are listed once.
- If LNbits can't be reached, only internal transfers are returned.

`limit` defaults to 100 (maximum 250). Page with `offset`. Only the newest 1000 entries can be paged.

### 4.6 `GET /api/v1/referral/lookup?code=…` — users who joined with a code

→ `{ "code": "PROMO2024", "count": 2, "users": [ { "telegram_id": 1, "username": "…", "joined_at": "…" } ] }`

The query string is signed: sign `code=PROMO2024` exactly as sent.

### 4.7 Collect — charge a user (sell orders)

Needs `can_collect: true` on the wallet. **The sats move at once, with no approval step**: the signed call is enough. The bot sends no chat message about it. Only call it after the user agreed in the app.

**`POST /api/v1/collect`**

Signed with the **send key**, but with its own signed text. The user to charge goes in **`X-BDB-User: <telegram_id>`**, and is signed as the seventh line:

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

```json
{ "amount": 50000, "memo": "Sell order #812", "external_id": "order-812" }
```

| Field | Notes |
|---|---|
| `X-BDB-User` (header) | The user to charge: numeric Telegram ID. They must have a bot wallet (started the bot, or `/api/v1/init`, §6.1), otherwise `404 recipient_not_found` |
| `amount` | Sats, at most the wallet's `max_amount` |
| `memo` | Optional. Shown in the user's history |
| `external_id` | 8–128 chars, unique per charge: **use your order ID**. A retry with it returns the same request (`200` instead of `201`) and never charges twice. The same ID with a different user, amount or memo → `409 idempotency_conflict` |

Returns `201` with the request, already charged. At most 10 charges per wallet per user per hour.

**`GET /api/v1/collect/{id}`**, to poll it (signed like §3, no `X-BDB-User`):

```json
{
  "id": "col-…", "status": "approved", "paid": true,
  "amount": 50000, "amount_lkr": "…", "memo": "Sell order #812", "external_id": "order-812",
  "payer": { "telegram_id": 111, "username": "alice" },
  "collector": { "telegram_id": 999, "username": "BiccoindeepaDSA", "name": "…" },
  "expires_at": "…", "created_at": "…",
  "transfer": { "id": "p2p-col-…", "status": "completed", "…": "…" }
}
```

| `status` | Meaning |
|---|---|
| `approved` | Charged. `transfer` shows the payment; **`paid: true` once it completed**. A `failed` transfer (for example, low balance) is final: use a new `external_id` to try again |
| `pending` | Rare: the bot stopped before paying. Retry with the same `external_id` |
| `expired` | A `pending` request not retried within 15 minutes. Nothing was paid |

A request can never pay twice.

Errors use the P2P shape `{ "code", "message" }`:

| HTTP | `code` |
|---|---|
| 400 | `invalid_amount`, `amount_too_large`, `memo_too_long`, `invalid_external_id`, `invalid_recipient`, `self_collect` |
| 403 | `collect_not_allowed` (no `can_collect`), `collector_not_allowed` |
| 404 | `recipient_not_found`, `not_found` |
| 409 | `idempotency_conflict`, `in_progress` |
| 429 | `rate_limited` |

---

## 5. Signing user-wallet calls (P2P key)

Used for `/api/v1/init` and every `/api/v1/p2p/*` endpoint. Headers: `X-Timestamp`, `X-Nonce`, `X-Signature`, with the same rules as §3. There's **no** `X-Wallet-ID`.

**The Telegram login is checked once, at login.** After that, DCA BE's own session says who the user is:

| Call | How the user is named | Signed text, first line | Signed text, seventh line |
|---|---|---|---|
| **Login:** `POST /api/v1/init` | The Telegram credential: `X-Telegram-Init-Data` or `X-Telegram-ID-Token` | `BDB-P2P-ED25519-V1` | SHA-256 of the credential |
| **Every other call** (`/api/v1/p2p/*`) | `X-BDB-User: <telegram_id>` | `BDB-P2P-ED25519-V2` | The Telegram ID, exactly as in `X-BDB-User` |

These are the same URLs. `…-V1` and `…-V2` only name the format of the signed text.

### 5.1 Login: `POST /api/v1/init` with the Telegram credential

| From | DCA BE receives | Forward as |
|---|---|---|
| Mini App | `window.Telegram.WebApp.initData` (raw string) | `X-Telegram-Init-Data` |
| Web app | The `id_token` from Log In with Telegram (OIDC, scopes `openid profile`, client = the bot) | `X-Telegram-ID-Token` |

- Forward **exactly one** credential, **unchanged**.
- The bot checks it with Telegram. It can be at most **1 hour** old (`init_data_max_age`).
- On success, the user's wallet exists. After that, name them with `X-BDB-User`.

Signing string:

```
BDB-P2P-ED25519-V1
<METHOD>
<PATH>                       exactly as sent (percent-encoded), i.e. /api/v1/init
<QUERY>
<TIMESTAMP>
<NONCE>
<hex sha256 of the credential value>     (the initData or id_token string)
<hex sha256 of the exact body bytes>
```

### 5.2 Every other call: `X-BDB-User`

Send `X-BDB-User: <telegram_id>` and **no** Telegram header:

```
BDB-P2P-ED25519-V2
<METHOD>
<PATH>                       exactly as sent (percent-encoded), e.g. /api/v1/p2p/send
<QUERY>
<TIMESTAMP>
<NONCE>
<telegram_id>                exactly as in X-BDB-User
<hex sha256 of the exact body bytes>
```

| What happens | Answer |
|---|---|
| A user with no bot wallet | `403 sender_not_found`: call `/api/v1/init` first |
| `X-BDB-User` missing, or a Telegram credential sent instead | `401 unauthorized` |
| `X-BDB-User` sent to `/api/v1/init` | `401 login_required` |

There's no login-age limit on these calls: DCA BE's session decides how long the user stays logged in. Use server-side sessions (`HttpOnly` cookies with refresh tokens). Never put the user ID in something the browser can change.

> **Security:** the bot trusts DCA BE about who the user is, **payments included**. If the P2P key leaks, it can pay from any user's wallet, up to `daily_limit`. Passkeys will add per-payment approval ([session-passkey-spec.md](session-passkey-spec.md)). Keep the P2P key in a secret store.

### 5.3 Reference client (Node.js)

```ts
const P2P_KEY = crypto.createPrivateKey(process.env.BDB_P2P_PRIVATE_KEY_PEM!);

type Who = { initData: string } | { idToken: string } | { telegramId: number };

export async function bdbP2P(method: "GET" | "POST", path: string, who: Who, body?: object) {
  const raw = body ? JSON.stringify(body) : "";
  const ts = Math.floor(Date.now() / 1000).toString();
  const nonce = crypto.randomBytes(16).toString("hex");
  const headers: Record<string, string> = { "Content-Type": "application/json", "X-Timestamp": ts, "X-Nonce": nonce };
  let lines: string[];
  if ("telegramId" in who) {                                   // every call except login
    const uid = String(who.telegramId);
    headers["X-BDB-User"] = uid;
    lines = ["BDB-P2P-ED25519-V2", method, path, "", ts, nonce, uid, sha256hex(raw)];
  } else {                                                     // login: POST /api/v1/init
    const value = "initData" in who ? who.initData : who.idToken;
    headers["initData" in who ? "X-Telegram-Init-Data" : "X-Telegram-ID-Token"] = value;
    lines = ["BDB-P2P-ED25519-V1", method, path, "", ts, nonce, sha256hex(value), sha256hex(raw)];
  }
  headers["X-Signature"] = crypto.sign(null, Buffer.from(lines.join("\n")), P2P_KEY).toString("base64");
  return fetch(BASE + path, { method, headers, body: body ? raw : undefined });
}

// login:   await bdbP2P("POST", "/api/v1/init", { initData }, {});
// later:   await bdbP2P("POST", "/api/v1/p2p/send", { telegramId: 111 }, { to_telegram_id: 222, amount: 1000, idempotency_key: uuid() });
```

---

## 6. User-wallet endpoints

### 6.1 `POST /api/v1/init` — create the wallet / log in

The **login** call, and the only one that carries the Telegram credential (§5.1). It creates the user's wallet if they never pressed Start in the bot, and it links the user for `X-BDB-User`. It's safe to call on every login.

```json
{ "referral_code": "PROMO2024" }       // optional; letters, digits, _ and -, up to 64; new users only
```

→ `201` for a new wallet, `200` if the user already had one:

```json
{
  "telegram_id": 111, "username": "alice", "name": "Alice",
  "balance": 0, "balance_lkr": "…",
  "limits": { "max_amount": 100000, "daily_limit": 500000, "daily_used": 0, "daily_remaining": 500000 },
  "created": true,
  "can_message": false
}
```

- **`can_message`** appears only for a new wallet. `false` means the bot can't message the user, so they get no receipts in the chat; payments still work.
  - **Mini App:** call `Telegram.WebApp.requestWriteAccess()` **before** `init`.
  - **Web app:** ask for message permission at Log In with Telegram.
- **Limits:** at most 30 new wallets per minute across all users. Banned users and bot accounts get `403`.

### 6.2 `GET /api/v1/p2p/me`

```json
{ "telegram_id": 111, "username": "alice", "name": "Alice", "balance": 250000, "balance_lkr": "…",
  "limits": { "max_amount": 100000, "daily_limit": 500000, "daily_used": 60000, "daily_remaining": 440000 } }
```

### 6.3 Send to another bot user

1. **`POST /api/v1/p2p/resolve`** `{ "to": "@bob" }` (also accepts `"bob"` or `"123456789"`) → `{ "telegram_id": 222, "username": "bob", "name": "Bob Silva" }`. Show the `name` on the review screen. Users who can't receive get `404 recipient_not_found`.
2. **`POST /api/v1/p2p/send`** `{ "to_telegram_id": 222, "amount": 1000, "memo": "lunch", "idempotency_key": "<uuid>" }` → a transfer (§6.5).

### 6.4 Pay a Lightning invoice

1. **`POST /api/v1/p2p/invoice/decode`** `{ "invoice": "lnbc…" }` → `{ "amount": 21000, "amount_lkr": "…", "description": "Coffee", "payment_hash": "…", "expires_at": "…", "over_limit": false }`
2. **`POST /api/v1/p2p/invoice/pay`** `{ "invoice": "lnbc…", "memo": "", "idempotency_key": "<uuid>" }`. It waits up to 15 s, then returns `202` `in_flight` if the payment hasn't settled; poll §6.5. The bot never pays an invoice twice.

### 6.5 Transfers — `GET /api/v1/p2p/transfers/{id}`

```json
{ "id": "p2p-…", "kind": "internal", "status": "completed", "amount": 1000, "amount_lkr": "…",
  "fee": 0, "memo": "lunch", "to": { "telegram_id": 222, "username": "bob", "name": "Bob Silva" },
  "payment_hash": "", "description": "", "preimage": "", "error": "", "created_at": "…" }
```

| `status` | HTTP | Meaning | Poll |
|---|---|---|---|
| `completed` | 200 | Done | — |
| `failed` | 200 | Nothing sent; reason in `error`. Final for its `idempotency_key` | — |
| `processing` | 202 | Running | every 2–3 s |
| `in_flight` | 202 | Sent, not settled yet. Can still complete or fail | every 15–30 s |

Low balance and daily-limit failures come back as a `failed` transfer. Only the user who sent a transfer can read it.

### 6.6 Collect — show a charge to the user (§4.7)

| Endpoint | Purpose |
|---|---|
| `GET /api/v1/p2p/collect/{id}` | Show a charge to the user it was taken from: amount, who charged, memo. Anyone else gets `404` |

There's nothing to approve: `POST /api/v1/collect` already took the sats.

### 6.7 Rules for DCA BE and the app UI

- **No confirmation in the bot chat.** The signed call **is** the approval and pays straight away. Always show a review screen (amount, recipient, memo) before `send`, `invoice/pay` or a collect charge. Passkey approval is planned.
- **`idempotency_key`:** a new UUID per payment, reused on every retry of **that** payment. The same key always returns the same transfer, so a retry can never pay twice. Reusing a key for a different payment → `409`.
- **`X-BDB-User` on every call:** any user with a bot wallet can be named. A user with no wallet gets `403 sender_not_found`; create it with `POST /api/v1/init`.
- **Limits:** `max_amount` per transfer and `daily_limit` per user per UTC day. 10 sends or payments per minute, 30 resolves or decodes per minute, 30 status polls per minute.
- **Recipients** must have started the bot or gone through `init`. No sending to yourself.

### 6.8 P2P errors

Body: `{ "code": "…", "message": "…" }`. `message` is written for the user and can be shown as-is.

| HTTP | `code` |
|---|---|
| 400 | `invalid_json`, `invalid_amount`, `amount_too_large`, `memo_too_long`, `invalid_idempotency_key`, `invalid_recipient`, `self_send`, `invalid_invoice`, `amountless_invoice`, `invoice_expired`, `invalid_referral_code`, `invalid_request` |
| 401 | `unauthorized`: on `init`, the Telegram login is missing, invalid, too old or sent twice. Elsewhere, `X-BDB-User` is missing or bad, or a Telegram credential was sent instead |
| 401 | `bad_service_signature` (DCA BE's P2P signature is wrong, expired or replayed: a server bug) |
| 401 | `login_required`: `X-BDB-User` sent to `init` |
| 403 | `forbidden` (IP), `sender_not_found` (no wallet: call `/api/v1/init`), `sender_not_allowed` (banned) |
| 404 | `recipient_not_found`, `not_found` |
| 409 | `idempotency_conflict`, `in_progress`, `request_closed` |
| 413 | `too_large` |
| 429 | `rate_limited` |
| 503 | `wallet_unavailable` (wallet creation failed; retry) |

---

## 7. Callback: bot → DCA BE

When sats arrive in the wallet of `dca.wallet_username`, the bot calls:

```
POST {dca.api_url}/transaction/reset-retry-counts
Content-Type: application/json
(empty body)
```

- Timeout 10 s, fire-and-forget: no retries, and the response is ignored.
- **It is not authenticated.** Make the handler safe to call any number of times: it should only reset retry counters, never move funds. Optionally allow only the bot server's IP.

---

## 8. Errors

### 8.1 Send-key signing layer (§3)

| HTTP | Body | Cause | Fix |
|---|---|---|---|
| `401` | `Invalid signature` | Unknown wallet ID, wrong key, or signing-string mismatch | Check the wallet ID and key pair, and hash the exact bytes sent |
| `401` | `Invalid timestamp` / `Request expired` | Missing timestamp, or clock drift > 300 s | Sync the clock (NTP) |
| `401` | `Invalid nonce` | Missing or wrong format | 16–128 chars `[A-Za-z0-9_-]` |
| `401` | `Request already processed` | Nonce reused | New nonce per request, including retries |
| `400` | `Could not read request body` | The connection dropped while sending | Retry |
| `403` | `Access denied` | Caller IP not allowed | Ask the operator to add the IP, or clear the list |
| `413` | `Request too large` | Body over 1 MiB | — |

### 8.2 P2P

See §6.8.

---

## 9. Recommended flows

### 9.1 DCA payout

```
1. (optional) POST /userbalance       → enough balance (+2% for invoices)?
2. POST /send  { to, amount, memo: "dca-<orderId>" }
     200 → paid
     202 pending (invoice)  → poll GET /send/payment/{payment_hash} until settled | failed
     202 awaiting_approval  → poll GET /send/status/{id} until executed | rejected | expired
     400 → order failed, nothing sent
     timeout / 5xx → resend with the SAME memo ("already completed" = it went through),
                     or check /usertransactions or /send/payment first
```

### 9.2 Sell order (collect)

```
1. User reviews the sell order in the app and taps Sell
2. POST /collect + X-BDB-User: <telegram_id>  { amount, memo, external_id: "<orderId>" }
     201 → charged
     timeout / 5xx → resend with the SAME external_id (never charges twice)
3. Poll GET /collect/{id} every 3–5 s:
     paid: true                         → sats received: pay the user LKR
     transfer processing / in_flight    → keep polling
     transfer failed                    → order failed (for example, low balance)
     pending                            → resend step 2 with the SAME external_id
     expired                            → order closed, nothing paid
```

**Only pay the user out (LKR) after `paid: true`.**

### 9.3 User login and session

```
App open → get initData (Mini App) or id_token (web app)
→ DCA BE: POST /api/v1/init with that credential       → wallet ready
→ DCA BE creates its own session (HttpOnly cookie, refresh tokens)
→ every later call: X-BDB-User (signed text …-V2)       → no Telegram re-login
```

Log in again (a new `/api/v1/init`) on a new device, or after DCA BE's session ends.

---

## 10. Checklist

**Bot operator**
- [ ] Set the wallet's `ed25519_public_key` and **remove** `hmac_secret`. Set `can_collect: true`.
- [ ] Set `api.p2p.enabled`, `service_public_key` and `oidc_enabled`.
- [ ] Use a different key pair for dev and prod.

**DCA BE**
- [ ] Send and P2P private keys are stored as secrets, never committed.
- [ ] Every call has a new nonce and timestamp, the exact bytes are signed, and the query string is signed.
- [ ] HTTP timeout ≥ 95 s for `/send`. Handle both `202` cases (§4.1).
- [ ] Payouts use a unique memo per order. Collect uses the order ID as `external_id`, and settles **only** on `paid: true`.
- [ ] Login calls `POST /api/v1/init` with the Telegram credential, after `requestWriteAccess()` in the Mini App.
- [ ] Login: forward exactly one Telegram credential, unchanged, to `/api/v1/init` only. Then use server-side sessions and `X-BDB-User` (signed text `…-V2`) on every other call.
- [ ] A review screen before every payment, a new `idempotency_key` per payment, polling per §6.5.
- [ ] The `reset-retry-counts` handler is safe to call repeatedly.
- [ ] If IP allowlists are used, DCA BE has a static egress IP and it's listed.
- [ ] Tested end-to-end on the dev bot. Deploy the bot and DCA BE **together**: after the bot deploys, HMAC requests get `401`.

---

## 11. Not for DCA BE

| API | Auth | Used by |
|---|---|---|
| `/api/v1/analytics/*` | Ed25519 with `X-Key-ID`, `BDB-ANALYTICS-ED25519-V1` | Data team |
| `/api/v1/balance`, `/payinvoice`, `/createinvoice`, … | A user's own wallet key | End-user wallet clients |
