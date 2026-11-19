# Send API — Ed25519 request signing

Every wallet API request must be signed with Ed25519. This covers `/api/v1/send`, `/userbalance`, `/usertransactions`, `/referral/lookup`, `/send/status/...` and `/send/payment/...`. HMAC signing is no longer accepted.

**Why Ed25519:** you keep the **private key**, and the bot stores only your **public key**. A leak of the bot's config cannot be used to forge requests.

## 1. Create a key pair (caller side)

```bash
openssl genpkey -algorithm ed25519 -out send-ed25519.pem                         # private key: keep secret
openssl pkey -in send-ed25519.pem -pubout -outform DER | tail -c 32 | base64     # public key
```

Send the base64 public key to the Bitcoin Deepa operator. Never share the `.pem` file.

## 2. Operator config

```yaml
api:
  send:
    whitelisted_wallets:
      CeycubeBank:
        username: "CeycubeBank"
        ed25519_public_key: "<base64 public key>"
```

A wallet with no valid `ed25519_public_key`, or one that still has `hmac_secret`, stops the send API from starting, and the error is logged.

## 3. Headers

| Header        | Value |
|---------------|-------|
| `X-Wallet-ID` | Your wallet ID (the key under `whitelisted_wallets`) |
| `X-Timestamp` | Unix time in seconds (must be within ±300 s of server time) |
| `X-Nonce`     | 16–128 random chars `[A-Za-z0-9_-]`, new for every request |
| `X-Signature` | Base64 Ed25519 signature of the signing string |

## 4. Signing string

Seven lines joined with `\n` (no trailing newline):

```
BDB-SEND-ED25519-V1
<METHOD>             e.g. POST
<PATH>               e.g. /api/v1/send
<QUERY>              raw query without "?", exactly as sent, or empty
<TIMESTAMP>          same as X-Timestamp
<NONCE>              same as X-Nonce
<hex sha256 of the exact body bytes>   (sha256 of empty string for GET)
```

Rules:
- Each nonce is accepted once. A replayed request gets `401 Request already processed`.
- Sign the exact body bytes you send. Re-serializing JSON breaks the signature.
- Method, path, query and body are all signed, so none can be changed in transit.
- Max body size: 1 MiB.

## 5. Examples

**Bash** (OpenSSL 3+). The other API docs use this helper in their examples.

```bash
# bdb.sh: set WALLET_ID, KEY and BASE, source this file, then call
#   bdb_request METHOD PATH[?QUERY] [JSON_BODY]
WALLET_ID="CeycubeBank"
KEY="send-ed25519.pem"
BASE="https://bitcoindeepa.com"

bdb_request() {
  local method="$1" target="$2" body="${3-}"
  local path="${target%%\?*}" query="" ts nonce hash msg sig
  [[ "$target" == *\?* ]] && query="${target#*\?}"
  ts=$(date +%s)
  nonce=$(openssl rand -hex 16)
  hash=$(printf '%s' "$body" | openssl dgst -sha256 -r | cut -d' ' -f1)
  msg=$(mktemp)
  printf 'BDB-SEND-ED25519-V1\n%s\n%s\n%s\n%s\n%s\n%s' "$method" "$path" "$query" "$ts" "$nonce" "$hash" > "$msg"
  sig=$(openssl pkeyutl -sign -rawin -inkey "$KEY" -in "$msg" | base64 | tr -d '\r\n')
  rm -f "$msg"
  curl -sS -X "$method" "$BASE$target" \
    -H "Content-Type: application/json" \
    -H "X-Wallet-ID: $WALLET_ID" -H "X-Timestamp: $ts" \
    -H "X-Nonce: $nonce" -H "X-Signature: $sig" \
    ${body:+--data-binary "$body"}
}

# bdb_request POST /api/v1/send '{"to":"lnbc10u1p3xyz..."}'
# bdb_request GET  "/api/v1/referral/lookup?code=abc"
```

**Node.js**

