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
				p := k[i-1]
				if (p >= 'a' && p <= 'z') || (p >= '0' && p <= '9') {
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

// promText renders the /api/metrics document in the Prometheus text format:
// every numeric leaf becomes a gauge named after its path (das_edge_runtime_heap_mb),
// route counters become labelled series.
func promText(doc map[string]any) []byte {
	var flat map[string]any
	raw, _ := json.Marshal(doc)
	_ = json.Unmarshal(raw, &flat)
	var lines []string
	if edge, ok := flat["edge"].(map[string]any); ok {
		if routes, ok := edge["routes"].(map[string]any); ok {
			delete(edge, "routes")
			names := make([]string, 0, len(routes))
			for k := range routes {
				names = append(names, k)
			}
			slices.Sort(names)
			for _, series := range []struct{ metric, field string }{
				{"das_http_requests_total", "requests"}, {"das_http_not_modified_total", "notModified"},
				{"das_http_errors_total", "errors"}, {"das_http_response_bytes_total", "bytes"},
			} {
				lines = append(lines, "# TYPE "+series.metric+" counter")
				for _, n := range names {
					if m, ok := routes[n].(map[string]any); ok {
						v, _ := m[series.field].(float64)
						lines = append(lines, series.metric+`{route="`+n+`"} `+strconv.FormatFloat(v, 'f', -1, 64))
					}
				}
			}
		}
	}
	var walk func(prefix string, v any)
	walk = func(prefix string, v any) {
		switch x := v.(type) {
		case map[string]any:
			keys := make([]string, 0, len(x))
			for k := range x {
				keys = append(keys, k)
			}
			slices.Sort(keys)
			for _, k := range keys {
				walk(prefix+"_"+snake(k), x[k])
			}
		case float64:
			lines = append(lines, "# TYPE "+prefix+" gauge", prefix+" "+strconv.FormatFloat(x, 'f', -1, 64))
		case bool:
			n := "0"
			if x {
				n = "1"
			}
			lines = append(lines, "# TYPE "+prefix+" gauge", prefix+" "+n)
		}
	}
	walk("das", flat)
	return []byte(strings.Join(lines, "\n") + "\n")
}
