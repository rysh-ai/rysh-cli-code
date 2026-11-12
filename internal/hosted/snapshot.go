// SPDX-License-Identifier: Apache-2.0

package hosted

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// Snapshot is a named snapshot record (rysh-server hostedrunner.SavedSnapshot):
// a hosted run's home kept under a name, to start new runs from. The server
// keeps the image on its runner host; nothing of it comes to this machine.
type Snapshot struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	SourceRunID string    `json:"source_run_id"`
	CreatedAt   time.Time `json:"created_at"`
	ExpiresAt   time.Time `json:"expires_at"`
}

func (c *Client) snapshotsPath() string {
	return "/api/workspaces/" + url.PathEscape(c.WorkspaceID) + "/hosted-snapshots"
}

// notEnabled turns a 404 into a plain answer about snapshots.
func notEnabled(err error) error {
	var ae *APIError
	if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
		return &APIError{ae.Status, "snapshots are not enabled on this server, or that snapshot or run does not exist or is not yours"}
	}
	return err
}

// SaveSnapshot keeps a running run's home under name.
func (c *Client) SaveSnapshot(ctx context.Context, runID, name string) (Snapshot, error) {
	p, err := c.runPath(runID)
	if err != nil {
		return Snapshot{}, err
	}
	var s Snapshot
	err = c.do(ctx, http.MethodPost, p+"/snapshots", map[string]any{"name": name}, nil, &s)
	return s, notEnabled(err)
}

// ListSnapshots lists this user's snapshots in the workspace.
func (c *Client) ListSnapshots(ctx context.Context) ([]Snapshot, error) {
	var out struct {
		Snapshots []Snapshot `json:"snapshots"`
	}
	err := c.do(ctx, http.MethodGet, c.snapshotsPath(), nil, nil, &out)
	return out.Snapshots, notEnabled(err)
}

// DeleteSnapshot removes one of this user's snapshots.
func (c *Client) DeleteSnapshot(ctx context.Context, id string) error {
	if !runIDRe.MatchString(id) {
		return fmt.Errorf("%q is not a snapshot id", id)
	}
	return notEnabled(c.do(ctx, http.MethodDelete, c.snapshotsPath()+"/"+id, nil, nil, nil))
}
