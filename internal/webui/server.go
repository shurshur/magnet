package webui

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/shurshur/magnet/internal/api"
	"github.com/shurshur/magnet/internal/daemon"
	"github.com/shurshur/magnet/internal/model"
)

//go:embed web
var webFiles embed.FS

// Server serves the web UI and its small JSON API.
type Server struct {
	backend Backend
	version string
	log     *slog.Logger
	handler http.Handler
}

// NewServer creates the web UI HTTP handler.
func NewServer(backend Backend, version string, log *slog.Logger) (*Server, error) {
	if backend == nil {
		return nil, errors.New("webui: backend is required")
	}
	if log == nil {
		log = slog.Default()
	}
	s := &Server{backend: backend, version: version, log: log}

	webRoot, err := fs.Sub(webFiles, "web")
	if err != nil {
		return nil, fmt.Errorf("prepare embedded web UI: %w", err)
	}
	fileServer := http.FileServer(http.FS(webRoot))

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/info", s.info)
	mux.HandleFunc("GET /api/stats", s.stats)
	mux.HandleFunc("GET /api/jobs", s.listJobs)
	mux.HandleFunc("POST /api/jobs", s.addJobs)
	mux.HandleFunc("GET /api/jobs/{id}", s.getJob)
	mux.HandleFunc("DELETE /api/jobs/{id}", s.deleteJob)
	mux.HandleFunc("POST /api/jobs/{id}/cancel", s.cancelJob)
	mux.HandleFunc("POST /api/jobs/{id}/retry", s.retryJob)
	mux.HandleFunc("GET /api/jobs/{id}/torrent", s.downloadTorrent)
	mux.Handle("GET /", noStore(fileServer))

	s.handler = s.securityHeaders(s.recoverer(s.logger(s.localOnly(mux))))
	return s, nil
}

// Handler returns the composed HTTP handler.
func (s *Server) Handler() http.Handler { return s.handler }

func (s *Server) info(w http.ResponseWriter, r *http.Request) {
	info := s.backend.Info()
	writeJSON(w, http.StatusOK, map[string]any{
		"mode":     info.Mode,
		"data_dir": info.DataDir,
		"server":   info.Server,
		"version":  s.version,
	})
}

func (s *Server) stats(w http.ResponseWriter, r *http.Request) {
	stats, uptime, err := s.backend.Stats(r.Context())
	if err != nil {
		s.backendError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"stats":          stats,
		"uptime_seconds": uptime,
	})
}

func (s *Server) listJobs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	state := model.State(strings.TrimSpace(q.Get("state")))
	switch state {
	case "", model.StateQueued, model.StateSearching, model.StateFound, model.StateIncomplete, model.StateFailed, model.StateCanceled:
	default:
		writeError(w, http.StatusBadRequest, "invalid_state", "unknown state "+string(state))
		return
	}
	limit, offset := 200, 0
	var err error
	if v := q.Get("limit"); v != "" {
		limit, err = strconv.Atoi(v)
		if err != nil || limit < 1 || limit > 1000 {
			writeError(w, http.StatusBadRequest, "invalid_limit", "limit must be between 1 and 1000")
			return
		}
	}
	if v := q.Get("offset"); v != "" {
		offset, err = strconv.Atoi(v)
		if err != nil || offset < 0 {
			writeError(w, http.StatusBadRequest, "invalid_offset", "offset must be a non-negative integer")
			return
		}
	}
	jobs, total, err := s.backend.List(r.Context(), daemon.ListOptions{State: state, Limit: limit, Offset: offset})
	if err != nil {
		s.backendError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"jobs":   jobs,
		"total":  total,
		"limit":  limit,
		"offset": offset,
	})
}

