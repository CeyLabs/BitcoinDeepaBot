# DCA Backend ↔ BitcoinDeepa Bot — API Integration Guide

How the DCA backend (DCA BE) talks to the BitcoinDeepa bot after the security changes in #84 (IP allowlist), #86 (Ed25519 request signing) and #85 (P2P), plus the collect API (#89). DCA BE also serves the **Telegram Mini App and the web app**, so it relays their P2P sends too. This guide covers setup, signing, every endpoint DCA BE calls, the callback the bot makes to DCA BE, and a migration checklist from HMAC.

> **Breaking change:** HMAC signing (`X-HMAC-Signature`) is removed. Every call from DCA BE must be signed with Ed25519, or it gets `401`.

---

## 1. Overview

```
             (1) wallet API, signed with the SEND key
 DCA BE ─────────────────────────────────────────────►  Bot  /api/v1/send, /collect, /userbalance, ...
   │
   │         (2) P2P relay, signed with the P2P key + the user's Telegram credential
   ├─────────────────────────────────────────────────►  Bot  /api/v1/p2p/*
   │  ▲
   │  └── Mini App (initData) / web app (Telegram login)
   ▲
   │         (3) callback: POST /transaction/reset-retry-counts
   └──────────────────────────────────────────────────  Bot  (when the DCA wallet receives sats)
```

1. **DCA's own payouts and collections.** DCA BE sends sats from its whitelisted wallet, asks users to pay it (collect, for sellers), checks balances, and polls payment status. Signed with the **send key** (§3–§4).
2. **P2P relay.** Mini App and web app users send sats and pay invoices from **their own** wallets. DCA BE forwards their requests, signed with the **P2P key**, plus the user's own Telegram credential. DCA BE cannot send on a user's behalf without that credential (§5).
3. **Callback.** When the DCA wallet receives a deposit, the bot calls `POST {dca.api_url}/transaction/reset-retry-counts` so DCA BE can retry failed buys (§6).

### The two keys

| Key | Signs | Bot config | If it leaks |
|---|---|---|---|
| **Send key** | (1) DCA payouts and collect requests | `api.send.whitelisted_wallets.<id>.ed25519_public_key` | Someone can send sats **from the DCA wallet**, up to its limits, and send users payment prompts (they still have to approve each one) |
| **P2P key** | (2) P2P relay | `api.p2p.service_public_key` | Little on its own: every P2P call also needs that user's Telegram credential |

Keep them **separate**, so either one can be rotated or revoked without touching the other. A signature made with one can't be used on the other API anyway: each signs a different first line (`BDB-SEND-…` vs `BDB-P2P-…`).

| | Dev | Production |
|---|---|---|
| Bot base URL | `<dev bot URL>` | `https://bitcoindeepa.com` |
| Wallet ID (`X-Wallet-ID`) | `BiccoindeepaDSATest` | given by the operator |
| Telegram bot ID | `8219648988` | given by the operator |

---

## 2. One-time setup

### 2.1 Create the two key pairs (on DCA BE's side)

```bash
# send key
openssl genpkey -algorithm ed25519 -out dca-send.pem
openssl pkey -in dca-send.pem -pubout -outform DER | tail -c 32 | base64    # → wallet ed25519_public_key
# P2P key
openssl genpkey -algorithm ed25519 -out dca-p2p.pem
openssl pkey -in dca-p2p.pem -pubout -outform DER | tail -c 32 | base64     # → p2p service_public_key
```

- Store both **private keys** in DCA BE's secret store, e.g. Railway variables `BDB_SEND_PRIVATE_KEY_PEM` and `BDB_P2P_PRIVATE_KEY_PEM` holding the PEM text. Never commit them.
- Send only the two **public keys** (44-character base64 each) to the bot operator, saying which is which.
- Use different key pairs for dev and production (four keys in total).

### 2.2 Bot config (done by the bot operator)

