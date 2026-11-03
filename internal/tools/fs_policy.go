// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"path/filepath"

	"github.com/rysh-ai/rysh-cli-code/internal/policy"
)

// fsPolicyRefusal checks a resolved path against the policy's fs: rules
// (design 068) and returns the tool's refusal, or nil when the access is
// allowed. With no fs: section nothing is ever refused. The refusal is logged
// by policy.CheckFS like other policy refusals.
func fsPolicyRefusal(path string, access policy.FSAccess) *ToolOutput {
	err := policy.CheckFS(path, access)
	if err == nil {
		return nil
	}
	out := ErrOutput(ErrKindPermissionDenied, err.Error())
	out.Metadata = map[string]string{"file_path": path}
	return out
}

// fsPolicyHidden reports whether a path found while walking a directory must
// be left out of a listing or search. It does not log: one refused root is
// logged, the entries it hides are not.
func fsPolicyHidden(path string) bool {
	return policy.ActiveFS().Check(path, policy.FSRead) != nil
}

// bashFSRefusal is the bash pre-check (design 068 S3): the working directory
// and the paths the command text names are checked against the fs: rules.
// It is a guard-rail, not a sandbox — a path built at run time is not seen.
func bashFSRefusal(command, workDir, defaultDir string) *ToolOutput {
	if policy.ActiveFS() == nil {
		return nil
	}
	if !filepath.IsAbs(workDir) {
		workDir = filepath.Join(defaultDir, workDir)
	}
	if out := fsPolicyRefusal(workDir, policy.FSRead); out != nil {
		return out
	}
	if err := policy.CheckBashFS(command, workDir); err != nil {
		return ErrOutput(ErrKindPermissionDenied, err.Error()+
			" (bash pre-check: the command names this path; it is not run)")
	}
	return nil
}
