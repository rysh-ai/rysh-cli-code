// SPDX-License-Identifier: Apache-2.0

package channels

// E33 G5: guided bot creation — numbered terminal steps per channel, a Discord
// invite URL built from the validated application id with the minimum
// permissions, and a Slack app manifest whose scopes and events are checked
// against what the Slack adapter really calls and handles. Pure text; no
// network.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/url"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
	"github.com/slack-go/slack"
	"gopkg.in/yaml.v3"
)

func TestBotSetupStepsTelegram(t *testing.T) {
	steps, ok := BotSetupSteps("telegram", BotIdentity{})
	if !ok || len(steps) < 3 {
		t.Fatalf("telegram steps = %v (ok=%v)", steps, ok)
	}
	all := strings.Join(steps, "\n")
	for _, want := range []string{"@BotFather", "/newbot", "@userinfobot"} {
		if !strings.Contains(all, want) {
			t.Errorf("telegram steps miss %q", want)
		}
	}
	// With a validated identity the last step names the bot to message.
	steps, _ = BotSetupSteps("telegram", BotIdentity{ID: "1", Username: "xyz_bot"})
	if last := steps[len(steps)-1]; !strings.Contains(last, "@xyz_bot") || !strings.Contains(last, "https://t.me/xyz_bot") {
		t.Errorf("last step = %q, want it to name @xyz_bot and its t.me link", last)
	}
}

func TestBotSetupStepsDiscord(t *testing.T) {
	steps, ok := BotSetupSteps("discord", BotIdentity{})
	if !ok {
		t.Fatal("no discord steps")
	}
	all := strings.Join(steps, "\n")
	for _, want := range []string{"discord.com/developers/applications", "MESSAGE CONTENT", "Reset Token", "Copy User ID"} {
		if !strings.Contains(all, want) {
			t.Errorf("discord steps miss %q", want)
		}
	}
	// Without an application id there is no invite URL to print; the step
	// points at the portal's URL generator instead.
	if strings.Contains(all, "client_id=") {
		t.Error("invite URL printed without an application id")
	}
	steps, _ = BotSetupSteps("discord", BotIdentity{ID: "7", Username: "xyz", ApplicationID: "999888777666555444"})
	if all := strings.Join(steps, "\n"); !strings.Contains(all, DiscordInviteURL("999888777666555444")) {
		t.Error("discord steps with an application id must print the invite URL")
	}
}

// TestDiscordInviteURLMinimumPermissions: bot scope only, and exactly the
// permissions the adapter needs — read the channel, send, send in threads
// (ThreadID may be a thread channel). Nothing administrative.
func TestDiscordInviteURLMinimumPermissions(t *testing.T) {
	u, err := url.Parse(DiscordInviteURL("999888777666555444"))
	if err != nil {
		t.Fatal(err)
	}
	if u.Host != "discord.com" || u.Path != "/oauth2/authorize" {
		t.Errorf("invite URL host/path = %s%s", u.Host, u.Path)
	}
	q := u.Query()
	if q.Get("client_id") != "999888777666555444" || q.Get("scope") != "bot" {
		t.Errorf("query = %v", q)
	}
	perms, err := strconv.ParseInt(q.Get("permissions"), 10, 64)
	if err != nil {
		t.Fatalf("permissions not an integer: %q", q.Get("permissions"))
	}
	want := int64(discordgo.PermissionViewChannel | discordgo.PermissionSendMessages | discordgo.PermissionSendMessagesInThreads)
	if perms != want {
		t.Errorf("permissions = %d, want %d (view channel + send + send in threads)", perms, want)
	}
	if perms&int64(discordgo.PermissionAdministrator) != 0 || perms&int64(discordgo.PermissionManageMessages) != 0 {
		t.Error("invite asks for an administrative permission")
	}
	if DiscordInviteURL("") != "" {
		t.Error("an empty application id must yield no URL")
	}
}

func TestBotSetupStepsSlack(t *testing.T) {
	steps, ok := BotSetupSteps("slack", BotIdentity{})
	if !ok {
		t.Fatal("no slack steps")
	}
	all := strings.Join(steps, "\n")
	for _, want := range []string{"api.slack.com/apps", "manifest", "xoxb-", "xapp-", "connections:write", "Copy member ID"} {
		if !strings.Contains(all, want) {
			t.Errorf("slack steps miss %q", want)
		}
	}
	link := SlackCreateFromManifestURL("rysh")
	u, err := url.Parse(link)
	if err != nil || u.Host != "api.slack.com" || u.Query().Get("new_app") != "1" {
		t.Fatalf("create link = %q", link)
	}
	if u.Query().Get("manifest_yaml") != SlackAppManifest("rysh") {
		t.Error("create link must carry the same manifest the steps print")
	}
	if _, ok := BotSetupSteps("whatsapp", BotIdentity{}); ok {
		t.Error("channels without a guide must report ok=false")
	}
}

