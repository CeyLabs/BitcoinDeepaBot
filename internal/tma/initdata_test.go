package tma

import (
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"
)

const token = "1234567890:TEST-bot-token"

func TestVerify(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	alice := User{ID: 111, FirstName: "Alice", Username: "alice", LanguageCode: "en"}
	extra := url.Values{"query_id": {"AAH"}, "signature": {"sig"}}
	valid := Sign(alice, token, now.Add(-5*time.Minute), extra)

	u, err := Verify(valid, token, time.Hour, now)
	if err != nil {
		t.Fatalf("valid init data rejected: %v", err)
	}
	if u.ID != 111 || u.Username != "alice" || u.FirstName != "Alice" {
		t.Fatalf("wrong user: %+v", u)
	}

	tamper := func(field, value string) string {
		v, _ := url.ParseQuery(valid)
		v.Set(field, value)
		return v.Encode()
	}
	bad := map[string]string{
		"empty":                      "",
		"other bot's token":          Sign(alice, "999:OTHER", now, nil),
		"user swapped after signing": tamper("user", `{"id":222,"first_name":"Mallory"}`),
		"auth_date changed":          tamper("auth_date", "1800000000"),
		"extra field changed":        tamper("query_id", "XYZ"),
		"hash removed":               strings.Replace(valid, "hash=", "nohash=", 1),
		"hash not hex":               tamper("hash", "zz"),
		"expired":                    Sign(alice, token, now.Add(-2*time.Hour), nil),
		"from the future":            Sign(alice, token, now.Add(10*time.Minute), nil),
		"no user":                    signRaw(url.Values{"auth_date": {"1800000000"}}),
		"bot user":                   Sign(User{ID: 5, IsBot: true}, token, now, nil),
		"oversized":                  strings.Repeat("a", 9000),
	}
	for name, data := range bad {
		t.Run(name, func(t *testing.T) {
			if _, err := Verify(data, token, time.Hour, now); !errors.Is(err, ErrInvalid) {
				t.Fatalf("accepted (err=%v)", err)
			}
		})
	}
	if _, err := Verify(valid, "", time.Hour, now); !errors.Is(err, ErrInvalid) {
		t.Fatal("accepted with no bot token configured")
	}
}

func signRaw(v url.Values) string {
	v.Set("hash", hexOf(sign(v, token)))
	return v.Encode()
}

func hexOf(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, c := range b {
		out[i*2], out[i*2+1] = digits[c>>4], digits[c&15]
	}
	return string(out)
}

// Known-answer test: the data-check-string and key derivation follow
// Telegram's documented algorithm, so an independent computation must match.
func TestSignMatchesDocumentedAlgorithm(t *testing.T) {
	v := url.Values{"auth_date": {"1700000000"}, "query_id": {"Q"}, "user": {`{"id":1}`}}
	got := hexOf(sign(v, token))
	// data_check_string = "auth_date=1700000000\nquery_id=Q\nuser={\"id\":1}"
	want := hexOf(hmacSHA256(hmacSHA256([]byte("WebAppData"), []byte(token)), []byte("auth_date=1700000000\nquery_id=Q\nuser={\"id\":1}")))
	if got != want {
		t.Fatalf("sign = %s, want %s", got, want)
	}
}

func hmacSHA256(key, msg []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(msg)
	return m.Sum(nil)
}
