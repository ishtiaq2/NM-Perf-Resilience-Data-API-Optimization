// Package api is the NOC's HTTP surface: the REST API for dashboards and
// integrations (tenant-scoped by the caller's token), the WebSocket endpoints
// (Master Unit uplink, dashboard push), the embedded NOC page, and the internal
// health and metrics endpoints.
package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/pprof"
	"strconv"
	"strings"
	"time"

	"dasnoc/internal/dash"
	"dasnoc/internal/fleet"
	"dasnoc/internal/proto"
	"dasnoc/internal/uplink"
	"dasnoc/internal/web"
)

// Server wires the handlers.
type Server struct {
	Store   *fleet.Store
	Uplink  *uplink.Server
	Hub     *dash.Hub
	Verify  *proto.Verifier
	Log     *slog.Logger
	Version string
	Started time.Time
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func errorJSON(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"error": code, "message": msg})
}

// user authenticates a request with a user token (Authorization: Bearer ...).
func (s *Server) user(w http.ResponseWriter, r *http.Request) (proto.Claims, bool) {
	c, err := s.Verify.Verify(proto.TokenFrom(r.Header.Get("Authorization"), nil))
	if err != nil || c.Kind != proto.KindUser {
		w.Header().Set("WWW-Authenticate", `Bearer realm="das-noc"`)
		errorJSON(w, http.StatusUnauthorized, "UNAUTHORIZED", "user token required")
		return proto.Claims{}, false
	}
	return c, true
}

func intParam(r *http.Request, name string, def int) int {
	if v, err := strconv.Atoi(r.URL.Query().Get(name)); err == nil && v >= 0 {
		return v
	}
	return def
}

// Handler is the public listener's handler (behind the ingress).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /uplink/v1", s.Uplink)
	mux.Handle("GET /api/ws/dashboard", s.Hub)

	mux.HandleFunc("GET /api/fleet/summary", func(w http.ResponseWriter, r *http.Request) {
		if c, ok := s.user(w, r); ok {
			writeJSON(w, http.StatusOK, s.Store.Overview(c))
		}
	})
	mux.HandleFunc("GET /api/sites", func(w http.ResponseWriter, r *http.Request) {
		c, ok := s.user(w, r)
		if !ok {
			return
		}
		q := r.URL.Query()
		f := fleet.SiteFilter{Tenant: q.Get("tenant"), Query: q.Get("q"), Sort: q.Get("sort"), Offset: intParam(r, "offset", 0), Limit: intParam(r, "limit", 100)}
		for _, name := range strings.Split(q.Get("status"), ",") {
			if st, ok := fleet.ParseStatus(strings.TrimSpace(name)); ok {
				f.Statuses = append(f.Statuses, st)
			}
		}
		items, total := s.Store.ListSites(c, f)
		if items == nil {
			items = []fleet.SiteView{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"total": total, "offset": f.Offset, "items": items})
	})
	mux.HandleFunc("GET /api/sites/{id}", func(w http.ResponseWriter, r *http.Request) {
		c, ok := s.user(w, r)
		if !ok {
			return
		}
		d, ok := s.Store.Site(c, r.PathValue("id"))
		if !ok {
			errorJSON(w, http.StatusNotFound, "NOT_FOUND", "no such site")
			return
		}
		writeJSON(w, http.StatusOK, d)
	})
	// Node-level detail: asks the device to stream it (for 60 s from the last call)
	// and waits up to waitMs for the first snapshot.
	mux.HandleFunc("GET /api/sites/{id}/detail", func(w http.ResponseWriter, r *http.Request) {
		c, ok := s.user(w, r)
		if !ok {
			return
		}
		id := r.PathValue("id")
		if _, ok := s.Store.Site(c, id); !ok {
			errorJSON(w, http.StatusNotFound, "NOT_FOUND", "no such site")
			return
		}
		s.Store.LeaseDetail(id, 60*time.Second)
		deadline := time.Now().Add(time.Duration(min(intParam(r, "waitMs", 3000), 10_000)) * time.Millisecond)
		for {
			d, _ := s.Store.Site(c, id)
			if d.Detail != nil {
				writeJSON(w, http.StatusOK, d.Detail)
				return
			}
			if !d.Connected || time.Now().After(deadline) {
				writeJSON(w, http.StatusAccepted, map[string]any{"status": "requested", "connected": d.Connected})
				return
			}
			select {
			case <-r.Context().Done():
				return
			case <-time.After(100 * time.Millisecond):
			}
		}
	})
	mux.HandleFunc("GET /api/alarms", func(w http.ResponseWriter, r *http.Request) {
		c, ok := s.user(w, r)
		if !ok {
			return
		}
		q := r.URL.Query()
		f := fleet.AlarmFilter{MinSeverity: proto.SeverityRank(q.Get("min")), Site: q.Get("site"), Unacked: q.Get("unacked") == "1" || q.Get("unacked") == "true", Limit: intParam(r, "limit", 500)}
		items, total := s.Store.ActiveAlarms(c, f)
		if items == nil {
			items = []fleet.AlarmView{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"total": total, "items": items})
	})
	mux.HandleFunc("GET /api/alarms/events", func(w http.ResponseWriter, r *http.Request) {
		c, ok := s.user(w, r)
		if !ok {
			return
		}
		after, _ := strconv.ParseUint(r.URL.Query().Get("after"), 10, 64)
		events, cursor, gap := s.Store.EventsSince(c, after, min(intParam(r, "limit", 500), 5000))
		if events == nil {
			events = []fleet.Event{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"events": events, "cursor": cursor, "gap": gap})
	})
	mux.HandleFunc("POST /api/alarms/acknowledge", func(w http.ResponseWriter, r *http.Request) {
		c, ok := s.user(w, r)
		if !ok {
			return
		}
		var body struct{ Site, ID, Note string }
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
			errorJSON(w, http.StatusBadRequest, "BAD_REQUEST", "expected {site, id, note}")
			return
		}
		if err := s.Store.Acknowledge(c, body.Site, body.ID, body.Note); err != nil {
			var nf fleet.ErrNotFound
			if errors.As(err, &nf) {
				errorJSON(w, http.StatusNotFound, "NOT_FOUND", err.Error())
				return
			}
			errorJSON(w, http.StatusInternalServerError, "ERROR", err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /api/me", func(w http.ResponseWriter, r *http.Request) {
		if c, ok := s.user(w, r); ok {
			writeJSON(w, http.StatusOK, map[string]any{"user": c.Subject, "tenant": c.Tenant, "admin": c.Admin()})
		}
	})
	// For the ingress / load balancer: liveness only, nothing about the fleet
	// (the internal listener's /healthz has the counts).
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.Handle("GET /", web.Handler())
	return securityHeaders(mux)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'self'; connect-src 'self' ws: wss:; img-src 'self' data:; style-src 'self' 'unsafe-inline'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	o := s.Store.Overview(proto.Claims{Kind: proto.KindUser, Tenant: proto.AllTenants})
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "sites": o.Sites, "connected": o.Connected, "uptimeS": int(time.Since(s.Started).Seconds())})
}

