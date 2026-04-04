package report

import (
	"fmt"
	"strings"
)

// FormatHuman returns a human-readable table representation of the report,
// using box-drawing characters for terminal display.
func FormatHuman(report *TopLevelReport) string {
	var b strings.Builder

	writeHeader(&b, report)

	if report.Play != nil {
		writePlayReport(&b, report.Play)
	}
	if report.Push != nil {
		writePushReport(&b, report.Push)
	}
	if report.Auth != nil {
		writeAuthReport(&b, report.Auth)
	}
	if report.Cluster != nil {
		writeClusterReport(&b, report.Cluster)
	}
	if len(report.Errors) > 0 {
		writeErrors(&b, report.Errors)
	}

	return b.String()
}

func writeHeader(b *strings.Builder, r *TopLevelReport) {
	status := "PASS"
	if !r.Pass {
		status = "FAIL"
	}
	b.WriteString("┌──────────────────────────────────────────────────┐\n")
	fmt.Fprintf(b, "│ %-48s │\n", fmt.Sprintf("lf-test: %s [%s]", r.Command, status))
	fmt.Fprintf(b, "│ %-48s │\n", fmt.Sprintf("Duration: %dms", r.DurationMs))
	b.WriteString("└──────────────────────────────────────────────────┘\n")
}

func writePlayReport(b *strings.Builder, p *PlayReport) {
	writeSection(b, "Video")
	writeRow(b, "Codec", fmt.Sprintf("%s (profile=%s, level=%s)", p.Video.Codec, p.Video.Profile, p.Video.Level))
	writeRow(b, "Resolution", p.Video.Resolution)
	writeRow(b, "FPS", fmt.Sprintf("%.2f", p.Video.FPS))
	writeRow(b, "Bitrate", fmt.Sprintf("%.1f kbps", p.Video.BitrateKbps))
	writeRow(b, "Keyframe Interval", fmt.Sprintf("%.1f", p.Video.KeyframeInterval))
	writeRow(b, "DTS Monotonic", fmt.Sprintf("%v", p.Video.DTSMonotonic))
	writeRow(b, "Frame Count", fmt.Sprintf("%d", p.Video.FrameCount))

	writeSection(b, "Audio")
	writeRow(b, "Codec", p.Audio.Codec)
	writeRow(b, "Sample Rate", fmt.Sprintf("%d Hz", p.Audio.SampleRate))
	writeRow(b, "Channels", fmt.Sprintf("%d", p.Audio.Channels))
	writeRow(b, "Bitrate", fmt.Sprintf("%.1f kbps", p.Audio.BitrateKbps))
	writeRow(b, "DTS Monotonic", fmt.Sprintf("%v", p.Audio.DTSMonotonic))
	writeRow(b, "Frame Count", fmt.Sprintf("%d", p.Audio.FrameCount))

	writeSection(b, "AV Sync")
	writeRow(b, "Max Drift", fmt.Sprintf("%.2f ms", p.Sync.MaxDriftMs))
	writeRow(b, "Avg Drift", fmt.Sprintf("%.2f ms", p.Sync.AvgDriftMs))

	if len(p.Stalls) > 0 {
		writeSection(b, fmt.Sprintf("Stalls (%d)", len(p.Stalls)))
		for i, s := range p.Stalls {
			writeRow(b, fmt.Sprintf("#%d", i+1),
				fmt.Sprintf("@%dms gap=%.1fms type=%s", s.TimestampMs, s.GapMs, s.MediaType))
		}
	}

	if p.Error != "" {
		writeSection(b, "Play Error")
		b.WriteString("  " + p.Error + "\n")
	}
}

func writePushReport(b *strings.Builder, p *PushReport) {
	writeSection(b, "Push")
	writeRow(b, "Protocol", p.Protocol)
	writeRow(b, "Target", p.Target)
	writeRow(b, "Duration", fmt.Sprintf("%dms", p.DurationMs))
	writeRow(b, "Frames Sent", fmt.Sprintf("%d", p.FramesSent))
	writeRow(b, "Bytes Sent", fmt.Sprintf("%d", p.BytesSent))
}

func writeAuthReport(b *strings.Builder, a *AuthReport) {
	writeSection(b, fmt.Sprintf("Auth (%d/%d passed)", a.Passed, a.Total))
	for _, c := range a.Cases {
		status := "PASS"
		if !c.Pass {
			status = "FAIL"
		}
		writeRow(b, fmt.Sprintf("[%s] %s/%s", status, c.Protocol, c.Action),
			fmt.Sprintf("expect=%v actual=%v latency=%dms", c.ExpectAllow, c.ActualAllow, c.LatencyMs))
		if c.Error != "" {
			b.WriteString("    error: " + c.Error + "\n")
		}
	}
}

func writeClusterReport(b *strings.Builder, c *ClusterReport) {
	writeSection(b, fmt.Sprintf("Cluster (%s)", c.Topology))
	for _, n := range c.Nodes {
		health := "healthy"
		if !n.Healthy {
			health = "unhealthy"
		}
		writeRow(b, fmt.Sprintf("%s (%s)", n.Name, n.Role), health)
	}
	writeRow(b, "Relay Latency", fmt.Sprintf("%dms", c.RelayMs))
}

func writeErrors(b *strings.Builder, errors []ErrorDetail) {
	writeSection(b, "Errors")
	for _, e := range errors {
		writeRow(b, e.Code, e.Message)
	}
}

func writeSection(b *strings.Builder, title string) {
	b.WriteString("\n├─ " + title + " ")
	remaining := 48 - len(title)
	if remaining > 0 {
		b.WriteString(strings.Repeat("─", remaining))
	}
	b.WriteString("\n")
}

func writeRow(b *strings.Builder, label, value string) {
	fmt.Fprintf(b, "│  %-20s %s\n", label, value)
}
