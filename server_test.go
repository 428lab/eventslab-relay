package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const eventsOrigin = "https://events.kojira.io"

func testServer(t *testing.T, dev bool) (*server, *httptest.Server) {
	t.Helper()
	p, err := newPairing(filepath.Join(t.TempDir(), "tokens.json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	s := &server{pair: p, dev: dev, ffmpeg: "ffmpeg", logf: t.Logf, version: "test"}
	ts := httptest.NewServer(s.handler())
	t.Cleanup(ts.Close)
	return s, ts
}

func do(t *testing.T, ts *httptest.Server, method, path, origin, token, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { res.Body.Close() })
	return res
}

func pairToken(t *testing.T, s *server, ts *httptest.Server, origin string) string {
	t.Helper()
	res := do(t, ts, "POST", "/v1/pair", origin, "", `{"origin":"`+origin+`","code":"`+s.pair.Code()+`"}`)
	if res.StatusCode != 200 {
		t.Fatalf("pair: %d", res.StatusCode)
	}
	var out struct{ Token string }
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil || out.Token == "" {
		t.Fatalf("pair token: %v %q", err, out.Token)
	}
	return out.Token
}

func TestCORSOnlyForAllowedOrigins(t *testing.T) {
	_, ts := testServer(t, false)
	ok := do(t, ts, "GET", "/v1/hello", eventsOrigin, "", "")
	if got := ok.Header.Get("Access-Control-Allow-Origin"); got != eventsOrigin {
		t.Fatalf("allowed origin: ACAO=%q", got)
	}
	bad := do(t, ts, "GET", "/v1/hello", "https://evil.example", "", "")
	if got := bad.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("other origin must get no CORS, got %q", got)
	}
	// localhost only with --dev
	if got := do(t, ts, "GET", "/v1/hello", "http://localhost:5173", "", "").Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("localhost without --dev: ACAO=%q", got)
	}
	_, devTS := testServer(t, true)
	if got := do(t, devTS, "GET", "/v1/hello", "http://localhost:5173", "", "").Header.Get("Access-Control-Allow-Origin"); got != "http://localhost:5173" {
		t.Fatalf("localhost with --dev: ACAO=%q", got)
	}

	// preflight with the Private Network Access header
	req, _ := http.NewRequest("OPTIONS", ts.URL+"/v1/start", nil)
	req.Header.Set("Origin", eventsOrigin)
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Private-Network", "true")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 204 || res.Header.Get("Access-Control-Allow-Private-Network") != "true" {
		t.Fatalf("preflight: %d PNA=%q", res.StatusCode, res.Header.Get("Access-Control-Allow-Private-Network"))
	}
}

func TestAuthAndPairing(t *testing.T) {
	s, ts := testServer(t, false)

	// hello works before pairing
	var hello struct {
		App    string
		Paired bool
	}
	_ = json.NewDecoder(do(t, ts, "GET", "/v1/hello", "", "", "").Body).Decode(&hello)
	if hello.App != "eventslab-relay" || hello.Paired {
		t.Fatalf("hello: %+v", hello)
	}

	// no token -> 401
	if res := do(t, ts, "GET", "/v1/status", eventsOrigin, "", ""); res.StatusCode != 401 {
		t.Fatalf("status without token: %d", res.StatusCode)
	}
	// pairing from a page whose Origin doesn't match the body, or isn't allowed -> 403
	if res := do(t, ts, "POST", "/v1/pair", "https://evil.example", "", `{"origin":"https://evil.example","code":"`+s.pair.Code()+`"}`); res.StatusCode != 403 {
		t.Fatalf("pair from other origin: %d", res.StatusCode)
	}
	if res := do(t, ts, "POST", "/v1/pair", eventsOrigin, "", `{"origin":"https://evil.example","code":"`+s.pair.Code()+`"}`); res.StatusCode != 403 {
		t.Fatalf("pair with mismatched origin: %d", res.StatusCode)
	}
	// wrong code -> 403; five wrong codes make a new code (the lockout is skipped here with a
	// test clock that jumps past it; TestPairingLockout covers the lockout itself)
	first := s.pair.Code()
	clock := time.Now()
	s.pair.now = func() time.Time { clock = clock.Add(time.Minute); return clock }
	for i := 0; i < maxCodeAttempts; i++ {
		if res := do(t, ts, "POST", "/v1/pair", eventsOrigin, "", `{"origin":"`+eventsOrigin+`","code":"xxxxxx"}`); res.StatusCode != 403 {
			t.Fatalf("wrong code: %d", res.StatusCode)
		}
	}
	if s.pair.Code() == first {
		t.Fatal("the code must change after 5 wrong attempts")
	}

	token := pairToken(t, s, ts, eventsOrigin)
	if res := do(t, ts, "GET", "/v1/status", eventsOrigin, token, ""); res.StatusCode != 200 {
		t.Fatalf("status with token: %d", res.StatusCode)
	}
	// the token is bound to its origin
	staging := "https://eventer-staging.kojiran.workers.dev"
	if res := do(t, ts, "GET", "/v1/status", staging, token, ""); res.StatusCode != 401 {
		t.Fatalf("token from another origin: %d", res.StatusCode)
	}
	// a fresh pairing survives a restart (tokens are on disk)
	p2, err := newPairing(s.pair.path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !p2.Check(eventsOrigin, token) || !p2.Paired() {
		t.Fatal("token must survive a restart")
	}
	// /v1/start validates the target; a bad URL is 400
	if res := do(t, ts, "POST", "/v1/start", eventsOrigin, token, `{"rtmpUrl":"http://x/y","streamKey":"k"}`); res.StatusCode != 400 {
		t.Fatalf("bad rtmpUrl: %d", res.StatusCode)
	}
	if res := do(t, ts, "POST", "/v1/start", eventsOrigin, token, `{"rtmpUrl":"rtmp://a.rtmp.youtube.com/live2","streamKey":"abcd-efgh"}`); res.StatusCode != 200 {
		t.Fatalf("start: %d", res.StatusCode)
	}
	// WHIP before /v1/start on a fresh server -> 409; wrong content type -> 415
	s2, ts2 := testServer(t, false)
	tok2 := pairToken(t, s2, ts2, eventsOrigin)
	req, _ := http.NewRequest("POST", ts2.URL+"/v1/whip", strings.NewReader("v=0"))
	req.Header.Set("Origin", eventsOrigin)
	req.Header.Set("Authorization", "Bearer "+tok2)
	req.Header.Set("Content-Type", "application/sdp")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 409 {
		t.Fatalf("whip before start: %d", res.StatusCode)
	}
}
