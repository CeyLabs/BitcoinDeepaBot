// Package tma verifies Telegram Mini App launch data (initData).
//
// Telegram signs initData with a key derived from the bot token, so a valid
// signature proves the data came from Telegram for this bot, and the user in
// it is the person who opened the Mini App. This is the only thing the P2P API
// trusts to name the sender: a backend that relays the request cannot.
//
// See https://core.telegram.org/bots/webapps#validating-data-received-via-the-mini-app
package tma

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ErrInvalid is wrapped by every verification failure.
var ErrInvalid = errors.New("invalid Telegram init data")

// maxInitDataLen bounds what an unauthenticated caller can make us parse.
const maxInitDataLen = 8 << 10

// User is the Telegram user from verified init data.
type User struct {
	ID           int64  `json:"id"`
	FirstName    string `json:"first_name"`
	LastName     string `json:"last_name"`
	Username     string `json:"username"`
	LanguageCode string `json:"language_code"`
	IsBot        bool   `json:"is_bot"`
}

// Verify checks the init data signature with the bot token and returns the
// user. Data older than maxAge, or signed more than a minute in the future,
// is rejected, which limits how long a leaked init data string is useful.
func Verify(initData, botToken string, maxAge time.Duration, now time.Time) (User, error) {
	if botToken == "" {
		return User{}, fmt.Errorf("%w: bot token not configured", ErrInvalid)
	}
	if initData == "" || len(initData) > maxInitDataLen {
		return User{}, fmt.Errorf("%w: missing or oversized", ErrInvalid)
	}
	values, err := url.ParseQuery(initData)
	if err != nil {
		return User{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	hash := values.Get("hash")
	if hash == "" {
		return User{}, fmt.Errorf("%w: no hash", ErrInvalid)
	}
	got, err := hex.DecodeString(hash)
	if err != nil {
		return User{}, fmt.Errorf("%w: hash is not hex", ErrInvalid)
	}
	if !hmac.Equal(got, sign(values, botToken)) {
		return User{}, fmt.Errorf("%w: signature mismatch", ErrInvalid)
	}

	authDate, err := strconv.ParseInt(values.Get("auth_date"), 10, 64)
	if err != nil || authDate <= 0 {
		return User{}, fmt.Errorf("%w: bad auth_date", ErrInvalid)
	}
	signedAt := time.Unix(authDate, 0)
	if now.Sub(signedAt) > maxAge {
		return User{}, fmt.Errorf("%w: expired", ErrInvalid)
	}
	if signedAt.Sub(now) > time.Minute {
		return User{}, fmt.Errorf("%w: auth_date in the future", ErrInvalid)
	}

	var u User
	if err := json.Unmarshal([]byte(values.Get("user")), &u); err != nil || u.ID <= 0 {
		return User{}, fmt.Errorf("%w: no user", ErrInvalid)
	}
	if u.IsBot {
		return User{}, fmt.Errorf("%w: bots cannot use the Mini App API", ErrInvalid)
	}
	return u, nil
}

// sign computes the init data hash: HMAC-SHA256 over the sorted "key=value"
// lines of every field except hash, keyed with HMAC-SHA256("WebAppData", token).
func sign(values url.Values, botToken string) []byte {
	keys := make([]string, 0, len(values))
	for k := range values {
		if k != "hash" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	lines := make([]string, 0, len(keys))
	for _, k := range keys {
		lines = append(lines, k+"="+values.Get(k))
	}
	secret := hmac.New(sha256.New, []byte("WebAppData"))
	secret.Write([]byte(botToken))
	mac := hmac.New(sha256.New, secret.Sum(nil))
	mac.Write([]byte(strings.Join(lines, "\n")))
	return mac.Sum(nil)
}

// Sign builds signed init data for user, the way Telegram does. It exists for
// tests and local tooling; production init data always comes from Telegram.
func Sign(user User, botToken string, authDate time.Time, extra url.Values) string {
	values := url.Values{}
	for k, v := range extra {
		values[k] = v
	}
	u, _ := json.Marshal(user)
	values.Set("user", string(u))
	values.Set("auth_date", strconv.FormatInt(authDate.Unix(), 10))
	values.Set("hash", hex.EncodeToString(sign(values, botToken)))
	return values.Encode()
}
