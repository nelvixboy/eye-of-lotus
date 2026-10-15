package web

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
)

// writeJSON writes a JSON response with the right content type.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if v == nil {
		return
	}
	enc := json.NewEncoder(w)
	// The panel is read by a person, not by a machine, so readable JSON
	// beats compact JSON when someone opens the URL directly.
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		// The headers are already out; nothing useful can be added.
		return
	}
}

// apiError is the shape of every error the API returns.
type apiError struct {
	Error   string `json:"error"`
	Detail  string `json:"detail,omitempty"`
	RetryIn int    `json:"retry_in_seconds,omitempty"`
}

func writeError(w http.ResponseWriter, status int, msg, detail string) {
	writeJSON(w, status, apiError{Error: msg, Detail: detail})
}

// decodeJSON reads a small JSON body, refusing anything oversized. A
// monitoring panel has no business accepting large uploads.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	if ct := r.Header.Get("Content-Type"); ct != "" {
		if !strings.HasPrefix(ct, "application/json") {
			return errors.New("expected a JSON body")
		}
	}
	body := http.MaxBytesReader(w, r.Body, 8<<10)
	defer body.Close()
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return errors.New("request body is too large")
		}
		if errors.Is(err, io.EOF) {
			return errors.New("empty request body")
		}
		return err
	}
	return nil
}

// logRequest records one request at debug level, and anything unusual at
// a higher one. Static asset noise is dropped entirely.
func logRequest(log *slog.Logger, r *http.Request, status int, dur int64) {
	switch {
	case strings.HasPrefix(r.URL.Path, "/static/"):
		return
	case status >= 500:
		log.Error("request", "method", r.Method, "path", r.URL.Path, "status", status, "ms", dur)
	case status >= 400:
		log.Warn("request", "method", r.Method, "path", r.URL.Path, "status", status, "ms", dur)
	default:
		log.Debug("request", "method", r.Method, "path", r.URL.Path, "status", status, "ms", dur)
	}
}

// statusRecorder remembers the status code for the access log.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += n
	return n, err
}

// queryInt reads an integer query parameter, clamped to a sane range.
func queryInt(r *http.Request, key string, def, min, max int) int {
	raw := r.URL.Query().Get(key)
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}
