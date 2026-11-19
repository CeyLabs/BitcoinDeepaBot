package api

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httputil"
	"strings"
	"sync"
	"time"

	"github.com/LightningTipBot/LightningTipBot/internal/lnbits"
	"github.com/LightningTipBot/LightningTipBot/internal/telegram"
	"gorm.io/gorm"

	log "github.com/sirupsen/logrus"
)

func LoggingMiddleware(prefix string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		log.Tracef("[%s] %s %s", prefix, r.Method, r.URL.Path)
		log.Tracef("[%s]\n%s", prefix, dump(r))
		r.BasicAuth()
		next.ServeHTTP(w, r)
	}
}

type AuthType struct {
	Type    string
	Decoder func(s string) ([]byte, error)
}

var AuthTypeBasic = AuthType{Type: "Basic"}
var AuthTypeBearerBase64 = AuthType{Type: "Bearer", Decoder: base64.StdEncoding.DecodeString}
var AuthTypeNone = AuthType{}

// invoice key or admin key requirement
type AccessKeyType struct {
	Type string
}

var AccessKeyTypeInvoice = AccessKeyType{Type: "invoice"}
var AccessKeyTypeAdmin = AccessKeyType{Type: "admin"}
var AccessKeyTypeNone = AccessKeyType{Type: "none"} // no authorization required

func AuthorizationMiddleware(database *gorm.DB, authType AuthType, accessType AccessKeyType, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if accessType.Type == "none" {
			next.ServeHTTP(w, r)
			return
		}
		auth := r.Header.Get("Authorization")
		// check if the user is banned
		if auth == "" {
			w.WriteHeader(401)
			log.Warn("[api] no auth")
			return
		}
		_, password, ok := parseAuth(authType, auth)
		if !ok {
			w.WriteHeader(401)
			return
		}
		// first we make sure that the password is not already "banned_"
		if strings.Contains(password, "_") || strings.HasPrefix(password, "banned_") {
			w.WriteHeader(401)
			log.Warnf("[api] Banned user %s. Not forwarding request", password)
			return
		}
		// then we check whether the "normal" password provided is in the database (it should be not if the user is banned)

		user := &lnbits.User{}
		var tx *gorm.DB
		if accessType.Type == "admin" {
			tx = database.Where("wallet_adminkey = ? COLLATE NOCASE", password).First(user)
		} else if accessType.Type == "invoice" {
			tx = database.Where("wallet_inkey = ? OR wallet_adminkey = ? COLLATE NOCASE", password, password).First(user)
		} else {
			log.Errorf("[api] route without access type")
			w.WriteHeader(401)
			return
		}
		if tx.Error != nil {
			log.Warnf("[api] could not load access key: %v", tx.Error)
			w.WriteHeader(401)
			return
		}

		log.Debugf("[api] User: %s Endpoint: %s %s %s", telegram.GetUserStr(user.Telegram), r.Method, r.URL.Path, r.URL.RawQuery)
		r = r.WithContext(context.WithValue(r.Context(), "user", user))
		next.ServeHTTP(w, r)
	}
}

// parseAuth parses an HTTP Basic Authentication string.
// "Bearer QWxhZGRpbjpvcGVuIHNlc2FtZQ==" returns ("Aladdin", "open sesame", true).
func parseAuth(authType AuthType, auth string) (username, password string, ok bool) {
	parse := func(prefix string) (username, password string, ok bool) {
		// Case insensitive prefix match. See Issue 22736.
		if len(auth) < len(prefix) || !strings.EqualFold(auth[:len(prefix)], prefix) {
			return
		}
		if authType.Decoder != nil {
			c, err := authType.Decoder(auth[len(prefix):])
			if err != nil {
				return
			}
			cs := string(c)
			s := strings.IndexByte(cs, ':')
			if s < 0 {
				return
			}
			return cs[:s], cs[s+1:], true
		}
		return auth[len(prefix):], auth[len(prefix):], true

	}
	return parse(fmt.Sprintf("%s ", authType.Type))

}

func dump(r *http.Request) string {
	x, err := httputil.DumpRequest(r, true)
	if err != nil {
		return ""
	}
	return string(x)
}

// ctxKey is an unexported context key type, so values stored under it cannot
// collide with keys set by other packages.
type ctxKey string

// requestStartKey carries the time a request entered the handler chain.
const requestStartKey ctxKey = "request_start"

// RequestStart returns when the request entered the handler chain, and whether
// the middleware recorded it. Handlers use it to budget a slow wait against the
// server's WriteTimeout, which starts before the handler is called.
func RequestStart(ctx context.Context) (time.Time, bool) {
	start, ok := ctx.Value(requestStartKey).(time.Time)
	return start, ok
}

// signatureSet remembers signatures and nonces that were already accepted, so each
// signed request can be used only once.
//
// The set lives in process memory, so this assumes the bot runs as a single
// instance: a second instance would not see signatures used on the first and
// would accept a replay. A restart is safe only because the set is empty for at
// most one timestamp window. Move it to shared storage (e.g. Bunt) before
// running more than one instance.
type signatureSet struct {
	mu   sync.Mutex
	seen map[string]time.Time // signature -> when it can be forgotten
}

var usedSignatures = &signatureSet{seen: make(map[string]time.Time)}

// claim records sig and reports whether it was unused. ttl must cover the whole
// timestamp window the signature is valid for; after that the timestamp check
// rejects it anyway.
func (s *signatureSet) claim(sig string, ttl time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	for k, expiry := range s.seen {
		if now.After(expiry) {
			delete(s.seen, k)
		}
	}
	if _, used := s.seen[sig]; used {
		return false
	}
	s.seen[sig] = now.Add(ttl)
	return true
}

