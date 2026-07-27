package actors

// resolveChannelEnvVars expands ${VAR} field by field from an explicit list, so
// a channel field that is not on that list silently keeps the literal
// "${VAR}" text and the adapter authenticates with the string "${TEAMS_APP_SECRET}".
// These tests pin the two newest channels' credential fields to that list.

import (
	"testing"
)

func TestTeamsSkillFileResolvesCredentials(t *testing.T) {
	t.Setenv("TEAMS_APP_ID", "app-id-from-env")
	t.Setenv("TEAMS_APP_SECRET", "secret-from-env")
	t.Setenv("TEAMS_TENANT", "tenant-from-env")

	def, err := parseHumanoidFileFromString(
		"---\nname: teamsbot\ncontacts:\n  teams:\n" +
			"    app_id: \"${TEAMS_APP_ID}\"\n" +
			"    app_secret: \"${TEAMS_APP_SECRET}\"\n" +
			"    tenant_id: \"${TEAMS_TENANT}\"\n" +
			"---\nbody\n")
	if err != nil {
		t.Fatal(err)
	}

	teams := def.Contacts["teams"]
	if teams.AppID != "app-id-from-env" {
		t.Errorf("app_id = %q, want the expanded value", teams.AppID)
	}
	if teams.AppSecret != "secret-from-env" {
		t.Errorf("app_secret = %q, want the expanded value", teams.AppSecret)
	}
	if teams.TenantID != "tenant-from-env" {
		t.Errorf("tenant_id = %q, want the expanded value", teams.TenantID)
	}
}

func TestPhoneSkillFileResolvesCredentials(t *testing.T) {
	t.Setenv("TWILIO_ACCOUNT_SID", "ACfromenv")
	t.Setenv("TWILIO_AUTH_TOKEN", "tok-from-env")

	def, err := parseHumanoidFileFromString(
		"---\nname: smsbot\ncontacts:\n  phone:\n" +
			"    number: \"+15550100\"\n" +
			"    provider: \"twilio\"\n" +
			"    account_sid: \"${TWILIO_ACCOUNT_SID}\"\n" +
			"    auth_token: \"${TWILIO_AUTH_TOKEN}\"\n" +
			"    webhook_url: \"https://tunnel.example/sms\"\n" +
			"---\nbody\n")
	if err != nil {
		t.Fatal(err)
	}

	phone := def.Contacts["phone"]
	if phone.AccountSID != "ACfromenv" {
		t.Errorf("account_sid = %q, want the expanded value", phone.AccountSID)
	}
	if phone.AuthToken != "tok-from-env" {
		t.Errorf("auth_token = %q, want the expanded value", phone.AuthToken)
	}
	// webhook_url is what enables inbound signature validation, so losing it in
	// parsing would silently downgrade the channel to unauthenticated.
	if phone.WebhookURL != "https://tunnel.example/sms" {
		t.Errorf("webhook_url = %q", phone.WebhookURL)
	}
	if phone.Number != "+15550100" {
		t.Errorf("number = %q", phone.Number)
	}
}
