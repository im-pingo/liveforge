package report

import "encoding/json"

// FormatJSON serializes the report as indented JSON.
func FormatJSON(report *TopLevelReport) ([]byte, error) {
	return json.MarshalIndent(report, "", "  ")
}
