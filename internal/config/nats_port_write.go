// SPDX-License-Identifier: Apache-2.0

package config

import (
	"strconv"

	"gopkg.in/yaml.v3"
)

// SetNATSPortIfUnset writes nats.port into the config at path unless the
// config already has a nats.port key (any value, 0 included). It reports
// whether it wrote. D-55 B: `rysh setup` gives a new project a bus port of
// its own; an existing choice is never changed.
func SetNATSPortIfUnset(path string, port int) (bool, error) {
	root, doc, err := loadConfigDoc(path)
	if err != nil {
		return false, err
	}
	node := mappingValue(doc, "nats")
	if node == nil {
		node = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		doc.Content = append(doc.Content, scalarNode("nats"), node)
	} else if node.Kind != yaml.MappingNode {
		return false, nil // not ours to rewrite
	}
	if mappingValue(node, "port") != nil {
		return false, nil
	}
	key := scalarNode("port")
	key.HeadComment = "this project's own bus (rysh setup, D-55): its daemon never joins another project's"
	node.Content = append(node.Content, key, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: strconv.Itoa(port)})
	return true, writeConfigDoc(path, root)
}
