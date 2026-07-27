package actors

import "testing"

// TestEffectiveVTermDims verifies the mode-aware sizing of the subscriber's
// remote VTerm. In view mode the source PTY is not resized to match the
// subscriber, so the VTerm must follow the source's geometry (otherwise
// line-scrolling and scrollback eviction — and thus copy-mode scroll-up — do
// not reproduce). In control mode the source is resized to the subscriber, so
// the VTerm follows the owner pane's geometry.
func TestEffectiveVTermDims(t *testing.T) {
	tests := []struct {
		name                   string
		mode                   string
		ownerRows, ownerCols   int
		sourceRows, sourceCols int // stored on the actor (last known)
		argRows, argCols       int // freshest dims from the current mode frame
		wantRows, wantCols     int
	}{
		{
			name:      "view prefers fresh source dims over owner dims",
			mode:      "view",
			ownerRows: 50, ownerCols: 200,
			argRows: 24, argCols: 80,
			wantRows: 24, wantCols: 80,
		},
		{
			name:      "view falls back to stored source dims when arg empty",
			mode:      "view",
			ownerRows: 50, ownerCols: 200,
			sourceRows: 30, sourceCols: 100,
			argRows: 0, argCols: 0,
			wantRows: 30, wantCols: 100,
		},
		{
			name:      "view falls back to owner dims when no source dims known",
			mode:      "view",
			ownerRows: 40, ownerCols: 120,
			wantRows: 40, wantCols: 120,
		},
		{
			name:     "view default when nothing known",
			mode:     "view",
			wantRows: 24, wantCols: 80,
		},
		{
			name:      "control prefers owner dims over source dims",
			mode:      "control",
			ownerRows: 50, ownerCols: 200,
			argRows: 24, argCols: 80,
			wantRows: 50, wantCols: 200,
		},
		{
			name:    "control falls back to source dims when owner unknown",
			mode:    "control",
			argRows: 24, argCols: 80,
			wantRows: 24, wantCols: 80,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := &RemoteShareListenerActor{
				mode:       tc.mode,
				ownerRows:  tc.ownerRows,
				ownerCols:  tc.ownerCols,
				sourceRows: tc.sourceRows,
				sourceCols: tc.sourceCols,
			}
			gotRows, gotCols := r.effectiveVTermDims(tc.argRows, tc.argCols)
			if gotRows != tc.wantRows || gotCols != tc.wantCols {
				t.Errorf("effectiveVTermDims(%d,%d) = (%d,%d), want (%d,%d)",
					tc.argRows, tc.argCols, gotRows, gotCols, tc.wantRows, tc.wantCols)
			}
		})
	}
}
