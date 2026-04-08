# Boltz Lightning → USDT Swap — Setup Guide

This document explains how to enable and operate the `/swap` command, which lets
users convert Lightning-network satoshis into USDT using Boltz reverse swaps.

---

## Table of Contents

1. [How It Works](#how-it-works)
2. [Prerequisites](#prerequisites)
3. [Configuration](#configuration)
4. [Webhook Setup](#webhook-setup)
5. [Supported Networks](#supported-networks)
6. [Address Validation](#address-validation)
7. [Fee & Limit Information](#fee--limit-information)
8. [Running in Production](#running-in-production)
9. [Testnet / Sandbox](#testnet--sandbox)
10. [Troubleshooting](#troubleshooting)
11. [Security Notes](#security-notes)

---

## How It Works

```
User sends:  /swap 10000 TYourTronAddress
                        │
                        ▼
         Bot validates address via regex
         (auto-detects Tron or EVM network)
                        │
                        ▼
         Bot fetches live fee estimate
         from Boltz GET /v2/pairs
                        │
                        ▼
         Bot shows confirmation keyboard
         ✅ Confirm Swap  /  🚫 Cancel
                        │
                (user confirms)
                        │
                        ▼
         Bot generates random 32-byte preimage
         → SHA-256 hash → preimage hash
                        │
                        ▼
         Bot calls Boltz POST /v2/swap/reverse
         (From: BTC, To: ETH or TRX, preimageHash, callbackURL)
                        │
                        ▼
         Boltz returns Lightning invoice
                        │
                        ▼
         Bot pays invoice via LNbits Wallet.Pay()
                        │
                        ▼
         Boltz broadcasts on-chain USDT tx
                        │
                (webhook callback)
                        │
                        ▼
         Bot notifies user: "✅ Swap complete!"
```

A **polling fallback goroutine** runs every 30 seconds so the swap completes even
if the webhook is not delivered (e.g. after a restart).

---

## Prerequisites

| Requirement | Details |
|---|---|
| **Boltz account** | Register at [boltz.exchange](https://boltz.exchange) to obtain API credentials |
| **LNbits wallet** | Configured and funded; the bot pays Lightning invoices via LNbits |
| **Public webhook URL** | The bot's webhook server must be reachable by Boltz's callback (HTTPS required in production) |
| **Go 1.19+** | Already required by the bot |

---

## Configuration

Add the following block to your `config.yaml` (see `config.yaml.example` for a full template):

```yaml
boltz:
  enabled: true
  api_url: "https://api.boltz.exchange"   # Boltz API base URL
  api_key: "YOUR_BOLTZ_API_KEY"           # API key from your Boltz account
  api_secret: "YOUR_BOLTZ_API_SECRET"     # API secret from your Boltz account
  webhook_path: "/boltz/webhook"          # Path on the bot's webhook server
  min_swap_sat: 10000                     # Minimum swap amount (satoshis)
  max_swap_sat: 10000000                  # Maximum swap amount (satoshis)
```

### Configuration Fields

| Field | Default | Description |
|---|---|---|
| `enabled` | `false` | Master switch; set `true` to enable the `/swap` command |
| `api_url` | `https://api.boltz.exchange` | Boltz v2 REST API base URL |
| `api_key` | _(required)_ | API key provided by Boltz |
| `api_secret` | _(required)_ | API secret used for HMAC request signing |
| `webhook_path` | `/boltz/webhook` | HTTP path on the bot's webhook server where Boltz POSTs status updates |
| `min_swap_sat` | `10000` | Minimum swap amount in satoshis (must be ≥ Boltz minimum) |
| `max_swap_sat` | `10000000` | Maximum swap amount in satoshis (must be ≤ Boltz maximum) |

> **Validation**: The bot panics at startup if `enabled: true` but `api_key` or
> `api_secret` is empty.

---

## Webhook Setup

Boltz POSTs status updates to a URL you supply when creating each swap.
The bot constructs this URL automatically:

```
https://<your-public-host><webhook_path>?id=<localSwapID>&token=<hmac-token>
```

### Requirements

1. **Public HTTPS endpoint** — Boltz requires HTTPS in production.
   Use a reverse proxy (nginx / Caddy / Traefik) in front of the bot's webhook server.

2. **Firewall** — Allow inbound POST requests on the webhook server port from Boltz's IP
   ranges (consult Boltz documentation) or from anywhere if you rely solely on the HMAC token.

3. **Token verification** — Every webhook request carries a `?token=` parameter that is
   `HMAC-SHA256(localSwapID + api_secret)[0:8]` in hex. The bot rejects any request with
   an incorrect token (HTTP 401). Never disable this check.

### Verifying Webhook Delivery

Check the bot logs for lines like:

```
[Boltz webhook] swap local:<id> state:transaction.confirmed
[boltz] swap <boltz-id> complete — USDT sent to <address>
```

If webhooks are not being received, the polling fallback will still complete the swap
within ~30 seconds. Look for:

```
[boltz poller] swap local:<id> state → transaction.confirmed
```

---

## Supported Networks

The destination network is **auto-detected from the address** the user provides.
No manual network selection is needed.

| Network | Address Format | Boltz Chain ID | Pair Key |
|---|---|---|---|
| **Ethereum (ERC-20)** | `0x` + 40 hex chars | `ETH` | `BTC/ETH` |
| **Tron (TRC-20)** | `T` + 33 base58 chars | `TRX` | `BTC/TRX` |

> **Note**: Boltz pair availability depends on your Boltz plan and the pairs returned by
> `GET /v2/pairs`. If a pair is not found in the response, the confirmation message falls
> back to showing no fee estimate. The swap is still created correctly.

---

## Address Validation

The bot validates addresses using strict regex before sending anything to Boltz:

| Network | Regex | Example |
|---|---|---|
| EVM (Ethereum) | `^0x[0-9a-fA-F]{40}$` | `0xAbCd...1234` (42 chars total) |
| Tron | `^T[A-Za-z1-9]{33}$` | `TYour...Addr` (34 chars total) |

**Key rules:**
- EVM: must be `0x`-prefixed, exactly 40 hexadecimal characters (case-insensitive)
- Tron: must start with `T`, followed by exactly 33 base-58 characters
  (`A–Z`, `a–z`, `1–9`; the digit `0` is **not** valid in base-58)
- Leading/trailing whitespace is stripped before validation
- Boltz may still reject an address that passes regex (e.g. invalid checksum); the user
  will be notified with the Boltz error message

---

## Fee & Limit Information

When the user runs `/swap`, the bot calls `GET /v2/pairs` to display a live fee estimate:

```
⚡️ Confirm Swap

🌐 Network: Ethereum (ERC-20)
💸 Send:    10000 sats
🪙 Receive: ~0.0052 USDT
📬 To:      0xYourAddress
💰 Fee:     ~100 sats

Proceed?  ✅ Confirm Swap   🚫 Cancel
```

If the pairs endpoint is unavailable, the confirmation still shows but without the USDT
estimate. The swap proceeds normally.

### Checking Boltz Limits

Run the following to see current limits and fees for your configured network:

```bash
curl https://api.boltz.exchange/v2/pairs | jq '.reverse'
```

Set `min_swap_sat` and `max_swap_sat` in `config.yaml` to values within Boltz's allowed
range for the target pair.

---

## Running in Production

### nginx Reverse Proxy (example)

```nginx
server {
    listen 443 ssl;
    server_name your-bot-domain.example.com;

    # ... SSL config ...

    # Forward Boltz webhook callbacks
    location /boltz/webhook {
        proxy_pass http://127.0.0.1:4000;  # bot's webhook server port
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
    }

    # LNbits webhook (existing)
    location / {
        proxy_pass http://127.0.0.1:4000;
    }
}
```

### Systemd Service (excerpt)

Ensure the bot can reach Boltz and that the webhook port is open:

```ini
[Service]
Environment="CONFIG_FILE=/etc/bitcoindeepabot/config.yaml"
ExecStart=/usr/local/bin/bitcoindeepabot
Restart=on-failure
RestartSec=10
```

### Health Check

The bot logs the following on successful startup when Boltz is enabled:

```
[boltz] Client initialized (url: https://api.boltz.exchange)
[boltz] scanning BuntDB for pending swaps to resume...
[boltz] resumed 0 pending swap(s)
```

---

## Testnet / Sandbox

1. **Boltz testnet** — Boltz provides a testnet instance. Check the Boltz documentation
   for the current testnet API URL and update `api_url` accordingly.

2. **LNbits testnet** — Point `lnbits_url` in `config.yaml` to a testnet LNbits instance
   funded via a Lightning testnet faucet.

3. **Test addresses:**
   - EVM (Sepolia/Goerli): any Ethereum address; use a testnet wallet (MetaMask)
   - Tron (Shasta): any Tron address; use TronLink on Shasta testnet

4. **Verify end-to-end:**
   ```
   /swap 50000 0xYourSepoliaAddress
   ```
   Confirm the swap and wait for the on-chain USDT to appear on Etherscan (Sepolia).

---

## Troubleshooting

| Symptom | Likely Cause | Fix |
|---|---|---|
| `/swap` returns "Swap is currently disabled" | `enabled: false` in config | Set `boltz.enabled: true` |
| "Boltz CreateReverseSwap: ..." error | Invalid API key/secret, or pair not supported | Check credentials; verify pair exists in `GET /v2/pairs` |
| Webhook never fires, swap eventually expires | Bot's webhook URL unreachable by Boltz | Check public URL, TLS cert, and firewall |
| "invalid token" in webhook logs | `api_secret` changed after swap was created | Tokens are derived from the secret at swap creation time; do not rotate secrets while swaps are in flight |
| Swap stays in `invoice.paid` state forever | Boltz on-chain broadcast failed | Check Boltz status page; the polling loop will detect expiry and notify the user |
| "insufficient funds" error | User balance < amount + 1% fee reserve | User needs to top up their balance |

### Useful Log Queries

```bash
# All Boltz-related log lines
journalctl -u bitcoindeepabot | grep '\[boltz\]'

# Webhook activity
journalctl -u bitcoindeepabot | grep '\[Boltz webhook\]'

# Failed swaps
journalctl -u bitcoindeepabot | grep 'swap.*failed\|CreateReverseSwap'
```

---

## Security Notes

- **Preimage confidentiality**: The 32-byte preimage is generated with `crypto/rand` and
  is **never logged**. Only the SHA-256 hash (`preimage_hash`) is persisted in BuntDB.
- **Webhook token**: Every callback URL includes a short HMAC token derived from the swap
  ID and `api_secret`. Requests with wrong tokens are rejected with HTTP 401.
- **Mutex**: `confirmSwapHandler` and `HandleBoltzWebhook` both acquire a per-swap mutex
  to prevent double-processing if a webhook races with the polling goroutine.
- **API secret rotation**: Rotate `api_secret` only when no swaps are pending; existing
  webhook tokens are derived from the old secret and will fail validation after rotation.
- **HTTPS only**: Never expose the webhook server over plain HTTP in production; Boltz
  requires HTTPS for callback URLs.
