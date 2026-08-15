// SPDX-License-Identifier: Apache-2.0

// Package msg proxy_audit_aliases.go — re-exports rysh-shared/msg governance
// proxy audit types (design 001 §4.5) so rysh-cli references them as
// msg.MsgProxyRequestAudit etc. The codec is registered in rysh-shared's
// DefaultCodecRegistry, which the CLI registry is built from, so no CLI-side
// codec registration is needed (mirrors usage_aliases.go).
package msg

import sharedmsg "github.com/rysh-ai/rysh-cli-shared/msg"

type (
	MsgProxyRequestAudit         = sharedmsg.MsgProxyRequestAudit
	MsgProxyAuditSnapshotRequest = sharedmsg.MsgProxyAuditSnapshotRequest
	MsgProxyAuditSnapshotReply   = sharedmsg.MsgProxyAuditSnapshotReply
)

const (
	TagProxyRequestAudit         = sharedmsg.TagProxyRequestAudit
	TagProxyAuditSnapshotRequest = sharedmsg.TagProxyAuditSnapshotRequest
	TagProxyAuditSnapshotReply   = sharedmsg.TagProxyAuditSnapshotReply

	ProxyBudgetOK          = sharedmsg.ProxyBudgetOK
	ProxyBudgetExceeded    = sharedmsg.ProxyBudgetExceeded
	ProxyBudgetRateLimited = sharedmsg.ProxyBudgetRateLimited
	ProxyBlocked           = sharedmsg.ProxyBlocked
)

var (
	ProxyAuditSubject         = sharedmsg.ProxyAuditSubject
	ProxyAuditWildcardSubject = sharedmsg.ProxyAuditWildcardSubject
	ProxyAuditInboxSubject    = sharedmsg.ProxyAuditInboxSubject
)
