// SPDX-License-Identifier: Apache-2.0

package config

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// CompanyConfig binds a session to one AI-native company on the upstream
// server (design 037, "Company Connect"). It is written by `ry connect` (and
// by the desktop app through the same command) next to the `upstream:` block
// that carries the read-only key; a session whose config has it opens on the
// company view instead of an empty shell.
//
// Nothing here is secret: the key lives in `upstream.api_key`. These fields
// only name the company so the daemon knows which one to follow and the TUI
// can label it before the first fetch returns.
type CompanyConfig struct {
	ID     string `yaml:"id"`
	Name   string `yaml:"name"`
	Slug   string `yaml:"slug"`
	FQDN   string `yaml:"fqdn"`
	Server string `yaml:"server"`
}

// Enabled reports whether this session is bound to a company.
func (c CompanyConfig) Enabled() bool { return strings.TrimSpace(c.ID) != "" }

// SetCompanyBlock writes the top-level `company:` block through the same
// yaml.Node round-trip SetUpstreamConnection uses, so every unrelated key and
// comment in the file survives.
func SetCompanyBlock(path string, company CompanyConfig) error {
	if strings.TrimSpace(company.ID) == "" || strings.TrimSpace(company.Slug) == "" {
		return fmt.Errorf("company block needs an id and a slug")
	}
	root, doc, err := loadConfigDoc(path)
	if err != nil {
		return err
	}
	node := mappingValue(doc, "company")
	if node == nil {
		node = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		doc.Content = append(doc.Content, scalarNode("company"), node)
	} else if node.Kind != yaml.MappingNode {
		return fmt.Errorf("config `company` is not a mapping")
	}
	setMappingKey(node, "id", strings.TrimSpace(company.ID))
	setMappingKey(node, "name", strings.TrimSpace(company.Name))
	setMappingKey(node, "slug", strings.TrimSpace(company.Slug))
	setMappingKey(node, "fqdn", strings.TrimSpace(company.FQDN))
	setMappingKey(node, "server", strings.TrimRight(strings.TrimSpace(company.Server), "/"))
	return writeConfigDoc(path, root)
}

// SetSessionName writes `rysh.session_name`. A company workspace names its
// session after the company slug so the desktop app and the terminal — which
// adopt daemons by (directory, session) — land on the same one.
func SetSessionName(path, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("session name is empty")
	}
	root, doc, err := loadConfigDoc(path)
	if err != nil {
		return err
	}
	node := mappingValue(doc, "rysh")
	if node == nil {
		node = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		doc.Content = append(doc.Content, scalarNode("rysh"), node)
	} else if node.Kind != yaml.MappingNode {
		return fmt.Errorf("config `rysh` is not a mapping")
	}
	setMappingKey(node, "session_name", name)
	return writeConfigDoc(path, root)
}

// CompanyBinding is what `##config load <file>` reads out of a downloaded
// rysh.config.yaml (design 037, second cut): the upstream connection that
// carries the read-only key, and the company block that names what it sees.
// Everything else in the file is ignored — a downloaded config binds a
// session to a company; it does not replace the session's own settings.
type CompanyBinding struct {
	URL         string
	APIKey      string
	WorkspaceID string
	Company     CompanyConfig
}

// ReadCompanyBinding parses and validates the file. The checks are the ones
// that would otherwise fail later and quietly: a workspace name where the id
// belongs (a session that connects and sees nothing), a key with the wrong
// shape, a company with no id.
func ReadCompanyBinding(path string) (*CompanyBinding, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Upstream struct {
			URL       string `yaml:"url"`
			APIKey    string `yaml:"api_key"`
			Workspace string `yaml:"workspace"`
		} `yaml:"upstream"`
		Company CompanyConfig `yaml:"company"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("%s: not a rysh config: %w", path, err)
	}
	b := &CompanyBinding{
		URL:         strings.TrimRight(strings.TrimSpace(doc.Upstream.URL), "/"),
		APIKey:      strings.TrimSpace(doc.Upstream.APIKey),
		WorkspaceID: strings.TrimSpace(doc.Upstream.Workspace),
		Company:     doc.Company,
	}
	b.Company.ID = strings.TrimSpace(b.Company.ID)
	b.Company.Name = strings.TrimSpace(b.Company.Name)
	b.Company.Slug = strings.TrimSpace(b.Company.Slug)
	b.Company.FQDN = strings.TrimSpace(b.Company.FQDN)
	b.Company.Server = strings.TrimRight(strings.TrimSpace(b.Company.Server), "/")
	switch {
	case !strings.HasPrefix(b.URL, "http://") && !strings.HasPrefix(b.URL, "https://"):
		return nil, fmt.Errorf("%s: upstream.url must be an http(s) URL", path)
	case b.APIKey == "" || !strings.HasPrefix(b.APIKey, "rysh_"):
		return nil, fmt.Errorf("%s: upstream.api_key is missing or not a rysh key", path)
	case !looksLikeUUID(b.WorkspaceID):
		return nil, fmt.Errorf("%s: upstream.workspace must be the workspace id (a UUID), not its name", path)
	case !looksLikeUUID(b.Company.ID):
		return nil, fmt.Errorf("%s: company.id is missing or not a UUID", path)
	case b.Company.Slug == "":
		return nil, fmt.Errorf("%s: company.slug is missing", path)
	}
	if b.Company.Server == "" {
		b.Company.Server = b.URL
	}
	if b.Company.Name == "" {
		b.Company.Name = b.Company.Slug
	}
	return b, nil
}

func looksLikeUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F') {
				return false
			}
		}
	}
	return true
}
