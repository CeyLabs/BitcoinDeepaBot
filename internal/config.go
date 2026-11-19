package internal

import (
	"crypto/ed25519"
	"crypto/subtle"
	"encoding/base64"
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
	// AdminAPIToken, when set, is required as "Authorization: Bearer <token>"
	// on every admin API request. It is mandatory if admin_api_host is not a
	// loopback address.
	AdminAPIToken string `yaml:"admin_api_token"`
	// AdminPprof exposes Go's /debug/pprof on the admin API. Off by default:
	// profiles reveal memory contents, including keys.
	AdminPprof bool `yaml:"admin_pprof"`
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

// DefaultAdminAPIHost is used when admin_api_host is empty. An empty address
// would otherwise make net/http listen on port 80 on every interface.
const DefaultAdminAPIHost = "127.0.0.1:6060"

// AdminAPIAllowed decides whether the admin API may start with this address
// and token. The admin API can ban users and dump process memory, so it only
// runs unauthenticated on loopback.
func AdminAPIAllowed(host, token string) error {
	if token != "" && WeakHMACSecret(token) {
		return fmt.Errorf("bot.admin_api_token is a placeholder or shorter than 32 characters; generate one with: openssl rand -hex 32")
	}
	if token == "" && !IsLoopbackAddress(host) {
		return fmt.Errorf("bot.admin_api_host %q is not a loopback address; set bot.admin_api_token or bind to 127.0.0.1", host)
	}
	return nil
}

// IsLoopbackAddress reports whether a host:port listen address only accepts
// local connections. An empty host (":6060") listens everywhere.
func IsLoopbackAddress(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
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
	// WebhookSecret, when set, is added to every webhook URL the bot hands to
	// lnbits and is required on every incoming webhook. Requests without it are
	// dropped before they are parsed. Generate one with: openssl rand -hex 32
	WebhookSecret string `yaml:"webhook_secret"`
}

type APIConfiguration struct {
	Send      APISendConfiguration      `yaml:"send"`
	Analytics APIAnalyticsConfiguration `yaml:"analytics"`
	// AllowedNetworks, when set, limits every /api/v1/ endpoint to these IPs
	// or CIDRs: the send API, analytics and the per-user wallet API. Empty
	// means no restriction. The client address is resolved through
	// bot.trusted_proxies.
	AllowedNetworks []string     `yaml:"allowed_networks"`
	AllowedNets     []*net.IPNet `yaml:"-"`
	P2P       APIP2PConfiguration       `yaml:"p2p"`
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
	if err := checkWebhookSecret(Configuration.Lnbits.WebhookSecret); err != nil {
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
	if Configuration.Bot.AdminAPIHost == "" {
		Configuration.Bot.AdminAPIHost = DefaultAdminAPIHost
	}
	Configuration.Bot.TrustedProxyNets, err = parseTrustedProxies(Configuration.Bot.TrustedProxies)
	if err != nil {
		panic(fmt.Errorf("bot.trusted_proxies: %w", err))
	}
	normalizeLnbitsConfiguration()
	// An unreadable allowlist must stop the bot rather than leave the API open.
	// Parsed before setAPISendDefaults, which logs it.
	Configuration.API.AllowedNets, err = netutil.ParseNetworks(Configuration.API.AllowedNetworks)
	if err != nil {
		panic(fmt.Errorf("api.allowed_networks: %w", err))
	}
	// Entries that are all blank (e.g. an unset env var) parse to nothing,
	// which would mean "no restriction". Treat that as a config error.
	if len(Configuration.API.AllowedNetworks) > 0 && len(Configuration.API.AllowedNets) == 0 {
		panic(fmt.Errorf("api.allowed_networks: all %d entries are blank", len(Configuration.API.AllowedNetworks)))
	}
	if len(Configuration.API.AllowedNets) > 0 {
		log.Infof("/api/v1/ restricted to: %s", strings.Join(Configuration.API.AllowedNetworks, ", "))
	}
	setAPISendDefaults()
	setAPIAnalyticsDefaults()
	setAPIP2PDefaults(&Configuration.API.P2P)
}

// WebhookSecretParam is the query parameter that carries the webhook secret.
// A query parameter rather than a path segment keeps working behind reverse
// proxies that rewrite or strip paths.
const WebhookSecretParam = "s"

// GetWebhookURL returns the URL lnbits should call for payment webhooks.
// If webhook_public_url is configured, it returns that (for reverse proxy scenarios)
// Otherwise, it returns the webhook_server URL (for direct access)
// When webhook_secret is set, it is appended as a query parameter.
func GetWebhookURL() string {
	return withWebhookSecret(webhookBaseURL(), Configuration.Lnbits.WebhookSecret)
}

// GetWebhookURLRedacted is GetWebhookURL with the secret masked, for logs.
func GetWebhookURLRedacted() string {
	if Configuration.Lnbits.WebhookSecret == "" {
		return webhookBaseURL()
	}
	return withWebhookSecret(webhookBaseURL(), "REDACTED")
}

func webhookBaseURL() string {
	if Configuration.Lnbits.WebhookPublicUrl != "" {
		return Configuration.Lnbits.WebhookPublicUrl
	}
	return Configuration.Lnbits.WebhookServer
}

func withWebhookSecret(base, secret string) string {
	if secret == "" {
		return base
	}
	u, err := url.Parse(base)
	if err != nil {
		return base
	}
	q := u.Query()
	q.Set(WebhookSecretParam, secret)
	u.RawQuery = q.Encode()
	return u.String()
}

// WebhookSecretMatches reports whether a webhook request carries the configured
// secret, comparing in constant time. With no secret configured every request
// passes this check and still has to pass payment verification.
func WebhookSecretMatches(got string) bool {
	return secretMatches(got, Configuration.Lnbits.WebhookSecret)
}

func secretMatches(got, want string) bool {
	if want == "" {
		return true
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// checkWebhookSecret refuses to start with a placeholder or short secret. An
// empty secret is allowed (webhooks are still verified against lnbits), but a
// weak one would only give false confidence.
func checkWebhookSecret(secret string) error {
	if secret == "" {
		return nil
	}
	if strings.Contains(secret, "change-me") || len(secret) < 32 {
		return fmt.Errorf("lnbits.webhook_secret is a placeholder or shorter than 32 characters; generate one with: openssl rand -hex 32")
	}
	return nil
}

// GetWebhookURLParsed returns the parsed webhook URL
func GetWebhookURLParsed() *url.URL {
	return Configuration.Lnbits.WebhookPublicUrlParsed
}

// CheckLnbitsConfiguration panics when no lnbits url is configured: the bot
// cannot move sats without one. main calls it at startup. It is not run from
// init because most packages import this one (lnbits -> satdress -> network ->
// internal), and a unit test that never talks to lnbits must not need a full
// operator config.yaml to load.
func CheckLnbitsConfiguration() {
	if Configuration.Lnbits.Url == "" {
		panic(fmt.Errorf("please configure a lnbits url"))
	}
}

// normalizeLnbitsConfiguration fills in derived lnbits settings.
func normalizeLnbitsConfiguration() {
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
		// Both lists apply: a caller must be in api.allowed_networks and in
		// api.send.allowed_networks.
		var limits []string
		if len(Configuration.API.AllowedNetworks) > 0 {
			limits = append(limits, "api: "+strings.Join(Configuration.API.AllowedNetworks, ", "))
		}
		if len(Configuration.API.Send.AllowedNetworks) > 0 {
			limits = append(limits, "send: "+strings.Join(Configuration.API.Send.AllowedNetworks, ", "))
		}
		network := "any (HMAC only)"
		if len(limits) > 0 {
			network = strings.Join(limits, " AND ")
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

// APIP2PConfiguration configures sending from the Telegram Mini App
// (/api/v1/p2p/*). The sender is always the Telegram user in the Mini App's
// signed init data. Amounts are in sats.
type APIP2PConfiguration struct {
	Enabled            bool  `yaml:"enabled"`
	MaxAmount          int64 `yaml:"max_amount"`            // per transfer
	DailyLimit         int64 `yaml:"daily_limit"`           // per sender, per UTC day
	ConfirmThreshold   int64 `yaml:"confirm_threshold"`     // above this, the sender confirms in the bot chat
	RateLimitPerMinute int   `yaml:"rate_limit_per_minute"` // sends and invoice payments per sender
	InitDataMaxAge     int64 `yaml:"init_data_max_age"`     // seconds
	// SpendInitDataMaxAge is a shorter init data age for /send and
	// /invoice/pay, so leaked init data can't move sats for long.
	SpendInitDataMaxAge int64 `yaml:"spend_init_data_max_age"` // seconds
	MaxMemoLength       int   `yaml:"max_memo_length"`
	// ServicePublicKey is the base64 Ed25519 public key of the Mini App
	// server. Every P2P call must be signed with its private key. Required.
	ServicePublicKey string            `yaml:"service_public_key"`
	ServiceKey       ed25519.PublicKey `yaml:"-"`
}

// DecodeKeyBase64 decodes standard or URL-safe base64, padded or not.
func DecodeKeyBase64(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	var err error
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		var b []byte
		if b, err = enc.DecodeString(s); err == nil {
			return b, nil
		}
	}
	return nil, err
}

func setAPIP2PDefaults(c *APIP2PConfiguration) {
	if c.MaxAmount <= 0 {
		c.MaxAmount = 100000
	}
	if c.DailyLimit <= 0 {
		c.DailyLimit = 500000
	}
	if c.ConfirmThreshold <= 0 {
		c.ConfirmThreshold = 50000
	}
	if c.RateLimitPerMinute <= 0 {
		c.RateLimitPerMinute = 10
	}
	if c.InitDataMaxAge <= 0 {
		c.InitDataMaxAge = 3600
	}
	if c.MaxMemoLength <= 0 {
		c.MaxMemoLength = 280
	}
	if c.SpendInitDataMaxAge <= 0 {
		c.SpendInitDataMaxAge = 600
	}
	if c.SpendInitDataMaxAge > c.InitDataMaxAge {
		c.SpendInitDataMaxAge = c.InitDataMaxAge
	}
	if c.Enabled {
		key, err := DecodeKeyBase64(c.ServicePublicKey)
		if c.ServicePublicKey == "" || err != nil || len(key) != ed25519.PublicKeySize {
			log.Errorf("Disabling P2P API: api.p2p.service_public_key must be a base64 Ed25519 public key (see docs/p2p-api.md)")
			c.Enabled = false
			return
		}
		c.ServiceKey = ed25519.PublicKey(key)
	}
	if c.Enabled {
		log.Infof("P2P API enabled: max %d sat per transfer, %d sat per day, confirm above %d sat", c.MaxAmount, c.DailyLimit, c.ConfirmThreshold)
	}
}
