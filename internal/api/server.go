// Package api exposes the daemon over HTTP/JSON.
package api

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/shurshur/magnet/internal/daemon"
	"github.com/shurshur/magnet/internal/fsutil"
	"github.com/shurshur/magnet/internal/model"
)

// Config controls the HTTP server.
type Config struct {
	Manager *daemon.Manager
	// Token enables Bearer token authentication for /v1 endpoints.
	Token   string
	Version string
	Logger  *slog.Logger
}

// Server is the daemon HTTP API.
type Server struct {
	mgr     *daemon.Manager
	token   string
	version string
	log     *slog.Logger
	handler http.Handler
}

// New creates the API server and its routes.
func New(cfg Config) (*Server, error) {
	if cfg.Manager == nil {
		return nil, errors.New("api: manager is required")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	s := &Server{
		mgr:     cfg.Manager,
		token:   cfg.Token,
		version: cfg.Version,
		log:     cfg.Logger,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /v1/stats", s.stats)
	mux.HandleFunc("GET /v1/jobs", s.listJobs)
	mux.HandleFunc("POST /v1/jobs", s.addJobs)
	mux.HandleFunc("GET /v1/jobs/{id}", s.getJob)
	mux.HandleFunc("DELETE /v1/jobs/{id}", s.deleteJob)
	mux.HandleFunc("POST /v1/jobs/{id}/retry", s.retryJob)
	mux.HandleFunc("POST /v1/jobs/{id}/cancel", s.cancelJob)
	mux.HandleFunc("GET /v1/jobs/{id}/torrent", s.downloadTorrent)

	s.handler = s.recoverer(s.logger(s.authenticate(mux)))
	return s, nil
}

// Handler returns the composed HTTP handler.
func (s *Server) Handler() http.Handler { return s.handler }

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":         "ok",
		"version":        s.version,
		"uptime_seconds": int64(s.mgr.Uptime().Seconds()),
	})
}

func (s *Server) stats(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"stats":          s.mgr.Stats(),
		"uptime_seconds": int64(s.mgr.Uptime().Seconds()),
	})
}

func (s *Server) addJobs(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<20)
	defer r.Body.Close()
	var req struct {
		Magnet  string   `json:"magnet"`
		Magnets []string `json:"magnets"`
	}
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&req); err != nil {
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

	jobs, created, err := s.mgr.AddBatch(links)
	if err != nil {
		var ve *daemon.ValidationError
		if errors.As(err, &ve) {
			writeError(w, http.StatusBadRequest, "invalid_magnet", ve.Error())
			return
		}
		s.log.Error("add jobs failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "could not add jobs")
		return
	}
	status := http.StatusOK
	if created > 0 {
		status = http.StatusCreated
	}
	writeJSON(w, status, map[string]any{
		"jobs":     jobs,
		"created":  created,
		"existing": len(jobs) - created,
	})
}

func (s *Server) listJobs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	state := model.State(strings.TrimSpace(q.Get("state")))
	limit := 100
	offset := 0
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
	jobs, total, err := s.mgr.List(daemon.ListOptions{State: state, Limit: limit, Offset: offset})
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_state", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"jobs":   jobs,
		"total":  total,
		"limit":  limit,
		"offset": offset,
	})
}

func (s *Server) getJob(w http.ResponseWriter, r *http.Request) {
	j, err := s.mgr.Get(r.PathValue("id"))
	if err != nil {
		s.jobError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"job": j})
}

func (s *Server) cancelJob(w http.ResponseWriter, r *http.Request) {
	j, err := s.mgr.Cancel(r.PathValue("id"))
	if err != nil {
		s.jobError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"job": j})
}

func (s *Server) retryJob(w http.ResponseWriter, r *http.Request) {
	j, err := s.mgr.Retry(r.PathValue("id"))
	if err != nil {
		s.jobError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"job": j})
}

func (s *Server) deleteJob(w http.ResponseWriter, r *http.Request) {
	purge := false
	if v := r.URL.Query().Get("purge"); v != "" {
		purge, _ = strconv.ParseBool(v)
	}
	if err := s.mgr.Delete(r.PathValue("id"), purge); err != nil {
		s.jobError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"deleted": true,
		"id":      strings.ToLower(r.PathValue("id")),
		"purged":  purge,
	})
}

func (s *Server) downloadTorrent(w http.ResponseWriter, r *http.Request) {
	path, name, err := s.mgr.TorrentFile(r.PathValue("id"))
	if err != nil {
		s.jobError(w, err)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		writeError(w, http.StatusNotFound, "torrent_unavailable", "torrent file is not available")
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "cannot read torrent file")
		return
	}
	filename := fsutil.TorrentBaseName(name, r.PathValue("id"))
	disposition := mime.FormatMediaType("attachment", map[string]string{"filename": filename})
	if disposition == "" {
		disposition = "attachment"
	}
	w.Header().Set("Content-Type", "application/x-bittorrent")
	w.Header().Set("Content-Disposition", disposition)
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, filename, st.ModTime(), f)
}

func (s *Server) jobError(w http.ResponseWriter, err error) {
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
		s.log.Error("api request failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
	}
}

func (s *Server) authenticate(next http.Handler) http.Handler {
	if s.token == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		provided := ""
		if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
			provided = strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
		}
		if provided == "" {
			provided = strings.TrimSpace(r.Header.Get("X-API-Token"))
		}
		want := sha256.Sum256([]byte(s.token))
		got := sha256.Sum256([]byte(provided))
		if provided == "" || subtle.ConstantTimeCompare(want[:], got[:]) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="magnet"`)
			writeError(w, http.StatusUnauthorized, "unauthorized", "missing or invalid API token")
			return
		}
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

func (s *Server) logger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)
		if sw.status == 0 {
			sw.status = http.StatusOK
		}
		s.log.Debug("http request",
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
				s.log.Error("panic in http handler",
					"panic", fmt.Sprint(rec),
					"stack", string(debug.Stack()),
				)
				writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
			}
		}()
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
