package internal

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/LightningTipBot/LightningTipBot/internal/netutil"
	"github.com/jinzhu/configor"
	log "github.com/sirupsen/logrus"
)

var Configuration = struct {
	Bot        BotConfiguration        `yaml:"bot"`
	Telegram   TelegramConfiguration   `yaml:"telegram"`
	Database   DatabaseConfiguration   `yaml:"database"`
	Lnbits     LnbitsConfiguration     `yaml:"lnbits"`
	Generate   GenerateConfiguration   `yaml:"generate"`
	Nostr      NostrConfiguration      `yaml:"nostr"`
	API        APIConfiguration        `yaml:"api"`
	ThirdParty ThirdPartyConfiguration `yaml:"third_party"`
	DCA        DCAConfiguration        `yaml:"dca"`
}{}

type DCAConfiguration struct {
	WalletUsername string `yaml:"wallet_username"`
	ApiUrl         string `yaml:"api_url"`
}

type NostrConfiguration struct {
	PrivateKey string `yaml:"private_key"`
}

type ThirdPartyConfiguration struct {
	CoinMarketCapAPIKey string `yaml:"coinmarketcap_api_key"`
}

type GenerateConfiguration struct {
	OpenAiBearerToken string `yaml:"open_ai_bearer_token"`
	DalleKey          string `yaml:"dalle_key"`
	DallePrice        int64  `yaml:"dalle_price"`
	Worker            int    `yaml:"worker"`
}

