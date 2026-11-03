// SPDX-License-Identifier: Apache-2.0

package channels

// botsetup.go — guided bot creation (E33 G5). Per channel, the numbered steps
// a terminal prints while the user creates the bot, plus the two generated
// artefacts that turn the slowest steps into a paste: the Discord invite URL
// (built from the application id Validate learned) and the Slack app manifest.
//
// Pure text: nothing here makes a network call, and no step ever echoes a
// token — it says where to get one and that it is stored in .rysh/secrets.

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/bwmarrin/discordgo"
	"gopkg.in/yaml.v3"
)

// discordInvitePermissions is the minimum the adapter needs: see the channel,
// send, and send inside threads (a reply's ThreadID may be a thread channel).
// Message content arrives through the MESSAGE CONTENT intent, not a
// permission, and nothing administrative is ever requested.
const discordInvitePermissions = discordgo.PermissionViewChannel |
	discordgo.PermissionSendMessages |
	discordgo.PermissionSendMessagesInThreads

// DiscordInviteURL returns the OAuth2 URL that adds the bot to a server with
// the minimum permissions, or "" when the application id is unknown.
func DiscordInviteURL(applicationID string) string {
	if applicationID == "" {
		return ""
	}
	q := url.Values{}
	q.Set("client_id", applicationID)
	q.Set("scope", "bot")
	q.Set("permissions", strconv.FormatInt(int64(discordInvitePermissions), 10))
	return "https://discord.com/oauth2/authorize?" + q.Encode()
}

// SlackAppManifest renders the app manifest for the Slack adapter: Socket
// Mode, a writable Messages tab (so the owner can DM the app), the bot scopes
// for exactly the Web API calls the adapter makes (chat.postMessage,
// users.info, conversations.info) and the events it handles (message.* and
// app_mention). e33_botsetup_test.go checks it against slack.go.
func SlackAppManifest(appName string) string {
	if appName == "" {
		appName = "rysh assistant"
	}
	type node = map[string]any
	manifest := node{
		"display_information": node{
			"name":        appName,
			"description": "Talk to your rysh terminal session from Slack",
		},
		"features": node{
			"app_home": node{
				"home_tab_enabled":               false,
				"messages_tab_enabled":           true,
				"messages_tab_read_only_enabled": false,
			},
			"bot_user": node{
				"display_name":  appName,
				"always_online": true,
			},
		},
		"oauth_config": node{
			"scopes": node{
				"bot": []string{
					"app_mentions:read",
					"channels:history", "groups:history", "im:history", "mpim:history",
					"channels:read", "groups:read", "im:read", "mpim:read",
					"chat:write",
					"users:read",
				},
			},
		},
		"settings": node{
			"event_subscriptions": node{
				"bot_events": []string{
					"app_mention",
					"message.channels", "message.groups", "message.im", "message.mpim",
				},
			},
			"interactivity":          node{"is_enabled": false},
			"org_deploy_enabled":     false,
			"socket_mode_enabled":    true,
			"token_rotation_enabled": false,
		},
	}
	out, err := yaml.Marshal(manifest)
	if err != nil {
		// A static map of strings and bools cannot fail to marshal.
		panic(fmt.Sprintf("slack manifest: %v", err))
	}
	return string(out)
}

// SlackCreateFromManifestURL is the "create an app from this manifest" link;
// it opens Slack's new-app dialog with the manifest pre-filled.
func SlackCreateFromManifestURL(appName string) string {
	q := url.Values{}
	q.Set("new_app", "1")
	q.Set("manifest_yaml", SlackAppManifest(appName))
	return "https://api.slack.com/apps?" + q.Encode()
}

