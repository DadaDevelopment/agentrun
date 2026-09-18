package apiclient

import "testing"

// TestOperationTerminalStatuses pins ddc to the platform's own definition in
// classifyOperationStatus: the gitops agent ends an agent write at Committed
// and never advances that row, so treating only Ready as finished made a
// successful deploy hang until the timeout.
func TestOperationTerminalStatuses(t *testing.T) {
	cases := []struct {
		status string
		done   bool
		ok     bool
	}{
		{"Committed", true, true},
		{"Ready", true, true},
		{"Failed", true, false},
		{"Cancelled", true, false},
		{"Created", false, false},
		{"Queued", false, false},
		{"Rendering", false, false},
		{"CommittingToGit", false, false},
		{"WaitingForArgoSync", false, false},
		{"WaitingForApproval", false, false},
	}
	for _, tc := range cases {
		op := Operation{Status: tc.status}
		if op.Done() != tc.done {
			t.Errorf("%s: Done() = %v, want %v", tc.status, op.Done(), tc.done)
		}
		if op.OK() != tc.ok {
			t.Errorf("%s: OK() = %v, want %v", tc.status, op.OK(), tc.ok)
		}
	}
}
