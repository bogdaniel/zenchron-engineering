package report_test

import (
	"testing"

	"example.com/basic/report"
)

func TestReport(t *testing.T) {
	if report.Report() == "" {
		t.Fatal("empty")
	}
}