// BotSetupSteps returns the numbered steps for creating the bot on channel,
// or ok=false when there is no guide for it. id is what Validate learned, if
// anything: with a username the last step names the bot to message, and with
// a Discord application id the invite URL is printed in full.
func BotSetupSteps(channel string, id BotIdentity) (steps []string, ok bool) {
	switch channel {
	case "telegram":
		steps = []string{
			"Open Telegram and message @BotFather (https://t.me/BotFather).",
			"Send /newbot, pick a display name, then a username that ends in \"bot\".",
			"BotFather replies with a token like 123456789:AA… — paste it when asked " +
				"(it is stored in .rysh/secrets; the profile only references it).",
			"Get your own numeric user id: message @userinfobot and it replies with it.",
		}
		if id.Username != "" {
			steps = append(steps, fmt.Sprintf("Now message %s (https://t.me/%s) — a plain \"hi\" is enough.",
				id.Handle(), id.Username))
		} else {
			steps = append(steps, "When setup finishes, open your bot in Telegram and send \"hi\".")
		}
		return numbered(steps), true

	case "discord":
		steps = []string{
			"Open https://discord.com/developers/applications → New Application, and name it.",
			"Bot → Reset Token → copy the token and paste it when asked " +
				"(it is stored in .rysh/secrets; the profile only references it).",
			"Bot → Privileged Gateway Intents → turn on MESSAGE CONTENT INTENT → Save Changes " +
				"(without it every message arrives empty).",
		}
		if invite := DiscordInviteURL(id.ApplicationID); invite != "" {
			steps = append(steps, "Add the bot to your server: "+invite)
		} else {
			steps = append(steps, "Add the bot to your server: OAuth2 → URL Generator → scope \"bot\", permissions "+
				"View Channels, Send Messages, Send Messages in Threads → open the generated URL.")
		}
		steps = append(steps,
			"Get your own user id: Settings → Advanced → Developer Mode on, then right-click your name → Copy User ID.")
		if id.Username != "" {
			steps = append(steps, fmt.Sprintf("Now DM %s (right-click it in the server member list → Message).", id.Handle()))
		} else {
			steps = append(steps, "When setup finishes, DM the bot (right-click it in the member list → Message).")
		}
		return numbered(steps), true

	case "slack":
		steps = []string{
			"Open https://api.slack.com/apps?new_app=1 → From an app manifest → pick your workspace, " +
				"and paste the manifest below (or open the create link, which pre-fills it).",
			"Install to Workspace → copy the Bot User OAuth Token (xoxb-…) and paste it when asked.",
			"Basic Information → App-Level Tokens → Generate Token and Scopes → add connections:write " +
				"→ copy the token (xapp-…) and paste it when asked. Both tokens are stored in .rysh/secrets.",
			"Get your own member id: your avatar → Profile → ⋮ More → Copy member ID.",
		}
		if id.Username != "" {
			steps = append(steps, fmt.Sprintf("Now open %s under Apps in Slack and send it a message.", id.Handle()))
		} else {
			steps = append(steps, "When setup finishes, open the app under Apps in Slack and send it a message.")
		}
		return numbered(steps), true

	case "mattermost":
		steps = []string{
			"System Console → Integrations → Bot Accounts → set Enable Bot Account Creation to true.",
			"Integrations → Bot Accounts → Add Bot Account → create it, then copy the access token it shows " +
				"once and paste it when asked (it is stored in .rysh/secrets; the profile only references it).",
			"Enter your server URL (https://chat.example.com) when asked.",
			"Add the bot to the team and the channels it should read; a direct message needs no invite.",
			"Get your own user ID (26 characters, not your @username): System Console → User Management → " +
				"Users → your name.",
		}
		if id.Username != "" {
			steps = append(steps, fmt.Sprintf("Now send %s a direct message.", id.Handle()))
		} else {
			steps = append(steps, "When setup finishes, send the bot a direct message.")
		}
		return numbered(steps), true

	case "matrix":
		steps = []string{
			"Create a user for the bot on your homeserver and log it in once with any client (e.g. Element).",
			"Copy the bot's access token (Element: Settings → Help & About → Access Token) and paste it when " +
				"asked (it is stored in .rysh/secrets; the profile only references it).",
			"Enter the homeserver URL (https://matrix.example.org) when asked.",
			"This version reads unencrypted rooms only: create the bot's DM with encryption off " +
				"(it cannot be turned off later).",
			"Get your own user ID: it looks like @you:example.org (Element shows it under your avatar).",
		}
		if strings.HasPrefix(id.ID, "@") {
			steps = append(steps, fmt.Sprintf("Now start an unencrypted direct chat with %s and send \"hi\".", id.ID))
		} else {
			steps = append(steps, "When setup finishes, start an unencrypted direct chat with the bot and send \"hi\".")
		}
		return numbered(steps), true

	case "line":
		steps = []string{
			"Open https://developers.line.biz/console/ → create a provider, then a Messaging API channel.",
			"Basic settings → copy the Channel secret; Messaging API → issue a long-lived Channel access token. " +
				"Paste both when asked (they are stored in .rysh/secrets; the profile only references them).",
			"rysh serves the webhook on 127.0.0.1:<webhook_port>/line/webhook (default 8790). Expose it with a " +
				"tunnel, set the public URL ending /line/webhook as Messaging API → Webhook URL, and turn Use webhook on.",
			"Get your own user ID (U followed by 32 characters, not your LINE ID): Basic settings → Your user ID.",
		}
		if id.Username != "" {
			steps = append(steps, fmt.Sprintf("Now add %s as a friend in LINE and send \"hi\".", id.Handle()))
		} else {
			steps = append(steps, "When setup finishes, add the bot as a friend (Messaging API → QR code) and send \"hi\".")
		}
		return numbered(steps), true
	}
	return nil, false
}

// numbered prefixes each step with "1. ", "2. ", …
func numbered(steps []string) []string {
	out := make([]string, len(steps))
	for i, s := range steps {
		out[i] = fmt.Sprintf("%d. %s", i+1, s)
	}
	return out
}
