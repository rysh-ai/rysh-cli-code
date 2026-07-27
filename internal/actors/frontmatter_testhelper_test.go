package actors

import (
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"

	"gopkg.in/yaml.v3"
)

func parseHumanoidFileFromString(content string) (*humanoidDefinition, error) {
	fmRaw, body, ok := splitFrontmatter(content)
	if !ok {
		return &humanoidDefinition{SystemPrompt: strings.TrimSpace(content)}, nil
	}
	var fm humanoidFrontmatter
	if err := yaml.Unmarshal([]byte(fmRaw), &fm); err != nil {
		return nil, err
	}
	resolved := make(map[string]msg.ChannelConfig, len(fm.Contacts))
	for ct, cc := range fm.Contacts {
		cc = resolveChannelEnvVars(cc, envOnlyExpand)
		resolved[ct] = cc
	}
	return &humanoidDefinition{Name: fm.Name, SystemPrompt: strings.TrimSpace(body), Contacts: resolved}, nil
}
