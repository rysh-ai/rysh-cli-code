// SPDX-License-Identifier: Apache-2.0

package config

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// SetEmailProvider persists only the transport selector and preserves every
// other config key/comment. Credentials are deliberately not accepted here.
func SetEmailProvider(path, provider string) error {
	if provider != "imap-smtp" && provider != "mailgun" {
		return fmt.Errorf("email provider must be imap-smtp or mailgun")
	}
	root, doc, err := loadConfigDoc(path)
	if err != nil {
		return err
	}
	email := mappingValue(doc, "email")
	if email == nil {
		email = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		doc.Content = append(doc.Content, scalarNode("email"), email)
	} else if email.Kind != yaml.MappingNode {
		return fmt.Errorf("config `email` is not a mapping")
	}
	setMappingKey(email, "provider", provider)
	return writeConfigDoc(path, root)
}
