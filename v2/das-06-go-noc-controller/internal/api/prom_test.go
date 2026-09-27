package api

import (
	"regexp"
	"strings"
	"testing"
)

func TestPromTextTypesAndLabels(t *testing.T) {
	doc := map[string]any{
		"fleet": map[string]any{
			"connected": 4999, "summaries": 12,
			"byStatus": map[string]int{"ok": 4990, "critical": 9},
			"resync":   map[string]uint64{"summary": 1, "alarms": 0},
		},
		"uplink":  map[string]any{"active": 4999, "bytesIn": 1234, "rejected": map[string]uint64{"auth": 3, "admission": 40}},
		"process": map[string]any{"heapMB": 36.5, "cpuSeconds": 12.25, "goVersion": "go1.24"},
	}
	out := string(promText(doc))
	for _, want := range []string{
		"# TYPE das_noc_fleet_connected gauge\ndas_noc_fleet_connected 4999\n",
		"# TYPE das_noc_fleet_summaries_total counter\ndas_noc_fleet_summaries_total 12\n",
		`das_noc_sites{status="critical"} 9`,
		"# TYPE das_noc_uplink_rejected_total counter\n",
		`das_noc_uplink_rejected_total{reason="admission"} 40`,
		`das_noc_fleet_resync_requests_total{what="summary"} 1`,
		"das_noc_uplink_bytes_in_total 1234\n",
		"das_noc_process_heap_mb 36.5\n",
		"das_noc_process_cpu_seconds_total 12.25\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	sample := regexp.MustCompile(`^[a-z_][a-z0-9_]*(\{[a-z_]+="[a-z0-9_-]+"\})? -?[0-9.e+]+$`)
	typed := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if name, ok := strings.CutPrefix(line, "# TYPE "); ok {
			if typed[name] {
				t.Errorf("duplicate TYPE line %q", line)
			}
			typed[name] = true
			continue
		}
		if !sample.MatchString(line) {
			t.Errorf("not a valid sample line: %q", line)
		}
	}
}
