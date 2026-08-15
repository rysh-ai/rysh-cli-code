// SPDX-License-Identifier: Apache-2.0

// Package msg usage_aliases.go — re-exports rysh-shared/msg usage-ledger types
// (design 003) so rysh-cli actors reference them as msg.MsgUsageRecord etc.
// The codecs are registered in rysh-shared's DefaultCodecRegistry, which the
// CLI registry is built from, so no CLI-side codec registration is needed.
package msg

import sharedmsg "github.com/rysh-ai/rysh-cli-shared/msg"

type MsgUsageRecord = sharedmsg.MsgUsageRecord
type MsgUsageCheck = sharedmsg.MsgUsageCheck
type MsgUsageCheckReply = sharedmsg.MsgUsageCheckReply
type MsgUsageSnapshotRequest = sharedmsg.MsgUsageSnapshotRequest
type MsgUsageSnapshotReply = sharedmsg.MsgUsageSnapshotReply
type MsgUsageBudgetSet = sharedmsg.MsgUsageBudgetSet
type UsageAgg = sharedmsg.UsageAgg
type UsageCeiling = sharedmsg.UsageCeiling

const (
	TagUsageRecord          = sharedmsg.TagUsageRecord
	TagUsageCheck           = sharedmsg.TagUsageCheck
	TagUsageCheckReply      = sharedmsg.TagUsageCheckReply
	TagUsageSnapshotRequest = sharedmsg.TagUsageSnapshotRequest
	TagUsageSnapshotReply   = sharedmsg.TagUsageSnapshotReply
	TagUsageBudgetSet       = sharedmsg.TagUsageBudgetSet

	UsageSourceAgent = sharedmsg.UsageSourceAgent
	UsageSourceProxy = sharedmsg.UsageSourceProxy
	UsageSourceEval  = sharedmsg.UsageSourceEval
	UsageSourceCI    = sharedmsg.UsageSourceCI

	UsageScopePane   = sharedmsg.UsageScopePane
	UsageScopeTenant = sharedmsg.UsageScopeTenant
)

var (
	UsageSubject         = sharedmsg.UsageSubject
	UsageWildcardSubject = sharedmsg.UsageWildcardSubject
	UsageCheckSubject    = sharedmsg.UsageCheckSubject
	UsageInboxSubject    = sharedmsg.UsageInboxSubject
)
