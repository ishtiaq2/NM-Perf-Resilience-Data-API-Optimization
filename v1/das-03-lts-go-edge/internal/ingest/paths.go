package ingest

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
)

// Field paths name what changed in a delta, with protobuf field names:
// "temperature_c", "optical.rx_dbm", "bands.2.dl_out_dbm", "metrics.m03", "alarms".
// A numeric segment indexes a repeated field; the segment after a map field is a
// map key. A path absent from the delta's state means "now the default value"
// (proto3 JSON omits defaults), so a field that became 0 is still transferred.

// mapFields are the map<> fields of NodeTelemetry.
var mapFields = map[string]bool{"metrics": true}

func camel(seg string) string {
	if !strings.Contains(seg, "_") {
		return seg
	}
	var b strings.Builder
	up := false
	for _, c := range seg {
		switch {
		case c == '_':
			up = true
		case up && c >= 'a' && c <= 'z':
			b.WriteRune(c - 'a' + 'A')
			up = false
		default:
			b.WriteRune(c)
			up = false
		}
	}
	return b.String()
}

func snakeCase(seg string) string {
	var b strings.Builder
	for i, c := range seg {
		if c >= 'A' && c <= 'Z' {
			if i > 0 {
				b.WriteByte('_')
			}
			b.WriteRune(c - 'A' + 'a')
			continue
		}
		b.WriteRune(c)
	}
	return b.String()
}

func isIndex(s string) bool {
	_, err := strconv.Atoi(s)
	return err == nil
}

// jsonPath splits a protobuf-name path into JSON-name segments.
func jsonPath(p string) []string {
	segs := strings.Split(p, ".")
	for i, s := range segs {
		if (i > 0 && mapFields[segs[i-1]]) || isIndex(s) {
			continue
		}
		segs[i] = camel(s)
	}
	return segs
}

// protoPath joins JSON-name segments into a protobuf-name path.
func protoPath(segs []string) string {
	out := make([]string, len(segs))
	for i, s := range segs {
		if (i > 0 && mapFields[segs[i-1]]) || isIndex(s) {
			out[i] = s
		} else {
			out[i] = snakeCase(s)
		}
	}
	return strings.Join(out, ".")
}

// tree is a decoded JSON value: map[string]any, []any, json.Number, string, bool or nil.
func decodeTree(b []byte) (any, error) {
	if len(bytes.TrimSpace(b)) == 0 {
		return map[string]any{}, nil
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber() // numbers keep their exact text
	var v any
	if err := d.Decode(&v); err != nil {
		return nil, err
	}
	if v == nil {
		return map[string]any{}, nil
	}
	return v, nil
}

func toTree(v any) any {
	b, _ := json.Marshal(v)
	t, _ := decodeTree(b)
	return t
}

func clone(t any) any {
	switch x := t.(type) {
	case map[string]any:
		m := make(map[string]any, len(x))
		for k, v := range x {
			m[k] = clone(v)
		}
		return m
	case []any:
		s := make([]any, len(x))
		for i, v := range x {
			s[i] = clone(v)
		}
		return s
	default:
		return t
	}
}

func get(t any, segs []string) (any, bool) {
	for _, s := range segs {
		switch x := t.(type) {
		case map[string]any:
			v, ok := x[s]
			if !ok {
				return nil, false
			}
			t = v
		case []any:
			i, err := strconv.Atoi(s)
			if err != nil || i < 0 || i >= len(x) {
				return nil, false
			}
			t = x[i]
		default:
			return nil, false
		}
	}
	return t, true
}

var errPath = errors.New("path does not fit the state")

// set stores v at segs in t (present=false deletes). Objects on the way are
// created; list indexes must exist (a list whose length changes is sent whole).
func set(t any, segs []string, v any, present bool) error {
	for i, s := range segs {
		last := i == len(segs)-1
		switch x := t.(type) {
		case map[string]any:
			if last {
				if present {
					x[s] = v
				} else {
					delete(x, s)
				}
				return nil
			}
			next, ok := x[s]
			if !ok || next == nil {
				if !present {
					return nil // deleting below something absent: already default
				}
				next = map[string]any{}
				x[s] = next
			}
			t = next
		case []any:
			idx, err := strconv.Atoi(s)
			if err != nil || idx < 0 || idx >= len(x) {
				return errPath
			}
			if last {
				if !present {
					return errPath
				}
				x[idx] = v
				return nil
			}
			t = x[idx]
		default:
			return errPath
		}
	}
	return errPath
}

// Diff lists the paths whose values differ between two trees of the same message.
func Diff(a, b any) []string {
	var out []string
	var walk func(prefix []string, a, b any)
	emit := func(p []string) {
		// A map key containing '.' cannot be addressed: send the whole map instead.
		for i := 1; i < len(p); i++ {
			if mapFields[p[i-1]] && strings.Contains(p[i], ".") {
				p = p[:i]
				break
			}
		}
		s := protoPath(p)
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	walk = func(prefix []string, a, b any) {
		switch av := a.(type) {
		case map[string]any:
			bv, ok := b.(map[string]any)
			if !ok {
				emit(prefix)
				return
			}
			keys := make([]string, 0, len(av)+len(bv))
			for k := range av {
				keys = append(keys, k)
			}
			for k := range bv {
				if _, ok := av[k]; !ok {
					keys = append(keys, k)
				}
			}
			slices.Sort(keys)
			for _, k := range keys {
				x, inA := av[k]
				y, inB := bv[k]
				p := append(slices.Clone(prefix), k)
				if !inA || !inB {
					emit(p)
					continue
				}
				walk(p, x, y)
			}
		case []any:
			bv, ok := b.([]any)
			if !ok || len(av) != len(bv) {
				emit(prefix)
				return
			}
			for i := range av {
				walk(append(slices.Clone(prefix), strconv.Itoa(i)), av[i], bv[i])
			}
		default:
			if a != b { // json.Number, string, bool, nil compare by value
				emit(prefix)
			}
		}
	}
	walk(nil, a, b)
	return out
}

// Prune keeps only the given paths of src (what a delta actually transfers).
// Lists keep their length, with empty objects for untouched elements.
func Prune(src any, paths []string) any {
	dst := map[string]any{}
	for _, p := range paths {
		segs := jsonPath(p)
		v, ok := get(src, segs)
		if !ok {
			continue // absent = default value; the path alone carries the change
		}
		var cur any = dst
		var from any = src
		for i, s := range segs {
			last := i == len(segs)-1
			fromNext, _ := get(from, []string{s})
			switch x := cur.(type) {
			case map[string]any:
				if last {
					x[s] = clone(v)
					break
				}
				next, ok := x[s]
				if !ok {
					if l, isList := fromNext.([]any); isList {
						ph := make([]any, len(l))
						for j := range ph {
							ph[j] = map[string]any{}
						}
						next = ph
					} else {
						next = map[string]any{}
					}
					x[s] = next
				}
				cur = next
			case []any:
				idx, _ := strconv.Atoi(s)
				if last {
					x[idx] = clone(v)
					break
				}
				cur = x[idx]
			}
			from = fromNext
		}
	}
	return dst
}

// Apply writes the listed paths from delta onto base (in place).
func Apply(base, delta any, paths []string) error {
	for _, p := range paths {
		segs := jsonPath(p)
		v, ok := get(delta, segs)
		if err := set(base, segs, clone(v), ok); err != nil {
			return errors.New(p + ": " + err.Error())
		}
	}
	return nil
}
