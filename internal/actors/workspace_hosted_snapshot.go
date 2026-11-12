// SPDX-License-Identifier: Apache-2.0

package actors

// ##hosted snapshot — named snapshots and templates of hosted runs (Boat's
// saved snapshots, built into rysh). A snapshot keeps a running run's home
// (the session and its files; never its host key, its laptop key, its
// environment or its model key) on the server's runner host under a name;
// `##hosted up --from <snapshot-id>` starts a new run from it. Only the
// person who saved a snapshot sees, uses or deletes it.
//
//	##hosted snapshot [run-id] --name N
//	##hosted snapshots
//	##hosted snapshot delete <snapshot-id>
//	##hosted up --from <snapshot-id> [...]

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"
)

const hostedSnapshotUsage = "##hosted snapshot [run-id] --name N | ##hosted snapshots | ##hosted snapshot delete <snapshot-id> | ##hosted up --from <snapshot-id>"

var snapshotNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

type hostedSnapshotOpts struct {
	Run  string
	Name string
}

func parseHostedSnapshot(args []string) (hostedSnapshotOpts, error) {
	var o hostedSnapshotOpts
	for len(args) > 0 {
		a := args[0]
		switch {
		case a == "--name":
			if len(args) < 2 {
				return o, fmt.Errorf("--name needs a value; usage: %s", hostedSnapshotUsage)
			}
			if !snapshotNameRE.MatchString(args[1]) {
				return o, fmt.Errorf("--name: a-z, 0-9 and dash, up to 32")
			}
			o.Name = args[1]
			args = args[2:]
		case strings.HasPrefix(a, "-"):
			return o, fmt.Errorf("unknown flag %s; usage: %s", a, hostedSnapshotUsage)
		case o.Run != "":
			return o, fmt.Errorf("one run id only; usage: %s", hostedSnapshotUsage)
		default:
			o.Run = a
			args = args[1:]
		}
	}
	if o.Name == "" {
		return o, fmt.Errorf("a snapshot needs --name; usage: %s", hostedSnapshotUsage)
	}
	return o, nil
}

// hostedSnapshotCommand implements ##hosted snapshot and ##hosted snapshots.
func (w *WorkspaceActor) hostedSnapshotCommand(ctx context.Context, out *strings.Builder, sub string, args []string) error {
	o := ryshWriter(out)
	api, st, err := w.hostedTools()
	if err != nil {
		return err
	}
	if sub == "snapshots" {
		list, err := api.ListSnapshots(ctx)
		if err != nil {
			return err
		}
		o.Header("hosted snapshots")
		if len(list) == 0 {
			o.Row("none — ##hosted snapshot [run-id] --name N")
			return nil
		}
		o.Row("%-18s %-20s %-18s %s", "ID", "NAME", "FROM RUN", "KEPT UNTIL")
		for _, s := range list {
			o.Row("%-18s %-20s %-18s %s", s.ID, s.Name, s.SourceRunID, s.ExpiresAt.Format(time.RFC3339))
		}
		return nil
	}
	if len(args) > 0 && args[0] == "delete" {
		if len(args) != 2 {
			o.Usage(hostedSnapshotUsage)
			return fmt.Errorf("##hosted snapshot delete needs one snapshot id")
		}
		if err := api.DeleteSnapshot(ctx, args[1]); err != nil {
			return err
		}
		fmt.Fprintf(out, "\n[rysh] hosted snapshot %s deleted\n", args[1])
		return nil
	}
	opts, err := parseHostedSnapshot(args)
	if err != nil {
		o.Usage(hostedSnapshotUsage)
		return err
	}
	run, err := w.hostedTarget(ctx, api, st, opts.Run)
	if err != nil {
		return err
	}
	s, err := api.SaveSnapshot(ctx, run, opts.Name)
	if err != nil {
		return err
	}
	o.Header("hosted snapshot saved")
	o.Row("id      %s", s.ID)
	o.Row("name    %s (of run %s)", s.Name, run)
	o.Row("kept    until %s (##hosted snapshot delete %s)", s.ExpiresAt.Format(time.RFC3339), s.ID)
	o.Row("use     ##hosted up --from %s", s.ID)
	return nil
}
