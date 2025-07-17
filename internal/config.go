package internal

import (
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/jinzhu/configor"
	log "github.com/sirupsen/logrus"
)

var Configuration = struct {
	Bot      BotConfiguration      `yaml:"bot"`
	Telegram TelegramConfiguration `yaml:"telegram"`
	Database DatabaseConfiguration `yaml:"database"`
	Lnbits   LnbitsConfiguration   `yaml:"lnbits"`
	Generate GenerateConfiguration `yaml:"generate"`
	Nostr    NostrConfiguration    `yaml:"nostr"`
	API      APIConfiguration      `yaml:"api"`
}{}

type NostrConfiguration struct {
	PrivateKey string `yaml:"private_key"`
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
}

type LnbitsConfiguration struct {
	AdminId                string   `yaml:"admin_id"`
	AdminKey               string   `yaml:"admin_key"`
	Url                    string   `yaml:"url"`
	LnbitsPublicUrl        string   `yaml:"lnbits_public_url"`
	WebhookServer          string   `yaml:"webhook_server"`
	WebhookServerUrl       *url.URL `yaml:"-"`
	WebhookPublicUrl       string   `yaml:"webhook_public_url"`
	WebhookPublicUrlParsed *url.URL `yaml:"-"`
}

type APIConfiguration struct {
	Send APISendConfiguration `yaml:"send"`
}

type APISendConfiguration struct {
	Enabled                bool   `yaml:"enabled"`
	InternalNetwork        string `yaml:"internal_network"`
	MaxAmount              int64  `yaml:"max_amount"`
	MinAmount              int64  `yaml:"min_amount"`
	AdminApprovalThreshold int64  `yaml:"admin_approval_threshold"`
	MaxMemoLength          int    `yaml:"max_memo_length"`
	RateLimit              int    `yaml:"rate_limit"`
	HMACSecret             string `yaml:"hmac_secret"`
	TimestampTolerance     int64  `yaml:"timestamp_tolerance"` // seconds
	FromUserId             string `yaml:"from_user_id"`        // Telegram user ID to use as sender
}

func init() {
	err := configor.Load(&Configuration, "config.yaml")
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
	checkLnbitsConfiguration()
	setAPISendDefaults()
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

// checkLnbitsConfiguration validates the lnbits configuration
func checkLnbitsConfiguration() {
	if Configuration.Lnbits.Url == "" {
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
	// Set defaults only if not configured
	if Configuration.API.Send.InternalNetwork == "" {
		Configuration.API.Send.InternalNetwork = "10.0.0.0/24"
	}

	// Validate CIDR format
	_, _, err := net.ParseCIDR(Configuration.API.Send.InternalNetwork)
	if err != nil {
		log.Errorf("Invalid internal_network CIDR format '%s': %v. Using default 10.0.0.0/24",
			Configuration.API.Send.InternalNetwork, err)
		Configuration.API.Send.InternalNetwork = "10.0.0.0/24"
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

	// FromUserId and HMACSecret must be configured in environment - no default value
	if Configuration.API.Send.Enabled {
		if Configuration.API.Send.HMACSecret == "" {
			panic(fmt.Errorf("API Send is enabled but CONFIGOR_API_SEND_HMAC_SECRET is not configured"))
		}

		if Configuration.API.Send.FromUserId == "" {
			panic(fmt.Errorf("API Send is enabled but CONFIGOR_API_SEND_FROM_USER_ID is not configured"))
		}
	}
}

// IsAPISendEnabled returns whether the API Send module is enabled
func IsAPISendEnabled() bool {
	return Configuration.API.Send.Enabled
}
