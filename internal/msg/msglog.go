// SPDX-License-Identifier: Apache-2.0

package msg

import sharedmsg "github.com/rysh-ai/rysh-cli-shared/msg"

// MessageLogger and its helpers are defined in rysh-shared/msg.
// Re-exported here so existing imports of rysh/internal/msg continue to work.

type MessageLogger = sharedmsg.MessageLogger

var InitMsgLog = sharedmsg.InitMsgLog
var MsgLog = sharedmsg.MsgLog
