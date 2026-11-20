# Spec: DCA-issued sessions + bot-verified passkeys

**Status:** part 1 built, part 2 planned.

| Part | What | State |
|---|---|---|
| 1 | The Telegram login is checked only at login (`POST /api/v1/init`); every other call names the user with `X-BDB-User` (§4) | **Built** (#104, #107). DCA BE sessions (§3) are DCA BE's job |
| 2 | Passkeys: enrollment (§5) and approval of every payment (§6) | **Planned.** Until then, the P2P key alone can pay from any user's wallet, up to `daily_limit` |

**Replaces:** the 10-minute / 1-hour Telegram login limits (removed in #107), and the bot-chat confirmations (removed in #101).

## 1. Goal

- **Stay logged in** for weeks in the Mini App and web app, like Wise. No new Telegram login every 10 minutes or every hour.
- **Approve every payment with a passkey** (Face ID, fingerprint or device PIN).
- **A hacked DCA backend can't move anyone's sats.** It could read balances, but it can't pay.

## 2. Who does what

| Part | Owns |
|---|---|
| **DCA backend (DCA BE)** | User sessions: Telegram login, cookies, refresh, logout. Relays calls to the bot. |
| **Bot** | Money. Stores passkey public keys and checks every passkey signature. Telegram login is checked only to add a passkey. |
| **Mini App / web app** | Runs the passkey prompt in the browser (`navigator.credentials`). |
| **User's device** | Holds the passkey private key. It never leaves the device. |

The rule: **identity comes from DCA BE, permission to spend comes from the user's passkey, checked by the bot.**

## 3. Sessions (DCA BE)

The bot doesn't store or check these sessions. DCA BE's requirements:

1. **Login:** check the Telegram login (Mini App `initData` or Log In with Telegram ID token), then create a session for that `telegram_id`.
2. **Tokens:**
   - A short access token (about 15 min) plus a refresh token (about 30 days) that rotates on every use.
   - If an old refresh token is used again, end the whole session: it was stolen.
3. **Storage:**
   - Browser: `HttpOnly`, `Secure`, `SameSite=Lax` cookies. Never `localStorage`.
   - Server: store refresh tokens hashed.
4. **Logout and revoke:** per device, plus "log out everywhere".
5. **When DCA BE calls the bot,** it names the user with `X-BDB-User` (§4). It sends no Telegram credential, except when adding a passkey.

## 4. Calling the bot: P2P signature v2

Same URLs, headers and key as for login (`X-Timestamp`, `X-Nonce`, `X-Signature`, DCA BE's P2P key). Only the signed text differs: its first line is `…-V2`, and the **user ID** replaces the credential hash:

```
BDB-P2P-ED25519-V2
<METHOD>
<PATH>                  exactly as sent
<QUERY>
<TIMESTAMP>
<NONCE>
<telegram_id>           same value as the X-BDB-User header
<hex sha256 of the body>
```

- **Header:** `X-BDB-User: <numeric telegram_id>`.
- **Built:** every `/api/v1/p2p/*` call must use V2, payments included. Any user with a bot wallet can be named; there is no login check. A Telegram credential on these calls gets `401`.
- **`/api/v1/init`** is the only call that takes the Telegram credential (signed text `…-V1`). Adding a passkey (§5) will need it too.
- **With passkeys (part 2):** V2 alone will still allow reads and **starting** a payment, but it won't move sats by itself: every spend will also need a passkey (§6).

## 5. Adding a passkey (enrollment)

This is the only step that needs a **fresh Telegram login** checked by the bot. That way DCA BE can't add its own passkey to someone else's account.

```
1. POST /api/v1/passkeys/register/begin          (V1: initData / ID token, at most 10 min old)
   → WebAuthn creation options { challenge, rp, user, ... }
     challenge: 32 random bytes, kept by the bot for 5 min, single use
2. App: navigator.credentials.create(options)    → Face ID / fingerprint
3. POST /api/v1/passkeys/register/finish         (V1, same user) { credential }
   → bot checks challenge, origin, rpId, user present + verified
   → stores { credential_id, public_key, sign_count, aaguid, name, created_at }
   → messages the user in the bot chat: "🔐 New passkey added: <name>. Not you? Remove it with /passkeys."
```

- At most 10 passkeys per user.
- `GET /api/v1/passkeys` lists them (name, created, last used). It works with V2.
- `DELETE /api/v1/passkeys/{id}` needs a passkey signature or a fresh Telegram login.
- In the bot chat, `/passkeys` lists the user's passkeys and lets them remove any.
- **Lost phone:** log in with Telegram again on the new device and add a new passkey. The Telegram account is the root of trust.

## 6. Paying with a passkey

Two steps. The challenge is tied to one transfer on the bot's side, so a signature for one payment can't approve another.

```
1. POST /api/v1/p2p/send | /invoice/pay    (V2)
   → 202 { id, status: "awaiting_passkey", amount, to | invoice,
           passkey: { challenge, allowCredentials, rpId, timeout } }
   Nothing has moved yet. The transfer waits up to 5 min.

2. App shows the review screen, then navigator.credentials.get(options)
   → user approves with Face ID / fingerprint

3. POST /api/v1/p2p/transfers/{id}/authorize   (V2) { assertion }
   → bot checks: challenge belongs to this transfer and is unused,
                 credential belongs to this user, signature valid,
                 origin + rpId allowed, user verified, sign count
   → runs the transfer → normal transfer response
```

- **New transfer status:** `awaiting_passkey`. After 5 minutes it becomes `expired`, and the daily limit is given back.
- **Failures:** a wrong or missing passkey returns `401 passkey_failed`. After 5 failures the transfer is cancelled.
- **No passkey yet:** step 1 returns `403 passkey_required`, and the app sends the user to §5.
- **Limits still apply:** `max_amount`, `daily_limit`, rate limits, idempotency and the invoice lock.

## 7. Bot config

```yaml
api:
  p2p:
    passkeys:
      enabled: true
      required_for_spends: true          # false only during rollout
      rp_id: "app.bitcoindeepa.com"      # the web app's domain
      rp_name: "Bitcoin Deepa"
      allowed_origins: ["https://app.bitcoindeepa.com"]
      challenge_ttl: 300                 # seconds
```

Library: `github.com/go-webauthn/webauthn`. Storage: buntdb, keyed by `telegram_id`.

## 8. What each party can do if compromised

| Compromised | Can | Can't |
|---|---|---|
| DCA BE (server or P2P key) | Read any user's balance and history; start payments | **Move sats** (no passkey); add a passkey (needs a fresh Telegram login) |
| DCA BE **and** a fresh Telegram login of one user | Add a passkey for that user, who gets a chat alert | Touch other users |
| A user's session cookie | Act as that user for reads | Pay without the user's device |
| The bot's database | Read passkey public keys | Sign anything: public keys can't sign |
| The user's phone, unlocked | Pay as that user, up to `daily_limit` | Exceed the limits |

## 9. Open questions

1. **Passkeys inside Telegram's Mini App webview.** WebAuthn support in Telegram's in-app browser differs between iOS, Android and desktop, and may not work everywhere. Options:
   - (a) open passkey steps in the system browser on the web app's domain;
   - (b) use Telegram's `BiometricManager` in the Mini App. It unlocks a secret stored on the device, but the bot **can't check it**, so it is weaker;
   - (c) web app only at first.

   This needs a device test before building.
2. **`rp_id` domain:** passkeys only work on the domain they were created for, so this choice is permanent. Pick the final web domain before launch.
3. **A new passkey while an older one exists:** should large payments wait, for example 24 hours, after a passkey is added? This is optional extra protection against a stolen Telegram login.
4. **Collect:** charges at once with the wallet's key, with no user approval, so passkeys don't cover it.

## 10. Rollout

1. ✅ **Bot:** V2 signing and login-only Telegram checks (#104, #107).
2. **DCA BE:** login through `/api/v1/init`, its own sessions, and `X-BDB-User` on every other call. This is needed now, because the old calls get `401`.
3. **Bot:** passkey enrollment, `awaiting_passkey` and authorize, with `required_for_spends: false`.
4. **DCA BE:** enrollment screen and a passkey step before each payment.
5. Test on dev with real iOS, Android and desktop devices, inside and outside Telegram.
6. Set `required_for_spends: true`. Spends without a passkey stop.
