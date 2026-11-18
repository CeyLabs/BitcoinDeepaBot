package lnbits

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/zpay32"
)

const testMetadata = `[["text/plain","Donation to Bitcoin Deepa"]]`

// signedInvoice builds a real, signed mainnet invoice.
func signedInvoice(t *testing.T, amountMsat int64, metadata string, created time.Time, expiry time.Duration) string {
	t.Helper()
	key, err := btcec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	var hash [32]byte
	if _, err := rand.Read(hash[:]); err != nil {
		t.Fatal(err)
	}
	inv, err := zpay32.NewInvoice(&chaincfg.MainNetParams, hash, created,
		zpay32.Amount(lnwire.MilliSatoshi(amountMsat)),
		zpay32.DescriptionHash(sha256.Sum256([]byte(metadata))),
		zpay32.Expiry(expiry))
	if err != nil {
		t.Fatal(err)
	}
	pr, err := inv.Encode(zpay32.MessageSigner{SignCompact: func(msg []byte) ([]byte, error) {
		return ecdsa.SignCompact(key, chainhash.HashB(msg), true)
	}})
	if err != nil {
		t.Fatal(err)
	}
	return pr
}

type fakeLNURLServer struct {
	params      map[string]interface{}
	callback    func(amount string) interface{}
	gotAmount   string
	gotComment  string
	callbackURL string
}

func startLNURLServer(t *testing.T, f *fakeLNURLServer) string {
	t.Helper()
	mux := http.NewServeMux()
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	mux.HandleFunc("/.well-known/lnurlp/donate", func(w http.ResponseWriter, r *http.Request) {
		p := map[string]interface{}{
			"tag": "payRequest", "callback": srv.URL + "/cb", "metadata": testMetadata,
			"minSendable": 1000, "maxSendable": 100000000, "commentAllowed": 10,
		}
		for k, v := range f.params {
			p[k] = v
		}
		json.NewEncoder(w).Encode(p)
	})
	mux.HandleFunc("/cb", func(w http.ResponseWriter, r *http.Request) {
		f.gotAmount, f.gotComment = r.URL.Query().Get("amount"), r.URL.Query().Get("comment")
		json.NewEncoder(w).Encode(f.callback(f.gotAmount))
	})
	saved := lnurlHTTP
	c := srv.Client() // trusts the test certificate
	c.CheckRedirect = httpsOnlyRedirects
	lnurlHTTP = c
	t.Cleanup(func() { lnurlHTTP = saved })
	f.callbackURL = srv.URL + "/cb"
	return srv.URL + "/.well-known/lnurlp/donate"
}

func TestFetchLNURLPayInvoice(t *testing.T) {
	const amount = int64(21000)
	now := time.Now()

	t.Run("valid invoice is returned", func(t *testing.T) {
		f := &fakeLNURLServer{}
		want := signedInvoice(t, amount, testMetadata, now, time.Hour)
		f.callback = func(string) interface{} { return map[string]string{"pr": want} }
		endpoint := startLNURLServer(t, f)
		got, err := FetchLNURLPayInvoice(endpoint, amount, "from @alice bot @BitcoinDeepaBot")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != want {
			t.Fatal("returned a different invoice")
		}
		if f.gotAmount != "21000" {
			t.Fatalf("callback got amount %q, want 21000", f.gotAmount)
		}
		if f.gotComment != "from @alic" {
			t.Fatalf("comment not truncated to commentAllowed: %q", f.gotComment)
		}
	})

	rejects := map[string]struct {
		params   map[string]interface{}
		callback func(t *testing.T) interface{}
	}{
		// The case this check exists for: a dishonest endpoint asks for more.
		"invoice for a larger amount": {callback: func(t *testing.T) interface{} {
			return map[string]string{"pr": signedInvoice(t, amount*100, testMetadata, now, time.Hour)}
		}},
		"invoice for a smaller amount": {callback: func(t *testing.T) interface{} {
			return map[string]string{"pr": signedInvoice(t, amount-1000, testMetadata, now, time.Hour)}
		}},
		"expired invoice": {callback: func(t *testing.T) interface{} {
			return map[string]string{"pr": signedInvoice(t, amount, testMetadata, now.Add(-2*time.Hour), time.Hour)}
		}},
		"invoice not committed to metadata": {callback: func(t *testing.T) interface{} {
			return map[string]string{"pr": signedInvoice(t, amount, `[["text/plain","something else"]]`, now, time.Hour)}
		}},
		"garbage invoice":      {callback: func(*testing.T) interface{} { return map[string]string{"pr": "lnbc1garbage"} }},
		"callback error":       {callback: func(*testing.T) interface{} { return map[string]string{"status": "ERROR", "reason": "nope"} }},
		"plain http callback":  {params: map[string]interface{}{"callback": "http://example.com/cb"}},
		"not a pay request":    {params: map[string]interface{}{"tag": "withdrawRequest"}},
		"amount below minimum": {params: map[string]interface{}{"minSendable": amount + 1}},
		"amount above maximum": {params: map[string]interface{}{"maxSendable": amount - 1}},
		"endpoint error":       {params: map[string]interface{}{"status": "ERROR", "reason": "disabled"}},
	}
	for name, tc := range rejects {
		t.Run(name, func(t *testing.T) {
			f := &fakeLNURLServer{params: tc.params}
			f.callback = func(string) interface{} {
				if tc.callback == nil {
					return map[string]string{"pr": signedInvoice(t, amount, testMetadata, now, time.Hour)}
				}
				return tc.callback(t)
			}
			endpoint := startLNURLServer(t, f)
			if pr, err := FetchLNURLPayInvoice(endpoint, amount, ""); err == nil {
				t.Fatalf("accepted %s: %s", name, pr)
			}
		})
	}

	t.Run("non-positive amount", func(t *testing.T) {
		if _, err := FetchLNURLPayInvoice("https://example.com", 0, ""); err == nil || !strings.Contains(err.Error(), "positive") {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("comment is cut on a character boundary", func(t *testing.T) {
		f := &fakeLNURLServer{}
		f.callback = func(string) interface{} {
			return map[string]string{"pr": signedInvoice(t, amount, testMetadata, now, time.Hour)}
		}
		endpoint := startLNURLServer(t, f)
		// commentAllowed is 10; "ශ්‍රී" style multi-byte names must not be split mid-rune.
		if _, err := FetchLNURLPayInvoice(endpoint, amount, "from ශ්‍රී ලංකා bot"); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !utf8.ValidString(f.gotComment) || utf8.RuneCountInString(f.gotComment) != 10 {
			t.Fatalf("comment %q: valid=%v runes=%d, want valid UTF-8 of 10 runes", f.gotComment, utf8.ValidString(f.gotComment), utf8.RuneCountInString(f.gotComment))
		}
	})
}

func TestLNURLPayRefusesRedirectToHTTP(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://example.invalid/.well-known/lnurlp/donate", http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	saved := lnurlHTTP
	c := srv.Client()
	c.CheckRedirect = httpsOnlyRedirects
	lnurlHTTP = c
	t.Cleanup(func() { lnurlHTTP = saved })

	_, err := FetchLNURLPayInvoice(srv.URL+"/.well-known/lnurlp/donate", 21000, "")
	if err == nil || !strings.Contains(err.Error(), "non-https") {
		t.Fatalf("want redirect refusal, got %v", err)
	}
	if lnurlHTTP.CheckRedirect == nil {
		t.Fatal("production client must refuse non-https redirects")
	}
}
