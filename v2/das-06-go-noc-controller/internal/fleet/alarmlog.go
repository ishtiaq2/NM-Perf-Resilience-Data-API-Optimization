package fleet

import (
	"encoding/json"
	"sync"
)

// alarmLog is the fleet-wide ring of recent alarm events. Dashboards read it with
// a cursor; a dashboard that falls further behind than the ring holds gets a gap
// marker and reloads the active alarms instead of an unbounded backlog.
//
// Each event is encoded to JSON once, when it is appended: twenty dashboards
// receiving a storm of 20 000 events then concatenate ready-made bytes instead
// of encoding 400 000 events.
type alarmLog struct {
	mu    sync.Mutex
	buf   []logEntry
	next  uint64 // N of the next event; events carry N >= 1
	start int    // index of the oldest event in buf
	n     int    // number of events held
}

type logEntry struct {
	ev  Event
	raw json.RawMessage
}

func newAlarmLog(capacity int) *alarmLog {
	if capacity < 16 {
		capacity = 16
	}
	return &alarmLog{buf: make([]logEntry, capacity), next: 1}
}

func (l *alarmLog) append(ev Event) Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	ev.N = l.next
	l.next++
	raw, _ := json.Marshal(&ev)
	e := logEntry{ev: ev, raw: raw}
	if l.n < len(l.buf) {
		l.buf[(l.start+l.n)%len(l.buf)] = e
		l.n++
	} else {
		l.buf[l.start] = e
		l.start = (l.start + 1) % len(l.buf)
	}
	return ev
}

// last returns the N of the newest event (0 when empty).
func (l *alarmLog) last() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.next - 1
}

// scan visits up to max entries with N > after that match keep; it returns the
// cursor to continue from and whether events after `after` were overwritten.
func (l *alarmLog) scan(after uint64, max int, keep func(*Event) bool, visit func(*logEntry)) (cursor uint64, gap bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	cursor = after
	if l.n == 0 {
		return cursor, false
	}
	oldest := l.buf[l.start].ev.N
	if after+1 < oldest {
		gap = true
		after = oldest - 1
		cursor = after
	}
	count := 0
	for i := int(after + 1 - oldest); i < l.n; i++ {
		e := &l.buf[(l.start+i)%len(l.buf)]
		cursor = e.ev.N
		if keep == nil || keep(&e.ev) {
			visit(e)
			if count++; count >= max {
				break
			}
		}
	}
	return cursor, gap
}

func (l *alarmLog) since(after uint64, max int, keep func(*Event) bool) (out []Event, cursor uint64, gap bool) {
	cursor, gap = l.scan(after, max, keep, func(e *logEntry) { out = append(out, e.ev) })
	return out, cursor, gap
}

func (l *alarmLog) sinceRaw(after uint64, max int, keep func(*Event) bool) (out []json.RawMessage, cursor uint64, gap bool) {
	cursor, gap = l.scan(after, max, keep, func(e *logEntry) { out = append(out, e.raw) })
	return out, cursor, gap
}
