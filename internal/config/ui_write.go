package config

import (
	"fmt"
	"strconv"

	"gopkg.in/yaml.v3"
)

// SetUIBlock writes (or updates) the top-level `ui:` block of the YAML config
// at path via a structured yaml.Node round-trip, preserving every unrelated key
// and existing comments/formatting — never a string splice. It backs the
// terminal-preferences step of `rysh onboard` (design 008 TO2) and is the
// sibling of SetProviderBlock.
//
// Zero/empty arguments leave the corresponding key untouched, so a re-run only
// writes deltas (the wizard's idempotency contract).
func SetUIBlock(path, shell string, initialTabs, initialPanes int) error {
	if shell == "" && initialTabs <= 0 && initialPanes <= 0 {
		return nil // nothing to write — not an error
	}
	root, doc, err := loadConfigDoc(path)
	if err != nil {
		return err
	}
	ui := mappingValue(doc, "ui")
	if ui == nil {
		ui = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		doc.Content = append(doc.Content, scalarNode("ui"), ui)
	} else if ui.Kind != yaml.MappingNode {
		return fmt.Errorf("config `ui` is not a mapping")
	}
	if shell != "" {
		setMappingKey(ui, "shell", shell)
	}
	// initial_tabs / initial_panes are ints in ryshUI; writing them as !!str
	// would make the config fail to unmarshal, so they need an int-tagged node.
	if initialTabs > 0 {
		setMappingKeyInt(ui, "initial_tabs", initialTabs)
	}
	if initialPanes > 0 {
		setMappingKeyInt(ui, "initial_panes", initialPanes)
	}
	return writeConfigDoc(path, root)
}

// setMappingKeyInt is setMappingKey for integer values, emitting an !!int
// scalar so the value round-trips as a YAML number.
func setMappingKeyInt(mapNode *yaml.Node, key string, val int) {
	text := strconv.Itoa(val)
	if v := mappingValue(mapNode, key); v != nil {
		v.Kind, v.Tag, v.Value = yaml.ScalarNode, "!!int", text
		return
	}
	mapNode.Content = append(mapNode.Content,
		scalarNode(key),
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: text},
	)
}