```yaml
api:
  allowed_networks: []          # optional, #84: if set, DCA BE's egress IP must be listed
  send:
    enabled: true
    allowed_networks: []        # optional: DCA BE's IPs for the send API
    whitelisted_wallets:
      BiccoindeepaDSATest:                 # = X-Wallet-ID
        username: "<telegram username that owns the sending wallet>"
        ed25519_public_key: "<DCA BE public key>"
        # no hmac_secret: a leftover hmac_secret turns the send API off
        can_collect: true                  # allow /api/v1/collect (§4.7)
  p2p:
    enabled: true
    service_public_key: "<DCA BE P2P public key>"
    allowed_networks: []        # optional: DCA BE's IPs for /api/v1/p2p/*
    oidc_enabled: true          # accept the web app's Telegram login
dca:
  wallet_username: "<DCA wallet telegram username>"   # deposits to this user trigger the callback
  api_url: "<DCA BE base URL>"
```

> **IP allowlists and Railway:** Railway's outbound IPs change unless you use static egress IPs. If DCA BE doesn't have a fixed IP, leave both `allowed_networks` empty. The Ed25519 signature still protects every call.

---

## 3. Signing every request

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
<PATH>                   e.g. /api/v1/send  (no host, no query)
<QUERY>                  raw query without "?", exactly as sent, or empty
<TIMESTAMP>              same as X-Timestamp
<NONCE>                  same as X-Nonce
<hex sha256 of the exact body bytes>    (sha256 of "" for GET)
```

Rules:
- **Sign the exact bytes you send.** Serialize the JSON once, hash it, and send that same string. Re-serializing breaks the signature.
- **Each nonce works once.** A repeated nonce gets `401 Request already processed`. A retry must be signed again with a new nonce and timestamp.
- **Keep the server clock in sync** (NTP). More than 5 minutes of drift gives `401 Request expired`.
- Method, path, query and body are all signed, so nothing can be changed in transit.
- Maximum body size: 1 MiB.

### 3.3 Reference client (Node.js / TypeScript)

```ts
import crypto from "crypto";

const BASE = process.env.BDB_BASE_URL!;                  // e.g. https://bitcoindeepa.com
const WALLET_ID = process.env.BDB_WALLET_ID!;            // e.g. BiccoindeepaDSATest
const KEY = crypto.createPrivateKey(process.env.BDB_SEND_PRIVATE_KEY_PEM!);

const sha256hex = (b: string) => crypto.createHash("sha256").update(b).digest("hex");

