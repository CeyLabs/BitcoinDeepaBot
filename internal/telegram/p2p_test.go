package telegram

import (
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/LightningTipBot/LightningTipBot/internal"
	"github.com/LightningTipBot/LightningTipBot/internal/lnbits"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/zpay32"
	tb "gopkg.in/lightningtipbot/telebot.v3"
)

func withP2PConfig(t *testing.T) {
	t.Helper()
	saved := internal.Configuration.API.P2P
	t.Cleanup(func() { internal.Configuration.API.P2P = saved })
	internal.Configuration.API.P2P = internal.APIP2PConfiguration{
		Enabled: true, MaxAmount: 100000, DailyLimit: 500000, ConfirmThreshold: 50000,
		RateLimitPerMinute: 10, InitDataMaxAge: 3600, MaxMemoLength: 20,
	}
}

func TestValidateP2PCommon(t *testing.T) {
	withP2PConfig(t)
	key := "0b9a1c4e-uuid"
	if e := validateP2PCommon(100000, "lunch", key); e != nil {
		t.Fatalf("valid request rejected: %v", e)
	}
	bad := map[string]struct {
		amount int64
		memo   string
		key    string
		code   string
	}{
		"zero":         {0, "", key, "invalid_amount"},
		"negative":     {-5, "", key, "invalid_amount"},
		"over max":     {100001, "", key, "amount_too_large"},
		"long memo":    {10, strings.Repeat("ශ", 21), key, "memo_too_long"},
		"short key":    {10, "", "abc", "invalid_idempotency_key"},
		"missing key":  {10, "", "", "invalid_idempotency_key"},
		"oversize key": {10, "", strings.Repeat("k", 129), "invalid_idempotency_key"},
	}
	for name, c := range bad {
		t.Run(name, func(t *testing.T) {
			e := validateP2PCommon(c.amount, c.memo, c.key)
			if e == nil || e.Code != c.code {
				t.Fatalf("got %v, want code %s", e, c.code)
			}
		})
	}
	// A memo exactly at the limit, counted in characters not bytes.
	if e := validateP2PCommon(10, strings.Repeat("ශ", 20), key); e != nil {
		t.Fatalf("20-character Sinhala memo rejected: %v", e)
	}
}

func TestP2PEligible(t *testing.T) {
	ok := &lnbits.User{Initialized: true, Telegram: &tb.User{ID: 1}, Wallet: &lnbits.Wallet{Adminkey: "abc"}}
	if why := p2pEligible(ok); why != "" {
		t.Fatalf("eligible user rejected: %s", why)
	}
	for name, u := range map[string]*lnbits.User{
		"nil":             nil,
		"no wallet":       {Initialized: true, Telegram: &tb.User{ID: 1}},
		"never started":   {Initialized: false, Telegram: &tb.User{ID: 1}, Wallet: &lnbits.Wallet{}},
		"banned flag":     {Initialized: true, Banned: true, Telegram: &tb.User{ID: 1}, Wallet: &lnbits.Wallet{}},
		"banned by admin": {Initialized: true, Telegram: &tb.User{ID: 1}, Wallet: &lnbits.Wallet{Adminkey: "banned_abc"}},
	} {
		if p2pEligible(u) == "" {
			t.Errorf("%s: allowed", name)
		}
	}
}

func TestDisplayName(t *testing.T) {
	for want, u := range map[string]*tb.User{
		"Nisal Perera": {FirstName: "Nisal", LastName: "Perera"},
		"Nisal":        {FirstName: "Nisal"},
		"@bob":         {Username: "bob"},
	} {
		if got := DisplayName(u); got != want {
			t.Errorf("DisplayName = %q, want %q", got, want)
		}
	}
}

func TestP2PDailyKeyIsUTCDay(t *testing.T) {
	colombo := time.FixedZone("LK", 5*3600+1800)
	late := time.Date(2026, 11, 19, 23, 30, 0, 0, colombo) // 18:00 UTC same day
	early := time.Date(2026, 11, 20, 4, 0, 0, 0, colombo)  // 22:30 UTC on the 19th
	if p2pDailyKey(7, late) != p2pDailyKey(7, early) {
		t.Fatal("same UTC day produced different keys")
	}
	if p2pDailyKey(7, late) == p2pDailyKey(8, late) {
		t.Fatal("different senders share a key")
	}
}

func testInvoice(t *testing.T, msat int64, created time.Time, expiry time.Duration) string {
	t.Helper()
	key, _ := btcec.NewPrivateKey()
	var hash [32]byte
	rand.Read(hash[:])
	opts := []func(*zpay32.Invoice){zpay32.DescriptionHash(sha256.Sum256([]byte("x"))), zpay32.Expiry(expiry)}
	if msat > 0 {
		opts = append(opts, zpay32.Amount(lnwire.MilliSatoshi(msat)))
	}
	inv, err := zpay32.NewInvoice(&chaincfg.MainNetParams, hash, created, opts...)
	if err != nil {
		t.Fatal(err)
	}
	pr, err := inv.Encode(zpay32.MessageSigner{SignCompact: func(m []byte) ([]byte, error) {
		return ecdsa.SignCompact(key, chainhash.HashB(m), true)
	}})
	if err != nil {
		t.Fatal(err)
	}
	return pr
}

func TestP2PDecodeInvoice(t *testing.T) {
	now := time.Now()
	good := testInvoice(t, 21_000_000, now, time.Hour)
	for _, in := range []string{good, "lightning:" + good, "  " + strings.ToUpper(good) + " "} {
		dec, err := P2PDecodeInvoice(in)
		if err != nil {
			t.Fatalf("valid invoice rejected: %v", err)
		}
		if dec.Amount != 21000 || !lnbits.ValidPaymentHash(dec.PaymentHash) {
			t.Fatalf("bad decode: %+v", dec)
		}
	}
	for name, c := range map[string]struct {
		pr   string
		code string
	}{
		"garbage":    {"lnbc1garbage", "invalid_invoice"},
		"amountless": {testInvoice(t, 0, now, time.Hour), "amountless_invoice"},
		"expired":    {testInvoice(t, 1000, now.Add(-2*time.Hour), time.Hour), "invoice_expired"},
	} {
		_, err := P2PDecodeInvoice(c.pr)
		var pe *P2PError
		if !errors.As(err, &pe) || pe.Code != c.code {
			t.Errorf("%s: got %v, want %s", name, err, c.code)
		}
	}
}

func TestP2PIDs(t *testing.T) {
	a, b := newP2PID(), newP2PID()
	if a == b || !strings.HasPrefix(a, "p2p-") || len(a) != 28 {
		t.Fatalf("bad ids %q %q", a, b)
	}
}
