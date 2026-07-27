package actors

import "testing"

// Regression: a "---" inside a YAML comment must not be mistaken for the closing
// frontmatter delimiter (previously truncated the contacts block -> empty creds).
func TestSplitFrontmatterDashInComment(t *testing.T) {
	content := "---\n" +
		"name: bot\n" +
		"contacts:\n" +
		"  whatsapp:\n" +
		"    # --- Meta WhatsApp Cloud API ---------------------------\n" +
		"    api_key: \"TOKEN123\"\n" +
		"---\n" +
		"You are a bot.\n"
	fm, body, ok := splitFrontmatter(content)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if want := "TOKEN123"; !contains(fm, want) {
		t.Errorf("frontmatter missing api_key; got:\n%s", fm)
	}
	if want := "You are a bot."; !contains(body, want) {
		t.Errorf("body wrong; got: %q", body)
	}
}

func TestParseHumanoidDashInComment(t *testing.T) {
	// envOnlyExpand leaves ${VAR} as-is; we just check the field is populated.
	def, err := func() (*humanoidDefinition, error) {
		// write a temp skill file
		return parseHumanoidFileFromString(
			"---\nname: wa\ncontacts:\n  whatsapp:\n    # --- header ---\n    api_key: \"ABC\"\n    phone: \"123\"\n---\nbody\n")
	}()
	if err != nil {
		t.Fatal(err)
	}
	wa := def.Contacts["whatsapp"]
	if wa.APIKey != "ABC" || wa.Phone != "123" {
		t.Errorf("whatsapp creds not parsed: api_key=%q phone=%q", wa.APIKey, wa.Phone)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
