// SPDX-License-Identifier: Apache-2.0

package peercoord

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// CheckRunner uses provider-supported status commands. It never reads or copies
// login files and never includes authentication output in peer messages.
func CheckRunner(ctx context.Context, runner string) error {
	path, e := exec.LookPath(runner)
	if e != nil {
		return fmt.Errorf("%s is not installed", runner)
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var args []string
	switch runner {
	case "claude":
		args = []string{"auth", "status"}
	case "codex":
		args = []string{"login", "status"}
	default:
		return fmt.Errorf("unsupported runner")
	}
	data, e := exec.CommandContext(ctx, path, args...).CombinedOutput()
	if e != nil {
		return fmt.Errorf("%s login check failed; authenticate it locally", runner)
	}
	if runner == "claude" {
		var status struct {
			LoggedIn bool `json:"loggedIn"`
		}
		if json.Unmarshal(data, &status) != nil || !status.LoggedIn {
			return fmt.Errorf("claude is not logged in")
		}
	}
	if runner == "codex" && !strings.Contains(strings.ToLower(string(data)), "logged in") {
		return fmt.Errorf("codex login could not be verified")
	}
	return nil
}