export async function bdb(method: "GET" | "POST", pathWithQuery: string, body?: object) {
  const [path, query = ""] = pathWithQuery.split("?");
  const raw = body ? JSON.stringify(body) : "";           // serialize ONCE
  const ts = Math.floor(Date.now() / 1000).toString();
  const nonce = crypto.randomBytes(16).toString("hex");
  const msg = ["BDB-SEND-ED25519-V1", method, path, query, ts, nonce, sha256hex(raw)].join("\n");
  const sig = crypto.sign(null, Buffer.from(msg), KEY).toString("base64");

  const res = await fetch(BASE + pathWithQuery, {
    method,
    headers: {
      "Content-Type": "application/json",
      "X-Wallet-ID": WALLET_ID,
      "X-Timestamp": ts,
      "X-Nonce": nonce,
      "X-Signature": sig,
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

> Use an HTTP timeout of **at least 95 s** for `POST /api/v1/send`: an invoice payment can wait up to `settlement_timeout` (60 s) before answering.

---

## 4. Endpoints DCA BE can call

All are under `/api/v1/`, signed as in §3, and only work while `api.send.enabled` is true.

### 4.1 `POST /api/v1/send` — send sats

```json
{ "to": "johndoe", "amount": 1000, "memo": "dca-order-8f2c" }
```

| Field | Required | Notes |
|---|---|---|
| `to` | yes | Telegram username (no `@`), numeric Telegram ID, Lightning address, or bolt11 invoice (`lnbc…`) |
| `amount` | yes\* | Sats. \*Ignored for bolt11 invoices: the invoice amount is used |
| `memo` | no | **Use a unique memo per order.** A memo that already completed is rejected, which makes it your idempotency key |

**Responses**

| HTTP | Body | Meaning | DCA BE should |
|---|---|---|---|
| `200` | `success: true` | Sent. Invoices also return `payment_hash`, `fee`, `preimage` | Mark the order paid |
| `202` + `"status": "pending"` | `payment_hash` | Invoice payment still routing | Poll §4.3 with that `payment_hash`. **Don't** retry with a new invoice |
| `202` + `"status": "awaiting_approval"` | `message` ends with `Transaction ID: <id>` | Above `admin_approval_threshold`: waiting for approval in Telegram | Take the ID after `Transaction ID: ` and poll §4.2 |
| `400` | `success: false`, `message` | Rejected; nothing was sent (bad recipient, limits, duplicate memo, low balance, invoice rejected) | Fix or fail the order |
| `401` / `403` | — | Signature, timestamp, nonce, or IP problem (see §7) | Alert; don't retry blindly |

Default limits (bot config): `min_amount` 1, `max_amount` 1,000,000, `admin_approval_threshold` 100,000 sats. Per-wallet overrides are possible. Invoices also need about a 2% routing-fee reserve on top of the amount.

### 4.2 `GET /api/v1/send/status/{transaction_id}` — approval status

For sends that returned `202` waiting for approval.

```json
{ "id": "pending-…", "status": "pending", "amount": 150000, "memo": "dca-order-8f2c",
  "request_timestamp": "…", "expiry_time": "…" }
```

`status`: `pending` → `approved` → `executed`, or `rejected` / `expired` (after 24 h). An approved invoice payment can also report `in_flight` with a `payment_hash`; continue on §4.3.

### 4.3 `GET /api/v1/send/payment/{payment_hash}` — invoice settlement

```json
{ "success": true, "status": "settled", "payment_hash": "…", "preimage": "…", "fee": 1 }
```

| `status` | Meaning | DCA BE should |
|---|---|---|
| `settled` | Paid | Mark paid; store `payment_hash` + `preimage` |
| `failed` | Not paid; sats are back | Safe to retry with a new invoice |
| `pending` / `unknown` | Still routing, or can't be looked up yet | Poll again with backoff (e.g. 5 s, 10 s, 30 s, then every 60 s). **Not** a failure |

### 4.4 `POST /api/v1/userbalance` — wallet balance

```json
{ "telegram_id": 123456789 }
```
→ `{ "success": true, "balance": 5000, "balance_lkr": "…", "username": "…", ... }`

Use it as a pre-flight check before large sends.

### 4.5 `POST /api/v1/usertransactions` — a user's internal transactions

```json
{ "telegram_id": 123456789, "limit": 100, "offset": 0 }
```
→ `{ "success": true, "count": 2, "transactions": [ { "id": 1, "time": "…", "direction": "incoming", "from_user": "…", "to_user": "…", "amount": 1000, "memo": "…", "success": true } ] }`

`limit` defaults to 100, max 250. Use `offset` to page.

### 4.6 `GET /api/v1/referral/lookup?code=…` — users who joined with a code

→ `{ "code": "PROMO2024", "count": 2, "users": [ { "telegram_id": 1, "username": "…", "joined_at": "…" } ] }`

Remember the query string is signed: sign `code=PROMO2024` exactly as sent.

### 4.7 Collect: ask a user to pay you

Needs `can_collect: true` on the wallet. Nothing moves until the user approves, in the bot chat or in the Mini App / web app (§5.3). Full reference: [`docs/collect-api.md`](collect-api.md).

| Endpoint | Purpose |
|---|---|
| `POST /api/v1/collect` `{ to, amount, memo, external_id }` | Create a request (expires in 15 min). The bot sends the user **Pay / Decline** buttons. |
| `GET /api/v1/collect/{id}` | Poll it: `pending`, `approved` (see `paid` and `transfer`), `declined`, `cancelled`, `expired` |
| `POST /api/v1/collect/{id}/cancel` | Withdraw a pending request |

Use your order ID as `external_id`; retries with it return the same request.

The user approves in one of two places:

- **Bot chat:** they press **✅ Pay**. Nothing for DCA BE to do but poll.
- **Mini App / web app:** DCA BE shows the request and relays approve or decline with the user's credential (§5.3). DCA BE knows the request ID because it created it, so pass the ID to the Mini App (for example in the page URL).

Rules:

- Per-request cap is the wallet's `max_amount`. The P2P daily limit does not apply.
- At most 10 requests per wallet per user per hour (`429 rate_limited`).
- `notified: false` means the bot could not message the user (for example, they blocked the bot). Point them to the Mini App instead.
- A `failed` transfer (for example, the seller's balance is too low) is final. Create a new request with a new `external_id`.
- Errors use the P2P shape `{ "code", "message" }`: `403 collect_not_allowed` (no `can_collect`), `404 recipient_not_found`, `409 idempotency_conflict` / `request_closed`.

---

## 5. P2P relay: Mini App and web app

Mini App and web app users send sats and pay invoices from **their own** bot wallets. DCA BE never holds their funds or decides who the sender is: the sender is always the Telegram user in the credential the user's app hands to DCA BE. Full reference: [`docs/p2p-api.md`](p2p-api.md).

### 5.1 What DCA BE forwards

| From | DCA BE receives | DCA BE sends to the bot |
|---|---|---|
| Mini App | `window.Telegram.WebApp.initData` (raw string) | `X-Telegram-Init-Data: <initData>` |
| Web app | The `id_token` from Log In with Telegram (OIDC; scopes `openid profile`, client = the bot) | `X-Telegram-ID-Token: <id_token>` |

- Forward the credential **unchanged**. Send exactly one of the two headers.
- Keep the web app's ID token in DCA BE's server-side session, not in browser storage.
- Never add anything that names a user (no `telegram_id` from your own DB). The bot ignores it, and it must not be needed.

### 5.2 Signing (P2P key)

Headers: `X-Timestamp`, `X-Nonce`, `X-Signature`. There's **no** `X-Wallet-ID`. The signing string has **eight** lines:

```
BDB-P2P-ED25519-V1
<METHOD>
<PATH>                       exactly as sent (percent-encoded), e.g. /api/v1/p2p/send
<QUERY>
<TIMESTAMP>
<NONCE>
<hex sha256 of the credential value>     (the initData or id_token string)
<hex sha256 of the exact body bytes>
```

```ts
const P2P_KEY = crypto.createPrivateKey(process.env.BDB_P2P_PRIVATE_KEY_PEM!);

// cred: { initData } from the Mini App, or { idToken } from the web app session
export async function bdbP2P(method: "GET" | "POST", path: string, cred: { initData?: string; idToken?: string }, body?: object) {
  const raw = body ? JSON.stringify(body) : "";
  const value = cred.initData ?? cred.idToken!;
  const ts = Math.floor(Date.now() / 1000).toString();
  const nonce = crypto.randomBytes(16).toString("hex");
  const msg = ["BDB-P2P-ED25519-V1", method, path, "", ts, nonce, sha256hex(value), sha256hex(raw)].join("\n");
  return fetch(BASE + path, {
    method,
    headers: {
      "Content-Type": "application/json",
      [cred.initData ? "X-Telegram-Init-Data" : "X-Telegram-ID-Token"]: value,
      "X-Timestamp": ts, "X-Nonce": nonce,
      "X-Signature": crypto.sign(null, Buffer.from(msg), P2P_KEY).toString("base64"),
    },
    body: body ? raw : undefined,
  });
}
```

### 5.3 Endpoints

| Endpoint | Purpose |
|---|---|
| `POST /api/v1/init` `{ referral_code? }` | Create the user's wallet if they never pressed Start (P2P-signed, but not under `/p2p/`). Call it when another endpoint answers `403 sender_not_found`, or on first open. See `can_message` below. |
| `GET /api/v1/p2p/me` | The user's balance and limits (`daily_remaining`, `unconfirmed_remaining`) |
| `POST /api/v1/p2p/resolve` `{ "to": "@bob" }` | Find the recipient; show `name` on the confirm screen |
| `POST /api/v1/p2p/send` `{ to_telegram_id, amount, memo, idempotency_key }` | Send to another bot user |
| `POST /api/v1/p2p/invoice/decode` `{ invoice }` | Show an invoice before paying (`needs_confirmation`, `over_limit`) |
| `POST /api/v1/p2p/invoice/pay` `{ invoice, memo, idempotency_key }` | Pay a bolt11 invoice |
| `GET /api/v1/p2p/transfers/{id}` | Poll a transfer. Every 2–3 s while `processing` / `awaiting_confirmation`, every 15–30 s while `in_flight` |
| `GET /api/v1/p2p/collect/{id}` | Show a collect request (§4.7) to the user it is for |
| `POST /api/v1/p2p/collect/{id}/approve` | The user pays the collect request (fresh login needed). Returns a transfer. |
| `POST /api/v1/p2p/collect/{id}/decline` | The user declines it |

### 5.4 Rules DCA BE's UI must handle

- **Fresh login for spends.** `/send` and `/invoice/pay` need a credential issued in the last **10 minutes**. If it's older the bot answers `401 unauthorized`. Mini App: ask the user to reopen it. Web app: ask them to tap **Log In with Telegram** again. Then retry with the **same** `idempotency_key`. Check `auth_date` (Mini App) or `iat` (web) to warn before they tap Send.
- **`idempotency_key`:** a new UUID per payment, reused on every retry of that payment. `failed`, `cancelled` and `expired` are final for their key.
- **New users:** a user who never pressed Start has no wallet. Call `POST /api/v1/init` (it's idempotent). If it returns `can_message: false`, the bot can't message them, so Confirm buttons and collect prompts won't reach them. In the Mini App, call `Telegram.WebApp.requestWriteAccess()` before `init`; in the web app, ask for message permission at Log In with Telegram.
- **Confirmation:** past the daily no-confirmation allowance (50k sats by default), the bot asks the user to Confirm in the Telegram chat, and the transfer shows `awaiting_confirmation` until then.
- **Error codes:** `401 unauthorized` (user credential), `401 bad_service_signature` (DCA BE's P2P signing: a server bug), `403 forbidden` (IP), `429 rate_limited`.

---

## 6. Callback: bot → DCA BE

When sats arrive in the wallet of `dca.wallet_username` (an internal send or an incoming Lightning payment), the bot calls:

```
POST {dca.api_url}/transaction/reset-retry-counts
Content-Type: application/json
(empty body)
```

- Timeout 10 s, fire-and-forget: the bot doesn't retry and ignores the response.
- **It is not authenticated today.** Anyone who knows the URL can call it. Make the handler safe to call any number of times: it should only reset retry counters, never move funds. Optionally restrict it to the bot server's IP. A signed callback can be added on the bot side if needed.

---

## 7. Errors from the signing layer

| HTTP | Body | Cause | Fix |
|---|---|---|---|
| `401` | `Invalid signature` | Unknown `X-Wallet-ID`, no public key configured, wrong key, or signing string mismatch | Check wallet ID, key pair, and that the exact sent bytes were hashed |
| `401` | `Invalid timestamp` / `Request expired` | Missing timestamp or clock drift > 300 s | Sync the clock (NTP) |
| `401` | `Invalid nonce` | Missing or wrong format | 16–128 chars `[A-Za-z0-9_-]` |
| `401` | `Request already processed` | Nonce reused | New nonce per request, including retries |
| `403` | `Access denied` | Caller IP not in `api.allowed_networks` / `api.send.allowed_networks` | Ask the operator to add the IP, or clear the list |
| `413` | `Request too large` | Body over 1 MiB | — |

---

## 8. Recommended DCA payout flow

```
1. (optional) POST /userbalance       → enough balance (+2% for invoices)?
2. POST /send  { to, amount, memo: "dca-<orderId>" }
     200 → paid
     202 pending (invoice) → poll GET /send/payment/{payment_hash} until settled | failed
     202 awaiting_approval → poll GET /send/status/{id} until executed | rejected | expired
     400 → order failed, nothing sent (reason in message)
     timeout / 5xx → DON'T resend with a new memo. Resend with the SAME memo:
                     "already completed" means it went through; or check
                     /usertransactions or /send/payment first
```

The memo is your idempotency key: one memo per order, reused on every retry of that order.

### 8.1 Collect flow (user sells bitcoin to DCA)

```
1. POST /collect { to: "<telegram_id>", amount, memo, external_id: "<orderId>" }
     201 → pending; the user gets Pay / Decline in the bot chat
     4xx → nothing created (reason in message)
     timeout / 5xx → resend with the SAME external_id; you get the same request back
2. Poll GET /collect/{id} every 3–5 s:
     pending                         → keep polling (expires after 15 min)
     approved, paid: true            → sats received: settle the order (pay the user LKR)
     approved, transfer processing / awaiting_confirmation / in_flight → keep polling
     approved, transfer failed       → order failed, nothing received
     declined | cancelled | expired  → order closed, nothing received
3. (optional) POST /collect/{id}/cancel to withdraw a request the user hasn't answered
```

Only pay the user out (LKR) after `paid: true`.

---

## 9. Code changes and migration checklist

**Bot:** no further code changes. Only config: the two public keys, `hmac_secret` removed, `can_collect: true` on the DCA wallet, and `api.p2p` enabled (§2.2).

**DCA BE:**

| Area | Change |
|---|---|
| Payout client | Replace HMAC signing with the Ed25519 send-key client (§3.3) |
| Payout handling | Handle `202 awaiting_approval` and `202 pending` (§4.1), and poll §4.2 / §4.3 |
| Collect (new) | Create requests for sell orders, poll until `paid` or closed, settle only on `paid: true` (§4.7, §8.1) |
| Collect in the Mini App (new) | Screen that shows a request and relays approve / decline (§5.3) |
| P2P relay (new) | Routes for the Mini App and web app that forward the user's credential and sign with the P2P key (§5.2), for each P2P endpoint (§5.3) |
| Web app login | Log In with Telegram (OIDC, scopes `openid profile`, client = the bot); keep the `id_token` in the server session (§5.1) |
| P2P UI | Re-login prompt when the credential is over 10 min old for spends, `idempotency_key` per payment, the confirmation state, polling (§5.4) |
| Callback | Make `reset-retry-counts` safe to call repeatedly (§6) |
| Secrets | `BDB_SEND_PRIVATE_KEY_PEM` and `BDB_P2P_PRIVATE_KEY_PEM` (§2.1) |

**Checklist:**

- [ ] DCA BE generates the send and P2P key pairs (separate for dev and prod) and stores the private keys as secrets.
- [ ] Public keys sent to the bot operator; the operator sets the wallet's `ed25519_public_key` and **removes** its `hmac_secret`, and sets `api.p2p.service_public_key`.
- [ ] DCA BE replaces `X-HMAC-Signature` with `X-Wallet-ID`, `X-Nonce` and `X-Signature` (§3).
- [ ] P2P relay routes sign with the P2P key and forward exactly one user credential (§5).
- [ ] New nonce and timestamp on every request and every retry.
- [ ] Query strings are now signed (the old HMAC only signed path + body).
- [ ] HTTP timeout ≥ 95 s for `/send`.
- [ ] `202` handling for both approval and pending invoice payments (§4).
- [ ] `reset-retry-counts` handler is safe to call repeatedly (§6).
- [ ] Collect: `can_collect: true` set on the DCA wallet; sell orders use the order ID as `external_id` and settle only on `paid: true` (§8.1).
- [ ] If IP allowlists are used, DCA BE has a static egress IP and it's listed.
- [ ] Tested end-to-end on the dev bot before the production switch. Deploy the bot change and the DCA BE change **together**: after the bot deploys, HMAC requests get `401`.

---

## 10. Not for DCA BE

These bot APIs exist but use other credentials. Listed so nobody wires DCA BE to them by mistake:

| API | Auth | Used by |
|---|---|---|
| `/api/v1/analytics/*` | Ed25519 with `X-Key-ID`, signing line `BDB-ANALYTICS-ED25519-V1` | Data team (`docs/send-api-ed25519.md` §6) |
| `/api/v1/balance`, `/payinvoice`, `/createinvoice`, … | A user's own wallet key | End-user wallet clients |
