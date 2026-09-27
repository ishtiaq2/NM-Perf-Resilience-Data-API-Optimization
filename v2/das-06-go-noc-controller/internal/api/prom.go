package api

import (
	"encoding/json"
	"slices"
	"strconv"
	"strings"
)

// snake converts a JSON key to a Prometheus name segment: heapMB -> heap_mb.
func snake(k string) string {
	var b strings.Builder
	for i, c := range k {
		switch {
		case c >= 'A' && c <= 'Z':
			if i > 0 {
				if p := k[i-1]; (p >= 'a' && p <= 'z') || (p >= '0' && p <= '9') {
					b.WriteByte('_')
				}
			}
			b.WriteRune(c + 'a' - 'A')
		case (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_':
			b.WriteRune(c)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// counters are the cumulative values of the metrics document (by metric name
// before the _total suffix); every other number is a gauge.
var counters = map[string]bool{
	"das_noc_fleet_summaries": true, "das_noc_fleet_deltas": true, "das_noc_fleet_alarm_messages": true,
	"das_noc_fleet_alarm_events": true, "das_noc_fleet_duplicate_events": true, "das_noc_fleet_detail_messages": true,
	"das_noc_fleet_hash_mismatch": true, "das_noc_fleet_takeovers": true,
	"das_noc_uplink_handshakes": true, "das_noc_uplink_accepted": true, "das_noc_uplink_messages": true,
	"das_noc_uplink_bad_messages": true, "das_noc_uplink_replaced": true, "das_noc_uplink_pings": true,
	"das_noc_uplink_bytes_in": true, "das_noc_uplink_bytes_out": true,
	"das_noc_dashboard_messages": true, "das_noc_dashboard_bytes": true, "das_noc_dashboard_overview_messages": true,
	"das_noc_dashboard_alarm_messages": true, "das_noc_dashboard_site_messages": true, "das_noc_dashboard_events": true,
	"das_noc_dashboard_gaps": true, "das_noc_dashboard_rejected": true, "das_noc_dashboard_wakeups": true,
	"das_noc_process_cpu_seconds": true, "das_noc_process_gc_cycles": true,
}

// labelled maps of the document become one metric with a label.
var labelled = []struct{ section, field, metric, label, kind string }{
	{"fleet", "byStatus", "das_noc_sites", "status", "gauge"},
	{"fleet", "alarms", "das_noc_active_alarms", "severity", "gauge"},
	{"fleet", "resync", "das_noc_fleet_resync_requests_total", "what", "counter"},
	{"uplink", "rejected", "das_noc_uplink_rejected_total", "reason", "counter"},
}

// promText renders the metrics document in the Prometheus text format: numeric
// leaves are named after their path (das_noc_fleet_connected), cumulative ones
// are counters with the _total suffix, and the per-status, per-severity and
// per-reason maps become labelled series.
func promText(doc map[string]any) []byte {
	var flat map[string]any
	raw, _ := json.Marshal(doc)
	_ = json.Unmarshal(raw, &flat)
	num := func(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }
	var lines []string
	for _, l := range labelled {
		sec, _ := flat[l.section].(map[string]any)
		m, ok := sec[l.field].(map[string]any)
		if !ok {
			continue
		}
		delete(sec, l.field)
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		lines = append(lines, "# TYPE "+l.metric+" "+l.kind)
		for _, k := range keys {
			v, _ := m[k].(float64)
			lines = append(lines, l.metric+`{`+l.label+`="`+k+`"} `+num(v))
		}
	}
	var walk func(name string, v any)
	walk = func(name string, v any) {
		switch x := v.(type) {
		case map[string]any:
			keys := make([]string, 0, len(x))
			for k := range x {
				keys = append(keys, k)
			}
			slices.Sort(keys)
			for _, k := range keys {
				walk(name+"_"+snake(k), x[k])
			}
		case float64:
			if counters[name] {
				lines = append(lines, "# TYPE "+name+"_total counter", name+"_total "+num(x))
			} else {
				lines = append(lines, "# TYPE "+name+" gauge", name+" "+num(x))
			}
		}
	}
	walk("das_noc", flat)
	return []byte(strings.Join(lines, "\n") + "\n")
}