```js
const crypto = require("crypto");
const fs = require("fs");

const key = crypto.createPrivateKey(fs.readFileSync("send-ed25519.pem"));

function signedHeaders(walletId, method, path, query, body) {
  const ts = Math.floor(Date.now() / 1000).toString();
  const nonce = crypto.randomBytes(16).toString("hex");
  const bodyHash = crypto.createHash("sha256").update(body).digest("hex");
  const msg = ["BDB-SEND-ED25519-V1", method, path, query, ts, nonce, bodyHash].join("\n");
  const sig = crypto.sign(null, Buffer.from(msg), key).toString("base64");
  return { "X-Wallet-ID": walletId, "X-Timestamp": ts, "X-Nonce": nonce, "X-Signature": sig };
}

const body = JSON.stringify({ to: "lnbc10u1p3xyz..." });
const headers = { "Content-Type": "application/json", ...signedHeaders("CeycubeBank", "POST", "/api/v1/send", "", body) };
// send `body` unchanged with these headers
```

**Python** (`pip install cryptography`)

```python
import base64, hashlib, json, os, time, requests
from cryptography.hazmat.primitives.serialization import load_pem_private_key

key = load_pem_private_key(open("send-ed25519.pem", "rb").read(), password=None)

def signed_headers(wallet_id, method, path, query, body: bytes):
    ts = str(int(time.time()))
    nonce = os.urandom(16).hex()
    msg = "\n".join(["BDB-SEND-ED25519-V1", method, path, query, ts, nonce, hashlib.sha256(body).hexdigest()])
    sig = base64.b64encode(key.sign(msg.encode())).decode()
    return {"X-Wallet-ID": wallet_id, "X-Timestamp": ts, "X-Nonce": nonce, "X-Signature": sig}

body = json.dumps({"to": "lnbc10u1p3xyz..."}, separators=(",", ":")).encode()
headers = {"Content-Type": "application/json", **signed_headers("CeycubeBank", "POST", "/api/v1/send", "", body)}
requests.post("https://bitcoindeepa.com/api/v1/send", data=body, headers=headers)
```

## 6. Analytics API

The analytics endpoints (`GET /api/v1/analytics/transactions`, `GET /api/v1/analytics/user/{user_id}/transactions`) use the same scheme, with two differences:

- Send `X-Key-ID` (the key under `api.analytics.api_keys`) instead of `X-Wallet-ID`.
- The first line of the signing string is `BDB-ANALYTICS-ED25519-V1`. A signature made for one API is not valid on the other.

The query string is signed, so the filters can't be changed after signing. GET requests have an empty body, so the last line is the SHA-256 of the empty string.

```yaml
api:
  analytics:
    api_keys:
      data-team:
        name: "Data Team"
        ed25519_public_key: "<base64 public key>"
```

A key with no valid `ed25519_public_key`, or one that still has `hmac_secret`, disables the analytics API at startup, and the error is logged.

Bash, reusing the helper above:

```bash
# in bdb_request: use "BDB-ANALYTICS-ED25519-V1" and -H "X-Key-ID: $KEY_ID"
bdb_request GET "/api/v1/analytics/transactions?start_date=2026-11-01&end_date=2026-11-19"
```

Python:

```python
def analytics_headers(key_id, method, path, query):
    ts = str(int(time.time()))
    nonce = os.urandom(16).hex()
    msg = "\n".join(["BDB-ANALYTICS-ED25519-V1", method, path, query, ts, nonce, hashlib.sha256(b"").hexdigest()])
    sig = base64.b64encode(key.sign(msg.encode())).decode()
    return {"X-Key-ID": key_id, "X-Timestamp": ts, "X-Nonce": nonce, "X-Signature": sig}

q = "start_date=2026-11-01&end_date=2026-11-19"
requests.get(f"https://bitcoindeepa.com/api/v1/analytics/transactions?{q}",
             headers=analytics_headers("data-team", "GET", "/api/v1/analytics/transactions", q))
```

## 7. Errors

| Status | Message | Cause |
|--------|---------|-------|
| 401 | `Invalid signature` | Missing signature, unknown wallet, or signature does not match |
| 401 | `Invalid timestamp` / `Request expired` | Timestamp missing or outside ±300 s |
| 401 | `Invalid nonce` | Nonce missing or wrong format |
| 401 | `Request already processed` | Nonce already used |
| 413 | `Request too large` | Body over 1 MiB |