func (s *Server) addJobs(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<20)
	defer r.Body.Close()
	var req struct {
		Magnet  string   `json:"magnet"`
		Magnets []string `json:"magnets"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "invalid JSON body: "+err.Error())
		return
	}
	links := make([]string, 0, len(req.Magnets)+1)
	if strings.TrimSpace(req.Magnet) != "" {
		links = append(links, req.Magnet)
	}
	links = append(links, req.Magnets...)
	if len(links) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "provide \"magnet\" or \"magnets\"")
		return
	}
	jobs, created, existing, err := s.backend.Add(r.Context(), links)
	if err != nil {
		s.backendError(w, err)
		return
	}
	status := http.StatusOK
	if created > 0 {
		status = http.StatusCreated
	}
	writeJSON(w, status, map[string]any{
		"jobs":     jobs,
		"created":  created,
		"existing": existing,
	})
}

func (s *Server) getJob(w http.ResponseWriter, r *http.Request) {
	j, err := s.backend.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		s.backendError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"job": j})
}

func (s *Server) cancelJob(w http.ResponseWriter, r *http.Request) {
	j, err := s.backend.Cancel(r.Context(), r.PathValue("id"))
	if err != nil {
		s.backendError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"job": j})
}

func (s *Server) retryJob(w http.ResponseWriter, r *http.Request) {
	j, err := s.backend.Retry(r.Context(), r.PathValue("id"))
	if err != nil {
		s.backendError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"job": j})
}

func (s *Server) deleteJob(w http.ResponseWriter, r *http.Request) {
	purge := false
	if v := r.URL.Query().Get("purge"); v != "" {
		purge, _ = strconv.ParseBool(v)
	}
	if err := s.backend.Delete(r.Context(), r.PathValue("id"), purge); err != nil {
		s.backendError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true, "purged": purge})
}

func (s *Server) downloadTorrent(w http.ResponseWriter, r *http.Request) {
	body, filename, err := s.backend.OpenTorrent(r.Context(), r.PathValue("id"))
	if err != nil {
		s.backendError(w, err)
		return
	}
	defer body.Close()
	if filename == "" {
		filename = "magnet.torrent"
	}
	disposition := mime.FormatMediaType("attachment", map[string]string{"filename": filename})
	if disposition == "" {
		disposition = "attachment"
	}
	w.Header().Set("Content-Type", "application/x-bittorrent")
	w.Header().Set("Content-Disposition", disposition)
	w.Header().Set("Cache-Control", "no-store")
	if _, err := io.Copy(w, body); err != nil {
		s.log.Debug("torrent download interrupted", "error", err)
	}
}

func (s *Server) backendError(w http.ResponseWriter, err error) {
	var apiErr *api.Error
	if errors.As(err, &apiErr) {
		code := apiErr.Code
		if code == "" {
			code = "backend_error"
		}
		writeError(w, apiErr.StatusCode, code, apiErr.Message)
		return
	}
	var validation *daemon.ValidationError
	if errors.As(err, &validation) {
		writeError(w, http.StatusBadRequest, "invalid_magnet", validation.Error())
		return
	}
	switch {
	case errors.Is(err, daemon.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "job not found")
	case errors.Is(err, daemon.ErrNotCancelable):
		writeError(w, http.StatusConflict, "not_cancelable", err.Error())
	case errors.Is(err, daemon.ErrNotRetryable):
		writeError(w, http.StatusConflict, "not_retryable", err.Error())
	case errors.Is(err, daemon.ErrNotReady):
		writeError(w, http.StatusNotFound, "torrent_unavailable", err.Error())
	default:
		s.log.Error("web UI backend request failed", "error", err)
		writeError(w, http.StatusBadGateway, "backend_error", err.Error())
	}
}

func (s *Server) localOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") && r.Method != http.MethodGet && r.Method != http.MethodHead {
			if r.Header.Get("X-Magnet-Web") != "1" {
				writeError(w, http.StatusForbidden, "forbidden", "missing X-Magnet-Web header")
				return
			}
			if origin := r.Header.Get("Origin"); origin != "" {
				allowed := false
				for _, host := range []string{r.Host, r.Header.Get("X-Forwarded-Host")} {
					if host == "" {
						continue
					}
					if origin == "http://"+host || origin == "https://"+host {
						allowed = true
						break
					}
				}
				if !allowed {
					writeError(w, http.StatusForbidden, "forbidden", "cross-origin request rejected")
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; base-uri 'none'; form-action 'self'")
		next.ServeHTTP(w, r)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytes += n
	return n, err
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (s *Server) logger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)
		if sw.status == 0 {
			sw.status = http.StatusOK
		}
		s.log.Debug("web UI http request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", sw.status,
			"bytes", sw.bytes,
			"duration", time.Since(start).String(),
			"remote", r.RemoteAddr,
		)
	})
}

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("panic in web UI handler",
					"panic", fmt.Sprint(rec),
					"stack", string(debug.Stack()),
				)
				writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func noStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		http.Error(w, `{"error":{"code":"internal_error","message":"failed to encode response"}}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	w.Write(append(data, '\n'))
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]string{
			"code":    code,
			"message": message,
		},
	})
}
