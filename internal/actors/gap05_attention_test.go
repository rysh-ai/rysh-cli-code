// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/channels"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// gap05 lead review (3): `##attention enable` refused when the only running
// channels were the new built-ins, because its list of external channels did
// not name mattermost, matrix or line.
func TestGap05AttentionEnableAcceptsNewChannels(t *testing.T) {
	for _, channelType := range []string{"telegram", "mattermost", "matrix", "line"} {
		t.Run(channelType, func(t *testing.T) {
			h := &HumanoidActor{
				name:     "att",
				adapters: map[string]channels.ChannelAdapter{channelType: &countingAdapter{}},
			}
			h.handleAttentionEnable(&msg.MsgAttentionEnable{HumanoidName: "att"})
			if !h.attentionEnabled {
				t.Fatalf("attention not enabled with only a %s channel running", channelType)
			}
		})
	}
}

func TestGap05AttentionEnableRefusesWithoutChannels(t *testing.T) {
	h := &HumanoidActor{name: "att", adapters: map[string]channels.ChannelAdapter{}}
	h.handleAttentionEnable(&msg.MsgAttentionEnable{HumanoidName: "att"})
	if h.attentionEnabled {
		t.Fatal("attention enabled with no external channel running")
	}
}
