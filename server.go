package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// builtinOrigins may pair with the relay and read its responses (design §4.2).
var builtinOrigins = []string{"https://events.kojira.io", "https://eventer-staging.kojiran.workers.dev"}

type server struct {
	pair    *pairing
	dev     bool // also allow http://localhost:* and http://127.0.0.1:*
	ffmpeg  string
	logf    func(string, ...any)
	version string

	mu     sync.Mutex
	target *target  // set by /v1/start; the key lives only here (memory)
	cur    *session // the live WHIP session, if any
}

// allowedOrigin reports whether origin may use the relay.
func (s *server) allowedOrigin(origin string) bool {
	for _, o := range builtinOrigins {
		if origin == o {
			return true
		}
	}
	if !s.dev {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "http" || u.Path != "" {
		return false
	}
	h := u.Hostname()
	return h == "localhost" || h == "127.0.0.1"
}

func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/hello", s.hello)
	mux.HandleFunc("POST /v1/pair", s.handlePair)
	mux.Handle("POST /v1/start", s.auth(s.start))
	mux.Handle("POST /v1/whip", s.auth(s.whip))
	mux.Handle("DELETE /v1/whip/{id}", s.auth(s.stopWHIP))
	mux.Handle("GET /v1/status", s.auth(s.status))
	return s.cors(mux)
}

// cors answers preflights and adds CORS headers only for allowed origins. Other origins get no
// CORS headers, so the browser won't let their pages read anything (design §4.2).
func (s *server) cors(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		allowed := origin != "" && s.allowedOrigin(origin)
		if allowed {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
			w.Header().Set("Access-Control-Expose-Headers", "Location")
			w.Header().Set("Access-Control-Max-Age", "600")
			// Chrome's (older) Private Network Access preflight. Harmless for newer Chrome.
			if r.Header.Get("Access-Control-Request-Private-Network") == "true" {
				w.Header().Set("Access-Control-Allow-Private-Network", "true")
			}
		}
		if r.Method == http.MethodOptions {
			if !allowed {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// auth requires an allowed Origin and the Bearer token issued to that origin.
func (s *server) auth(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if origin == "" || !s.allowedOrigin(origin) || !s.pair.Check(origin, token) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		h(w, r)
	})
}

func (s *server) hello(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"app": "eventslab-relay", "version": s.version, "paired": s.pair.Paired()})
}

func (s *server) handlePair(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Origin string `json:"origin"`
		Code   string `json:"code"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request"})
		return
	}
	origin := r.Header.Get("Origin")
	// The origin in the body must match the page's real Origin (a page can't fake that header).
	if origin == "" || in.Origin != origin || !s.allowedOrigin(origin) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "origin_not_allowed"})
		return
	}
	token, err := s.pair.Pair(origin, strings.TrimSpace(in.Code))
	if errors.Is(err, errBadCode) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "wrong_code"})
		return
	}
	if err != nil {
		s.logf("pair: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal"})
		return
	}
	s.logf("pair: paired with %s", origin)
	writeJSON(w, http.StatusOK, map[string]string{"token": token})
}

func (s *server) start(w http.ResponseWriter, r *http.Request) {
	var in struct {
		RTMPURL   string `json:"rtmpUrl"`
		StreamKey string `json:"streamKey"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request"})
		return
	}
	t, err := parseTarget(in.RTMPURL, in.StreamKey)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_target", "message": err.Error()})
		return
	}
	s.mu.Lock()
	s.target = &t
	s.mu.Unlock()
	s.logf("start: target %s", t)
	writeJSON(w, http.StatusOK, map[string]string{"whipUrl": "/v1/whip"})
}

func (s *server) whip(w http.ResponseWriter, r *http.Request) {
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/sdp") {
		writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{"error": "content_type"})
		return
	}
	offer, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil || len(offer) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_offer"})
		return
	}
	s.mu.Lock()
	t := s.target
	old := s.cur
	s.cur = nil
	s.mu.Unlock()
	if t == nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "not_started", "message": "call /v1/start first"})
		return
	}
	if old != nil {
		old.Close() // one stream at a time
	}
	id := randomID()
	sess, answer, err := newSession(id, string(offer), *t, s.ffmpeg, s.logf)
	if err != nil {
		s.logf("whip: %v", scrub(err, t.key))
		status := http.StatusBadRequest
		if errors.Is(err, errNoFFmpeg) {
			status = http.StatusServiceUnavailable
		}
		writeJSON(w, status, map[string]string{"error": "whip_failed", "message": scrub(err, t.key).Error()})
		return
	}
	sess.onClose = func() {
		s.mu.Lock()
		if s.cur == sess {
			s.cur = nil
		}
		s.mu.Unlock()
	}
	s.mu.Lock()
	s.cur = sess
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/sdp")
	w.Header().Set("Location", "/v1/whip/"+id)
	w.WriteHeader(http.StatusCreated)
	_, _ = io.WriteString(w, answer)
}

func (s *server) stopWHIP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	cur := s.cur
	if cur != nil && cur.id == r.PathValue("id") {
		s.cur = nil
	} else {
		cur = nil
	}
	s.mu.Unlock()
	if cur == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	cur.Close()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

type statusResponse struct {
	State  string `json:"state"`           // idle | connecting | live | error
	RTMP   string `json:"rtmp"`            // idle | ok | retrying | failed
	Error  string `json:"error,omitempty"` // last error (never contains the key)
	FFmpeg bool   `json:"ffmpeg"`
	statusSnapshot
}

func (s *server) status(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	cur := s.cur
	s.mu.Unlock()
	res := statusResponse{State: "idle", RTMP: rtmpIdle, FFmpeg: s.ffmpeg != ""}
	if s.ffmpeg == "" {
		res.State, res.Error = "error", errNoFFmpeg.Error()
	}
	if cur != nil {
		st, errText := cur.State()
		rs, rerr := cur.out.State()
		res.State, res.RTMP, res.statusSnapshot = st, rs, cur.stats.snapshot()
		if errText != "" {
			res.Error = errText
		} else if rerr != "" {
			res.Error = rerr
		}
	}
	writeJSON(w, http.StatusOK, res)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func randomID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