// InternalHandler serves health, metrics and Go profiles on the internal port
// (Prometheus, probes, operators). Never expose it through the ingress.
func (s *Server) InternalHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /debug/pprof/", pprof.Index)
	mux.HandleFunc("GET /debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("GET /debug/pprof/trace", pprof.Trace)
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		doc := s.Metrics()
		if r.URL.Query().Get("format") == "json" {
			writeJSON(w, http.StatusOK, doc)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write(promText(doc))
	})
	return mux
}

// Metrics is the whole metrics document.
func (s *Server) Metrics() map[string]any {
	o := s.Store.Overview(proto.Claims{Kind: proto.KindUser, Tenant: proto.AllTenants})
	st := &s.Store.C
	return map[string]any{
		"noc": map[string]any{"version": s.Version, "uptimeS": int(time.Since(s.Started).Seconds())},
		"fleet": map[string]any{
			"sites": o.Sites, "connected": o.Connected, "byStatus": o.ByStatus, "alarms": o.Alarms, "acknowledged": o.Acked,
			"lastEvent": o.LastEvent,
			"summaries": st.Summaries.Load(), "deltas": st.Deltas.Load(), "alarmMessages": st.AlarmMessages.Load(), "alarmEvents": st.AlarmEvents.Load(),
			"duplicateEvents": st.DuplicateEvents.Load(), "detailMessages": st.DetailMessages.Load(), "hashMismatch": st.HashMismatch.Load(),
			"resync":    map[string]uint64{"summary": st.ResyncSummary.Load(), "alarms": st.ResyncAlarms.Load(), "detail": st.ResyncDetail.Load()},
			"takeovers": st.Takeovers.Load(),
		},
		"uplink":    s.Uplink.Stats(),
		"dashboard": s.Hub.Stats(),
		"process":   processStats(),
	}
}
