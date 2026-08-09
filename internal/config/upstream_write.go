package config

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// SetUpstreamConnection writes the connection half of the top-level `upstream:`
// block — enabled/url/api_key/workspace — to the YAML config at path via a
// structured yaml.Node round-trip, preserving every unrelated key and the
// user's comments and formatting. It is the sibling of SetUIBlock and
// SetProviderBlock, and it backs `##upstream connect <url> <api-key>`.
//
// workspaceID MUST be the server workspace id (UUID), not the workspace name:
// the id is what namespaces NATS subjects upstream (ws.{id}.*). A name written
// here produces a session that connects and silently sees no shares. Callers
// resolve it from the api key via upstream.FetchWorkspaceInfo rather than
// asking the user to find it in a dashboard.
//
// Deliberately NOT written: `governance`. It opts the daemon into reporting
// local spend to the server, which is a separate data-egress decision from
// connecting (see the UpstreamConfig.Governance comment). Whatever the user
// has — set or absent — is left exactly as it was.
func SetUpstreamConnection(path, url, apiKey, workspaceID string) error {
	if url == "" || apiKey == "" || workspaceID == "" {
		return fmt.Errorf("upstream connection needs url, api_key and workspace id")
	}
	root, doc, err := loadConfigDoc(path)
	if err != nil {
		return err
	}
	up := mappingValue(doc, "upstream")
	if up == nil {
		up = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		doc.Content = append(doc.Content, scalarNode("upstream"), up)
	} else if up.Kind != yaml.MappingNode {
		return fmt.Errorf("config `upstream` is not a mapping")
	}
	// enabled is a bool in UpstreamConfig; a !!str "true" would make the config
	// fail to unmarshal — the same trap setMappingKeyInt exists for.
	setMappingKeyBool(up, "enabled", true)
	setMappingKey(up, "url", url)
	setMappingKey(up, "api_key", apiKey)
	setMappingKey(up, "workspace", workspaceID)
	return writeConfigDoc(path, root)
}

// setMappingKeyBool is setMappingKey for boolean values, emitting a !!bool
// scalar so the value round-trips as a YAML boolean rather than a string.
func setMappingKeyBool(mapNode *yaml.Node, key string, val bool) {
	text := "false"
	if val {
		text = "true"
	}
	if v := mappingValue(mapNode, key); v != nil {
		v.Kind, v.Tag, v.Value = yaml.ScalarNode, "!!bool", text
		return
	}
	mapNode.Content = append(mapNode.Content,
		scalarNode(key),
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: text},
	)
}