type SocksConfiguration struct {
	Host     string `yaml:"host"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

type BotConfiguration struct {
	SocksProxy     *SocksConfiguration `yaml:"socks_proxy,omitempty"`
	TorProxy       *SocksConfiguration `yaml:"tor_proxy,omitempty"`
	LNURLServer    string              `yaml:"lnurl_server"`
	LNURLServerUrl *url.URL            `yaml:"-"`
	LNURLHostName  string              `yaml:"lnurl_public_host_name"`
	LNURLHostUrl   *url.URL            `yaml:"-"`
	LNURLSendImage bool                `yaml:"lnurl_image"`
	AdminAPIHost   string              `yaml:"admin_api_host"`
	// DonateAddress is where /donate (and sends or tips to the bot itself)
	// pay: a lightning address (name@domain) or an https LNURL-pay URL.
	// Empty disables /donate.
	DonateAddress  string `yaml:"donate_address"`
	DonateEndpoint string `yaml:"-"`
	// TrustedProxies are the reverse proxies whose X-Forwarded-For and
	// X-Real-IP headers are believed. Empty means loopback only; ["none"]
	// trusts no proxy. Headers from anyone else are ignored.
	TrustedProxies   []string     `yaml:"trusted_proxies"`
	TrustedProxyNets []*net.IPNet `yaml:"-"`
}

// DonationEndpoint turns a lightning address (name@domain) or an https
// LNURL-pay URL into the LNURL-pay endpoint to query. Plain http is refused:
// the endpoint decides which invoice gets paid.
func DonationEndpoint(address string) (string, error) {
	address = strings.TrimSpace(address)
	if address == "" {
		return "", nil
	}
	if strings.Contains(address, "://") {
		u, err := url.Parse(address)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return "", fmt.Errorf("donate_address %q must be an https URL or a lightning address", address)
		}
		return u.String(), nil
	}
	name, domain, ok := strings.Cut(address, "@")
	if !ok || name == "" || domain == "" || strings.ContainsAny(name+domain, "/?#@ ") {
		return "", fmt.Errorf("donate_address %q must be an https URL or a lightning address (name@domain)", address)
	}
	return (&url.URL{Scheme: "https", Host: strings.ToLower(domain), Path: "/.well-known/lnurlp/" + name}).String(), nil
}

// configPath picks the configuration file: --config <path>, --config=<path>,
// or $BOT_CONFIG, falling back to ./config.yaml. It reads os.Args directly
// because the configuration is loaded in a package init, before any flag
// parsing runs.
func configPath(args []string, env string) string {
	for i, a := range args {
		for _, name := range []string{"--config", "-config"} {
			if a == name && i+1 < len(args) {
				return args[i+1]
			}
			if v, ok := strings.CutPrefix(a, name+"="); ok && v != "" {
				return v
			}
		}
	}
	if env != "" {
		return env
	}
	return "config.yaml"
}

type TelegramConfiguration struct {
	MessageDisposeDuration int64  `yaml:"message_dispose_duration"`
	ApiKey                 string `yaml:"api_key"`
	LogGroupId             int64  `yaml:"log_group_id"`
	ErrorThreadId          int64  `yaml:"error_thread_id"`
}
type DatabaseConfiguration struct {
	DbPath           string `yaml:"db_path"`
	ShopBuntDbPath   string `yaml:"shop_buntdb_path"`
	BuntDbPath       string `yaml:"buntdb_path"`
	TransactionsPath string `yaml:"transactions_path"`
	GroupsDbPath     string `yaml:"groupsdb_path"`
	ReferralsDbPath  string `yaml:"referrals_path"`
}

type LnbitsConfiguration struct {
	AdminId                string   `yaml:"admin_id"`
	AdminKey               string   `yaml:"admin_key"`
	// AdminToken is an lnbits ACL token, scoped to /users/api/v1. Account
	// management moved behind account-level auth in lnbits 1.x, where a wallet
	// api key no longer grants it, so creating users needs a bearer token.
	AdminToken             string   `yaml:"admin_token"`
	Url                    string   `yaml:"url"`
	LnbitsPublicUrl        string   `yaml:"lnbits_public_url"`
	WebhookServer          string   `yaml:"webhook_server"`
	WebhookServerUrl       *url.URL `yaml:"-"`
	WebhookPublicUrl       string   `yaml:"webhook_public_url"`
	WebhookPublicUrlParsed *url.URL `yaml:"-"`
}

type APIConfiguration struct {
	Send      APISendConfiguration      `yaml:"send"`
	Analytics APIAnalyticsConfiguration `yaml:"analytics"`
}

type APIAnalyticsConfiguration struct {
	Enabled            bool                      `yaml:"enabled"`
	APIKeys            map[string]AnalyticsAPIKey `yaml:"api_keys"`
	TimestampTolerance int64                      `yaml:"timestamp_tolerance"` // seconds
}

type AnalyticsAPIKey struct {
	Name       string `yaml:"name"`        // Descriptive name (e.g. "data-team")
	HMACSecret string `yaml:"hmac_secret"` // HMAC secret for this key
}

type APISendConfiguration struct {
	Enabled bool `yaml:"enabled"`
	// InternalNetwork is deprecated and was never enforced. Use AllowedNetworks.
	InternalNetwork string `yaml:"internal_network"`
	// AllowedNetworks, when set, limits the send API to these CIDRs or IPs.
	// Empty means no network restriction (HMAC still applies).
	AllowedNetworks        []string                     `yaml:"allowed_networks"`
	AllowedNets            []*net.IPNet                 `yaml:"-"`
	MaxAmount              int64                        `yaml:"max_amount"`
	MinAmount              int64                        `yaml:"min_amount"`
	AdminApprovalThreshold int64                        `yaml:"admin_approval_threshold"`
	MaxMemoLength          int                          `yaml:"max_memo_length"`
	RateLimit              int                          `yaml:"rate_limit"`
	WhitelistedWallets     map[string]WhitelistedWallet `yaml:"whitelisted_wallets"`
	TimestampTolerance     int64                        `yaml:"timestamp_tolerance"` // seconds
	SettlementTimeout      int64                        `yaml:"settlement_timeout"`  // seconds to wait for an outgoing payment to settle
}

type WhitelistedWallet struct {
	Username               string `yaml:"username"`                 // Telegram username without @
	HMACSecret             string `yaml:"hmac_secret"`              // Unique HMAC secret for this wallet
	AdminApprovalThreshold int64  `yaml:"admin_approval_threshold"` // 0 = use global
	MaxAmount              int64  `yaml:"max_amount"`               // 0 = use global
}

func init() {
	err := configor.Load(&Configuration, configPath(os.Args[1:], os.Getenv("BOT_CONFIG")))
	if err != nil {
		panic(err)
	}
	Configuration.Bot.DonateEndpoint, err = DonationEndpoint(Configuration.Bot.DonateAddress)
	if err != nil {
		panic(err)
	}
	webhookUrl, err := url.Parse(Configuration.Lnbits.WebhookServer)
	if err != nil {
		panic(err)
	}
	Configuration.Lnbits.WebhookServerUrl = webhookUrl

	// Parse webhook public URL if provided, otherwise use webhook server URL
	if Configuration.Lnbits.WebhookPublicUrl != "" {
		webhookPublicUrl, err := url.Parse(Configuration.Lnbits.WebhookPublicUrl)
		if err != nil {
			panic(fmt.Errorf("failed to parse webhook_public_url: %v", err))
		}
		Configuration.Lnbits.WebhookPublicUrlParsed = webhookPublicUrl
	} else {
		Configuration.Lnbits.WebhookPublicUrlParsed = webhookUrl
	}

	lnUrl, err := url.Parse(Configuration.Bot.LNURLServer)
	if err != nil {
		panic(err)
	}
	Configuration.Bot.LNURLServerUrl = lnUrl
	hostname, err := url.Parse(Configuration.Bot.LNURLHostName)
	if err != nil {
		panic(err)
	}
	Configuration.Bot.LNURLHostUrl = hostname
	Configuration.Bot.TrustedProxyNets, err = parseTrustedProxies(Configuration.Bot.TrustedProxies)
	if err != nil {
		panic(fmt.Errorf("bot.trusted_proxies: %w", err))
	}
	checkLnbitsConfiguration()
	setAPISendDefaults()
	setAPIAnalyticsDefaults()
}

// GetWebhookURL returns the appropriate webhook URL
// If webhook_public_url is configured, it returns that (for reverse proxy scenarios)
// Otherwise, it returns the webhook_server URL (for direct access)
func GetWebhookURL() string {
	if Configuration.Lnbits.WebhookPublicUrl != "" {
		return Configuration.Lnbits.WebhookPublicUrl
	}
	return Configuration.Lnbits.WebhookServer
}

// GetWebhookURLParsed returns the parsed webhook URL
func GetWebhookURLParsed() *url.URL {
	return Configuration.Lnbits.WebhookPublicUrlParsed
}

// underTest reports whether this binary is a `go test` binary.
//
// Package init runs before the testing package registers its flags, so
// flag.Lookup("test.v") is not yet usable here and os.Args is the only
// reliable signal. This exists because most packages reach this one through
// the import graph (lnbits -> satdress -> network -> internal), so without it
// a unit test that never talks to lnbits still cannot run unless a full
// operator config.yaml sits in its package directory.
func underTest() bool {
	if strings.HasSuffix(os.Args[0], ".test") || strings.HasSuffix(os.Args[0], ".test.exe") {
		return true
	}
	for _, arg := range os.Args[1:] {
		if strings.HasPrefix(arg, "-test.") {
			return true
		}
	}
	return false
}

// checkLnbitsConfiguration validates the lnbits configuration
func checkLnbitsConfiguration() {
	if Configuration.Lnbits.Url == "" {
		// A missing lnbits url stays fatal in production: the bot cannot move
		// sats without it. Under `go test` there is no wallet to talk to, so
		// refusing to link would only make the package untestable.
		if underTest() {
			return
		}
		panic(fmt.Errorf("please configure a lnbits url"))
	}
	if Configuration.Lnbits.LnbitsPublicUrl == "" {
		log.Warnf("Please specify a lnbits public url otherwise users won't be able to")
	} else {
		if !strings.HasSuffix(Configuration.Lnbits.LnbitsPublicUrl, "/") {
			Configuration.Lnbits.LnbitsPublicUrl = Configuration.Lnbits.LnbitsPublicUrl + "/"
		}
	}
}

// setAPISendDefaults sets default values for API Send configuration
func setAPISendDefaults() {
	if Configuration.API.Send.InternalNetwork != "" {
		log.Warnf("api.send.internal_network is deprecated and ignored; use api.send.allowed_networks to restrict the send API by network")
	}

	if Configuration.API.Send.MaxAmount == 0 {
		Configuration.API.Send.MaxAmount = 1000000 // 1M sats
	}
	if Configuration.API.Send.MinAmount == 0 {
		Configuration.API.Send.MinAmount = 1
	}
	if Configuration.API.Send.AdminApprovalThreshold == 0 {
		Configuration.API.Send.AdminApprovalThreshold = 100000 // 100k sats
	}
	if Configuration.API.Send.MaxMemoLength == 0 {
		Configuration.API.Send.MaxMemoLength = 280
	}
	if Configuration.API.Send.RateLimit == 0 {
		Configuration.API.Send.RateLimit = 60
	}
	if Configuration.API.Send.SettlementTimeout == 0 {
		Configuration.API.Send.SettlementTimeout = 60 // seconds
	}

	if err := validateAPISend(&Configuration.API.Send); err != nil {
		if Configuration.API.Send.Enabled {
			log.Errorf("Disabling API Send: %v", err)
		}
		Configuration.API.Send.Enabled = false
	}

	// Log API Send configuration status
	if Configuration.API.Send.Enabled {
		network := "any (HMAC only)"
		if len(Configuration.API.Send.AllowedNetworks) > 0 {
			network = strings.Join(Configuration.API.Send.AllowedNetworks, ", ")
		}
		log.Infof("API Send module enabled with %d whitelisted wallets, networks: %s",
			len(Configuration.API.Send.WhitelistedWallets), network)
	} else {
		log.Infof("API Send module disabled in configuration")
	}
}

// validateAPISend checks the send API configuration and parses its networks.
// There is deliberately no built-in wallet: a default secret shipped in the
// source would let anyone who reads the code sign requests.
func validateAPISend(cfg *APISendConfiguration) error {
	if len(cfg.WhitelistedWallets) == 0 {
		return fmt.Errorf("no whitelisted_wallets configured")
	}
	for id, w := range cfg.WhitelistedWallets {
		if WeakHMACSecret(w.HMACSecret) {
			return fmt.Errorf("whitelisted wallet %q has an insecure hmac_secret (placeholder or shorter than 32 characters); generate one with: openssl rand -hex 32", id)
		}
		if strings.TrimSpace(w.Username) == "" {
			return fmt.Errorf("whitelisted wallet %q has no username", id)
		}
	}
	nets, err := netutil.ParseNetworks(cfg.AllowedNetworks)
	if err != nil {
		return fmt.Errorf("allowed_networks: %w", err)
	}
	cfg.AllowedNets = nets
	return nil
}

// WeakHMACSecret reports whether an HMAC secret is a placeholder or too short
// to be trusted.
func WeakHMACSecret(secret string) bool {
	if len(secret) < 32 {
		return true
	}
	s := strings.ToLower(secret)
	for _, marker := range placeholderMarkers {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}

// placeholderMarkers appear in example and template secrets. A secret that
// contains one was copied from documentation, not generated, and is public.
var placeholderMarkers = []string{"change-me", "changeme", "your-", "-here", "placeholder"}

// parseTrustedProxies returns the proxies whose forwarding headers are
// believed. An empty list means loopback (the usual local reverse proxy);
// the single entry "none" means trust no proxy and always use the TCP peer.
func parseTrustedProxies(entries []string) ([]*net.IPNet, error) {
	if len(entries) == 0 {
		entries = []string{"127.0.0.1", "::1"}
	}
	if len(entries) == 1 && strings.EqualFold(strings.TrimSpace(entries[0]), "none") {
		return nil, nil
	}
	return netutil.ParseNetworks(entries)
}

// IsAPISendEnabled returns whether the API Send module is enabled
func IsAPISendEnabled() bool {
	return Configuration.API.Send.Enabled
}

// APISendSettlementTimeout returns how long to wait for an outgoing Lightning
// payment to settle before reporting it back as still pending.
func APISendSettlementTimeout() time.Duration {
	return time.Duration(Configuration.API.Send.SettlementTimeout) * time.Second
}

// setAPIAnalyticsDefaults sets default values for API Analytics configuration
func setAPIAnalyticsDefaults() {
	if !Configuration.API.Analytics.Enabled {
		log.Infof("Analytics API disabled in configuration")
		return
	}

	if Configuration.API.Analytics.TimestampTolerance == 0 {
		Configuration.API.Analytics.TimestampTolerance = 300 // 5 minutes
	}

	if len(Configuration.API.Analytics.APIKeys) == 0 {
		log.Errorf("Analytics API enabled but no API keys configured. Disabling analytics API.")
		Configuration.API.Analytics.Enabled = false
		return
	}

	// Reject placeholder/insecure secrets
	for keyID, apiKey := range Configuration.API.Analytics.APIKeys {
		if WeakHMACSecret(apiKey.HMACSecret) {
			log.Errorf("Analytics API key '%s' has an insecure HMAC secret (placeholder or too short). "+
				"Generate a secure secret with: openssl rand -hex 32. Disabling analytics API.", keyID)
			Configuration.API.Analytics.Enabled = false
			return
		}
	}

	log.Infof("Analytics API enabled with %d API keys", len(Configuration.API.Analytics.APIKeys))
}

// IsAPIAnalyticsEnabled returns whether the Analytics API is enabled
func IsAPIAnalyticsEnabled() bool {
	return Configuration.API.Analytics.Enabled
}
