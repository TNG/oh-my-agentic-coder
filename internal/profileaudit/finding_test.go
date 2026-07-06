package profileaudit

import "testing"

func TestExitCode(t *testing.T) {
	tests := []struct {
		name     string
		findings []Finding
		want     int
	}{
		{"empty", nil, 0},
		{"only-low", []Finding{{Severity: SeverityLow}}, 0},
		{"only-medium", []Finding{{Severity: SeverityMedium}}, 0},
		{"any-high", []Finding{{Severity: SeverityMedium}, {Severity: SeverityHigh}}, 2},
		{"all-high", []Finding{{Severity: SeverityHigh}, {Severity: SeverityHigh}}, 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExitCode(tc.findings); got != tc.want {
				t.Errorf("ExitCode(%v) = %d; want %d", tc.findings, got, tc.want)
			}
		})
	}
}

func TestSeverityOrdering(t *testing.T) {
	if severityRank(SeverityHigh) >= severityRank(SeverityMedium) {
		t.Error("high should rank before medium")
	}
	if severityRank(SeverityMedium) >= severityRank(SeverityLow) {
		t.Error("medium should rank before low")
	}
}