// slackManifest is the subset of the Slack app manifest the test inspects.
type slackManifest struct {
	DisplayInformation struct {
		Name string `yaml:"name"`
	} `yaml:"display_information"`
	Features struct {
		AppHome struct {
			MessagesTabEnabled         bool `yaml:"messages_tab_enabled"`
			MessagesTabReadOnlyEnabled bool `yaml:"messages_tab_read_only_enabled"`
		} `yaml:"app_home"`
		BotUser struct {
			DisplayName string `yaml:"display_name"`
		} `yaml:"bot_user"`
	} `yaml:"features"`
	OAuthConfig struct {
		Scopes struct {
			Bot []string `yaml:"bot"`
		} `yaml:"scopes"`
	} `yaml:"oauth_config"`
	Settings struct {
		EventSubscriptions struct {
			BotEvents []string `yaml:"bot_events"`
		} `yaml:"event_subscriptions"`
		SocketModeEnabled bool `yaml:"socket_mode_enabled"`
	} `yaml:"settings"`
}

func parseManifest(t *testing.T) slackManifest {
	t.Helper()
	var m slackManifest
	if err := yaml.Unmarshal([]byte(SlackAppManifest("rysh assistant")), &m); err != nil {
		t.Fatalf("manifest is not valid YAML: %v", err)
	}
	return m
}

func TestSlackManifestShape(t *testing.T) {
	m := parseManifest(t)
	if m.DisplayInformation.Name != "rysh assistant" || m.Features.BotUser.DisplayName == "" {
		t.Errorf("names = %q / %q", m.DisplayInformation.Name, m.Features.BotUser.DisplayName)
	}
	if !m.Settings.SocketModeEnabled {
		t.Error("the adapter connects over Socket Mode; the manifest must enable it")
	}
	// Without the Messages tab (and not read-only) a user cannot DM the app.
	if !m.Features.AppHome.MessagesTabEnabled || m.Features.AppHome.MessagesTabReadOnlyEnabled {
		t.Error("the App Home Messages tab must be enabled and writable so the owner can DM the bot")
	}
}

// slackScopesForMethod maps every *slack.Client method the adapter calls to
// the bot scopes it needs. A new call in the adapter that is not listed here
// fails TestSlackManifestCoversAdapterCalls — add it with its scopes.
var slackScopesForMethod = map[string][]string{
	"AuthTest":                   nil,
	"AuthTestContext":            nil,
	"PostMessage":                {"chat:write"},
	"GetUserInfo":                {"users:read"},
	"GetConversationInfo":        {"channels:read", "groups:read", "im:read", "mpim:read"},
	"GetConversationInfoContext": {"channels:read", "groups:read", "im:read", "mpim:read"},
}

// slackClientCalls returns every *slack.Client method name called from the
// adapter's non-test sources.
func slackClientCalls(t *testing.T) []string {
	t.Helper()
	clientType := reflect.TypeOf(&slack.Client{})
	seen := map[string]bool{}
	fset := token.NewFileSet()
	for _, file := range []string{"slack.go", "validate.go"} {
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if id, ok := sel.X.(*ast.Ident); ok && (id.Name == "slack" || id.Name == "slog" || id.Name == "socketmode") {
				return true // package-qualified function, not a client method
			}
			if _, ok := clientType.MethodByName(sel.Sel.Name); ok {
				seen[sel.Sel.Name] = true
			}
			return true
		})
	}
	var out []string
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func TestSlackManifestCoversAdapterCalls(t *testing.T) {
	m := parseManifest(t)
	scopes := map[string]bool{}
	for _, s := range m.OAuthConfig.Scopes.Bot {
		scopes[s] = true
	}
	calls := slackClientCalls(t)
	if len(calls) < 3 {
		t.Fatalf("found only %v Slack client calls — the scan is broken", calls)
	}
	for _, call := range calls {
		need, known := slackScopesForMethod[call]
		if !known {
			t.Errorf("adapter calls slack.Client.%s, which has no scope entry", call)
			continue
		}
		for _, s := range need {
			if !scopes[s] {
				t.Errorf("adapter calls %s, which needs %s; the manifest lacks it", call, s)
			}
		}
	}
	// Events the adapter handles (handleEventsAPI: MessageEvent,
	// AppMentionEvent), with the scope each subscription requires.
	events := map[string]bool{}
	for _, e := range m.Settings.EventSubscriptions.BotEvents {
		events[e] = true
	}
	for event, scope := range map[string]string{
		"message.im":       "im:history", // DMs — the first thing a new user sends
		"message.channels": "channels:history",
		"message.groups":   "groups:history",
		"message.mpim":     "mpim:history",
		"app_mention":      "app_mentions:read",
	} {
		if !events[event] {
			t.Errorf("manifest does not subscribe to %s", event)
		}
		if !scopes[scope] {
			t.Errorf("event %s needs scope %s; the manifest lacks it", event, scope)
		}
	}
	// Least privilege: nothing beyond what the calls and events need.
	allowed := map[string]bool{"im:history": true, "channels:history": true, "groups:history": true,
		"mpim:history": true, "app_mentions:read": true}
	for _, need := range slackScopesForMethod {
		for _, s := range need {
			allowed[s] = true
		}
	}
	for s := range scopes {
		if !allowed[s] {
			t.Errorf("manifest asks for %s, which the adapter does not use", s)
		}
	}
}
