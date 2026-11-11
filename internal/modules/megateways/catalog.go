package megateways

import "strings"

// Field is HermesGatewayField.
type Field struct {
	Key         string  `json:"key"`
	Label       string  `json:"label"`
	Placeholder *string `json:"placeholder,omitempty"`
	Kind        string  `json:"kind"`
	IsRequired  bool    `json:"isRequired"`
}

// entry is HermesGatewayCatalog.Entry.
type entry struct {
	DisplayName, IconSlug, Description string
	RequiredFields                     []Field
	PairingKind                        *string
}

func ptr[T any](v T) *T { return &v }

// gatewayIDs is HermesGatewayID.allCases, in declaration order.
var gatewayIDs = []string{"telegram", "discord", "slack", "whatsapp", "email", "matrix", "ntfy", "mattermost", "photon"}

// catalog is HermesGatewayCatalog.entries, verbatim.
var catalog = map[string]entry{
	"telegram": {
		DisplayName: "Telegram",
		IconSlug:    "telegram",
		Description: "Chat with Lumina from Telegram via a bot you control. Send a voice message and it is transcribed automatically.",
		RequiredFields: []Field{
			{Key: "bot_token", Label: "Bot token", Placeholder: ptr("123456:ABC-DEF…"), Kind: "secret", IsRequired: true},
			{Key: "allowed_users", Label: "Your Telegram user ID(s)", Placeholder: ptr("476978568 (from @userinfobot)"), Kind: "text", IsRequired: true},
		},
	},
	"discord": {
		DisplayName: "Discord",
		IconSlug:    "discord",
		Description: "Connect Lumina to a private Discord server.",
		RequiredFields: []Field{
			{Key: "bot_token", Label: "Bot token", Placeholder: ptr("MTE3…"), Kind: "secret", IsRequired: true},
			{Key: "allowed_users", Label: "Your Discord user ID(s)", Placeholder: ptr("184264928281100288"), Kind: "text", IsRequired: true},
		},
	},
	"slack": {
		DisplayName: "Slack",
		IconSlug:    "slack",
		Description: "Pipe Lumina into a Slack workspace via your Slack app (Socket Mode).",
		RequiredFields: []Field{
			{Key: "bot_token", Label: "Bot token (xoxb-…)", Placeholder: ptr("xoxb-…"), Kind: "secret", IsRequired: true},
			{Key: "app_token", Label: "App token (xapp-…)", Placeholder: ptr("xapp-…"), Kind: "secret", IsRequired: true},
			{Key: "allowed_users", Label: "Your Slack member ID(s)", Placeholder: ptr("U01234567"), Kind: "text", IsRequired: true},
		},
	},
	"whatsapp": {
		DisplayName:    "WhatsApp",
		IconSlug:       "whatsapp",
		Description:    "Chat with Lumina on WhatsApp. Pairs by scanning a QR with your phone — no token to enter.",
		RequiredFields: []Field{},
		PairingKind:    ptr("whatsappQR"),
	},
	"email": {
		DisplayName: "Email",
		IconSlug:    "email",
		Description: "Send and receive email as Lumina over IMAP/SMTP.",
		RequiredFields: []Field{
			{Key: "address", Label: "Email address", Placeholder: ptr("lumina@gmail.com"), Kind: "text", IsRequired: true},
			{Key: "password", Label: "Password (or app password)", Placeholder: ptr("app password"), Kind: "secret", IsRequired: true},
			{Key: "imap_host", Label: "IMAP host", Placeholder: ptr("imap.gmail.com"), Kind: "text", IsRequired: true},
			{Key: "smtp_host", Label: "SMTP host", Placeholder: ptr("smtp.gmail.com"), Kind: "text", IsRequired: true},
			{Key: "allowed_users", Label: "Allowed senders (comma-separated)", Placeholder: ptr("you@email.com"), Kind: "text", IsRequired: true},
			{Key: "imap_port", Label: "IMAP port", Placeholder: ptr("993"), Kind: "text", IsRequired: false},
			{Key: "smtp_port", Label: "SMTP port", Placeholder: ptr("587"), Kind: "text", IsRequired: false},
			{Key: "home_address", Label: "Home address (cron delivery)", Placeholder: ptr("you@email.com"), Kind: "text", IsRequired: false},
		},
	},
	"matrix": {
		DisplayName: "Matrix",
		IconSlug:    "matrix",
		Description: "Chat with Lumina over Matrix — your own homeserver or a public one like matrix.org.",
		RequiredFields: []Field{
			{Key: "homeserver", Label: "Homeserver URL", Placeholder: ptr("https://matrix.org"), Kind: "text", IsRequired: true},
			{Key: "access_token", Label: "Access token", Placeholder: ptr("syt_…"), Kind: "secret", IsRequired: true},
			{Key: "allowed_users", Label: "Allowed user ID(s)", Placeholder: ptr("@you:matrix.org"), Kind: "text", IsRequired: true},
			{Key: "user_id", Label: "Bot user ID (optional)", Placeholder: ptr("@lumina:matrix.org"), Kind: "text", IsRequired: false},
		},
	},
	"ntfy": {
		DisplayName: "ntfy",
		IconSlug:    "ntfy",
		Description: "Push-chat with Lumina via ntfy.sh (or a self-hosted server) — a topic is all you need.",
		RequiredFields: []Field{
			{Key: "topic", Label: "Topic", Placeholder: ptr("lumina-yourname-2026"), Kind: "text", IsRequired: true},
			{Key: "allowed_users", Label: "Allowed topic(s)", Placeholder: ptr("lumina-yourname-2026"), Kind: "text", IsRequired: true},
			{Key: "server_url", Label: "Server URL (optional)", Placeholder: ptr("https://ntfy.sh"), Kind: "text", IsRequired: false},
			{Key: "token", Label: "Token (optional)", Placeholder: ptr("tk_…"), Kind: "secret", IsRequired: false},
		},
	},
	"mattermost": {
		DisplayName: "Mattermost",
		IconSlug:    "mattermost",
		Description: "Connect Lumina to a Mattermost workspace via a bot account (outbound WebSocket).",
		RequiredFields: []Field{
			{Key: "url", Label: "Server URL", Placeholder: ptr("https://mm.example.com"), Kind: "text", IsRequired: true},
			{Key: "token", Label: "Bot token", Placeholder: ptr("bot account access token"), Kind: "secret", IsRequired: true},
			{Key: "allowed_users", Label: "Allowed user ID(s)", Placeholder: ptr("3uo8dkh1p7g1mfk49ear5fzs5c"), Kind: "text", IsRequired: true},
		},
	},
	"photon": {
		DisplayName:    "iMessage (Photon)",
		IconSlug:       "photon",
		Description:    "Free iMessage access via Photon shared lines (the new free path). Device-code login + phone bind; contacts text the assigned number directly.",
		RequiredFields: []Field{},
		PairingKind:    ptr("photonSetup"),
	},
}

// validate is HermesGatewayCatalog.validate: the first required field that is
// missing or blank once trimmed.
func validate(id string, config map[string]string) (missing string, ok bool) {
	for _, f := range catalog[id].RequiredFields {
		if !f.IsRequired {
			continue
		}
		if v, present := config[f.Key]; !present || strings.TrimSpace(v) == "" {
			return f.Key, false
		}
	}
	return "", true
}
