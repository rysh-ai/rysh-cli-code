package config

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// SetProviderBlock writes (or updates) the top-level `provider:` block of the
// YAML config at path via a structured yaml.Node round-trip, preserving every
// unrelated key and existing comments/formatting — never a string splice. It
// backs `rysh onboard` step 1 (design 004 §4.1).
//
// apiKeyRef is expected to be a ${ENV} secret REFERENCE (e.g.
// "${ANTHROPIC_API_KEY}"), never a literal key — the caller stores literals in
// the .rysh/secrets tier (WriteSecret) or the environment. Empty model /
// apiKeyRef / apiURL leave the corresponding key untouched, so a re-run only
// writes deltas (the wizard's idempotency contract).
func SetProviderBlock(path, name, model, apiKeyRef, apiURL string) error {
	if name == "" {
		return fmt.Errorf("provider name is required")
	}
	root, doc, err := loadConfigDoc(path)
	if err != nil {
		return err
	}
	prov := mappingValue(doc, "provider")
	if prov == nil {
		prov = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		doc.Content = append(doc.Content, scalarNode("provider"), prov)
	} else if prov.Kind != yaml.MappingNode {
		return fmt.Errorf("config `provider` is not a mapping")
	}
	setMappingKey(prov, "name", name)
	if model != "" {
		setMappingKey(prov, "model", model)
	}
	if apiKeyRef != "" {
		setMappingKey(prov, "api_key", apiKeyRef)
	}
	if apiURL != "" {
		setMappingKey(prov, "api_url", apiURL)
	}
	return writeConfigDoc(path, root)
}
