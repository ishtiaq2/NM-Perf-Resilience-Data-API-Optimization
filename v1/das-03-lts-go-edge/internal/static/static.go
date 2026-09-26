// Package static serves the web UI from the same origin as the API: the
// Angular build from a directory, or the status page embedded in the binary.
//
//   - fingerprinted assets (main-7XQ2ZK5D.js) are cached for a year, HTML is revalidated
//   - text assets are gzip-compressed ONCE (or a pre-built .gz next to the file is
//     used) and kept in memory; later requests only write bytes
//   - unknown paths without a file extension return index.html (client-side routes)
package static

import (
	"bytes"
	"compress/gzip"
	"embed"
	"hash/fnv"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed ui
var embedded embed.FS

// Embedded is the built-in status page, used when no web directory is configured.
func Embedded() fs.FS {
	sub, _ := fs.Sub(embedded, "ui")
	return sub
}

const (
	maxCachedFile = 16 << 20
	maxCacheTotal = 64 << 20
)

var fingerprinted = regexp.MustCompile(`[-.][A-Z0-9]{8,}\.[a-z0-9]+$`)

type entry struct {
	mod  time.Time
	size int64
	tag  string
	data []byte
	gz   []byte // nil: not compressible or not worth it
}

// Handler serves one file system.
type Handler struct {
	fsys fs.FS

	mu    sync.Mutex
	cache map[string]*entry
	bytes int
}

// New serves fsys.
func New(fsys fs.FS) *Handler { return &Handler{fsys: fsys, cache: map[string]*entry{}} }

func compressible(ctype string) bool {
	return strings.HasPrefix(ctype, "text/") || strings.Contains(ctype, "javascript") || strings.Contains(ctype, "json") ||
		strings.Contains(ctype, "xml") || strings.Contains(ctype, "svg") || strings.Contains(ctype, "wasm")
}

func acceptsGzip(r *http.Request) bool {
	for _, v := range r.Header.Values("Accept-Encoding") {
		for _, part := range strings.Split(v, ",") {
			name, params, _ := strings.Cut(strings.TrimSpace(part), ";")
			if !strings.EqualFold(strings.TrimSpace(name), "gzip") {
				continue
			}
			k, val, ok := strings.Cut(strings.TrimSpace(params), "=")
			if !ok || strings.TrimSpace(k) != "q" {
				return true
			}
			q, err := strconv.ParseFloat(strings.TrimSpace(val), 64)
			return err == nil && q > 0
		}
	}
	return false
}

func contentType(name string, data []byte) string {
	if t := mime.TypeByExtension(path.Ext(name)); t != "" {
		return t
	}
	return http.DetectContentType(data)
}

// load returns the cached entry for name, (re)reading it when the file changed.
func (h *Handler) load(name string) (*entry, bool) {
	st, err := fs.Stat(h.fsys, name)
	if err != nil || st.IsDir() || st.Size() > maxCachedFile {
		return nil, false
	}
	h.mu.Lock()
	e, ok := h.cache[name]
	h.mu.Unlock()
	if ok && e.mod.Equal(st.ModTime()) && e.size == st.Size() {
		return e, true
	}
	data, err := fs.ReadFile(h.fsys, name)
	if err != nil {
		return nil, false
	}
	sum := fnv.New64a()
	sum.Write(data)
	e = &entry{mod: st.ModTime(), size: st.Size(), data: data, tag: `"` + strconv.FormatUint(sum.Sum64(), 36) + `"`}
	if ctype := contentType(name, data); compressible(ctype) && len(data) > 1024 {
		if pre, err := fs.ReadFile(h.fsys, name+".gz"); err == nil { // built at deploy time (gzip -9k)
			e.gz = pre
		} else {
			var buf bytes.Buffer
			zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression) // once per file, so spend the CPU
			_, _ = zw.Write(data)
			_ = zw.Close()
			if buf.Len() < len(data)*9/10 {
				e.gz = buf.Bytes()
			}
		}
	}
	h.mu.Lock()
	if old, ok := h.cache[name]; ok {
		h.bytes -= len(old.data) + len(old.gz)
		delete(h.cache, name)
	}
	if h.bytes+len(e.data)+len(e.gz) <= maxCacheTotal {
		h.cache[name] = e
		h.bytes += len(e.data) + len(e.gz)
	}
	h.mu.Unlock()
	return e, true
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if name == "" {
		name = "index.html"
	}
	e, ok := h.load(name)
	if !ok {
		if st, err := fs.Stat(h.fsys, name); err == nil && st.IsDir() {
			name = path.Join(name, "index.html")
			e, ok = h.load(name)
		}
	}
	if !ok {
		if path.Ext(name) != "" { // a missing asset is a 404, not the app
			http.NotFound(w, r)
			return
		}
		name = "index.html" // client-side route (e.g. /nodes/12)
		if e, ok = h.load(name); !ok {
			http.NotFound(w, r)
			return
		}
	}
	hd := w.Header()
	ctype := contentType(name, e.data)
	hd.Set("Content-Type", ctype)
	switch {
	case strings.HasSuffix(name, ".html"):
		hd.Set("Cache-Control", "no-cache")
	case fingerprinted.MatchString(name):
		hd.Set("Cache-Control", "public, max-age=31536000, immutable")
	default:
		hd.Set("Cache-Control", "public, max-age=600")
	}
	if e.gz != nil {
		hd.Set("Vary", "Accept-Encoding")
	}
	body, tag := e.data, e.tag
	if e.gz != nil && acceptsGzip(r) {
		body, tag = e.gz, strings.TrimSuffix(e.tag, `"`)+`-gz"`
		hd.Set("Content-Encoding", "gzip")
	}
	hd.Set("ETag", tag)
	if inm := r.Header.Get("If-None-Match"); inm != "" && (inm == tag || strings.Contains(inm, tag)) {
		hd.Del("Content-Encoding")
		w.WriteHeader(http.StatusNotModified)
		return
	}
	hd.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = io.Copy(w, bytes.NewReader(body))
	}
}
