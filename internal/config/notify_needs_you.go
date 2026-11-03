// SPDX-License-Identifier: Apache-2.0

package config

import (
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// NotifyNeedsYouConfig is the daemon-side "needs you" notifier (gap02 / G6,
// internal/needsyou). It is opt-in: nothing is sent unless Webhook, Command
// or Assistant is set.
//
//	notify:
//	  needs_you:
//	    after: 60s        # still needing you this long → alert (0 = at once)
//	    cooldown: 5m      # at most one alert per pane per cooldown
//	    done_after: 3m    # a run this long that ends → "done" alert (0/unset = off)
//	    webhook: ${RYSH_NOTIFY_URL}            # POST a JSON document
//	    command: ["ntfy", "publish", "topic"]  # argv, no shell
//	    assistant: assistant                   # tell the owner over their DM with this humanoid (E33, design 086)
type NotifyNeedsYouConfig struct {
	After    time.Duration
	Cooldown time.Duration
	// DoneAfter turns on "task done" alerts for runs at least this long; 0 is
	// off (gap02 slice 2).
	DoneAfter time.Duration
	// Webhook is kept as written: a ${NAME} reference is resolved when the
	// daemon starts, so the URL's secret never sits in the config.
	Webhook string
	Command []string
	// Assistant names a humanoid that tells its owner, over the owner's own
	// DM with the bot (E33 slice E1, design 086).
	Assistant string
	// Problems are values that were ignored (a bad duration). The daemon
	// logs them and `rysh notify test` prints them; neither stops anything.
	Problems []string
}

// Enabled reports whether any sink is configured.
func (n NotifyNeedsYouConfig) Enabled() bool {
	return strings.TrimSpace(n.Webhook) != "" || len(n.Command) > 0 || n.Assistant != ""
}

func defaultNotifyNeedsYou() NotifyNeedsYouConfig {
	return NotifyNeedsYouConfig{After: 60 * time.Second, Cooldown: 5 * time.Minute}
}

type ryshNotify struct {
	NeedsYou ryshNotifyNeedsYou `yaml:"needs_you"`
}

type ryshNotifyNeedsYou struct {
	After     string   `yaml:"after"`
	Cooldown  string   `yaml:"cooldown"`
	DoneAfter string   `yaml:"done_after"`
	Webhook   string   `yaml:"webhook"`
	Command   []string `yaml:"command"`
	Assistant string   `yaml:"assistant"`
}

func applyNotifyNeedsYou(n *NotifyNeedsYouConfig, f ryshNotifyNeedsYou) {
	dur := func(key, v string, dst *time.Duration) {
		v = strings.TrimSpace(v)
		if v == "" {
			return
		}
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 {
			n.Problems = append(n.Problems, fmt.Sprintf("notify.needs_you.%s: %q is not a duration; using %s", key, v, *dst))
			return
		}
		*dst = d
	}
	dur("after", f.After, &n.After)
	dur("cooldown", f.Cooldown, &n.Cooldown)
	dur("done_after", f.DoneAfter, &n.DoneAfter)
	if w := strings.TrimSpace(f.Webhook); w != "" {
		n.Webhook = w
	}
	if len(f.Command) > 0 && strings.TrimSpace(f.Command[0]) != "" {
		n.Command = append([]string(nil), f.Command...)
	}
	if a := strings.TrimSpace(f.Assistant); a != "" {
		n.Assistant = a
	}
}

// SetNeedsYouAssistantIfUnset writes `notify.needs_you.assistant: <name>` into
// the config at path, keeping its comments, unless a `notify.needs_you` block
// is already there (E33 Q-E33-2: `rysh setup` turns the chat alert on for a
// new assistant and never changes a needs-you choice the owner already made).
// It reports whether it wrote.
func SetNeedsYouAssistantIfUnset(path, name string) (bool, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return false, fmt.Errorf("humanoid name is empty")
	}
	root, doc, err := loadConfigDoc(path)
	if err != nil {
		return false, err
	}
	notify := mappingValue(doc, "notify")
	if notify == nil {
		notify = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		doc.Content = append(doc.Content, scalarNode("notify"), notify)
	} else if notify.Kind != yaml.MappingNode {
		return false, fmt.Errorf("config `notify` is not a mapping")
	}
	if mappingValue(notify, "needs_you") != nil {
		return false, nil
	}
	needsYou := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	setMappingKey(needsYou, "assistant", name)
	notify.Content = append(notify.Content, scalarNode("needs_you"), needsYou)
	return true, writeConfigDoc(path, root)
}
