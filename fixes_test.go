package main

import (
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/yutopp/go-rtmp/handshake"
)

// stallServer accepts RTMP connections and misbehaves: "silent" accepts and never answers (the
// handshake hangs), "afterHandshake" completes the handshake and then never answers connect,
// "closeAfterHandshake" completes it and closes (connect then waits forever inside go-rtmp).
func stallServer(t *testing.T, mode string) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var conns []net.Conn
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
			go func(c net.Conn) {
				if mode == "silent" {
					return
				}
				if err := handshake.HandshakeWithClient(c, c, &handshake.Config{}); err != nil {
					return
				}
				if mode == "closeAfterHandshake" {
					_ = c.Close()
					return
				}
				_, _ = io.Copy(io.Discard, c) // read forever, answer nothing
			}(c)
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		for _, c := range conns {
			_ = c.Close()
		}
		mu.Unlock()
	})
	return ln
}

// P1-1: a server that stalls the connect (handshake, connect or createStream) must not block the
// publisher: it reaches "retrying" within the connect deadline, keeps retrying, publishes once a
// real server is there, and Close returns at once even mid-connect.
func TestPublisherConnectDeadline(t *testing.T) {
	for _, mode := range []string{"silent", "afterHandshake", "closeAfterHandshake"} {
		t.Run(mode, func(t *testing.T) {
			logs := &lockedLog{}
			bad := stallServer(t, mode)
			addr := bad.Addr().String()
			tg, _ := parseTarget("rtmp://"+addr+"/live", testKey)
			p := newPublisher(tg, newStats(), nil, logs.logf)
			p.connectTimeout = 300 * time.Millisecond
			p.backoffs = []time.Duration{50 * time.Millisecond}
			p.Start()

			start := time.Now()
			waitFor(t, func() bool { s, _ := p.State(); return s == rtmpRetrying })
			if d := time.Since(start); d > 2*time.Second {
				t.Fatalf("reached retrying after %s (deadline 300ms)", d)
			}
			if _, e := p.State(); !strings.Contains(e, "timed out") && mode != "closeAfterHandshake" {
				t.Fatalf("error: %q", e)
			}

			// A real server appears on the same address: the publisher must get there.
			_ = bad.Close()
			sk := startSinkAt(t, addr)
			waitFor(t, func() bool { s, _ := p.State(); return s == rtmpOK })
			waitFor(t, func() bool { _, _, _, _, n := sk.counts(); return n >= 1 })

			closed := make(chan struct{})
			go func() { p.Close(); <-p.stopped; close(closed) }()
			select {
			case <-closed:
			case <-time.After(2 * time.Second):
				t.Fatal("Close hung")
			}
			if strings.Contains(logs.String(), testKey) {
				t.Fatalf("the key leaked: %s", logs.String())
			}
		})
	}
}

// Close during a stalled connect returns at once (it doesn't wait for the deadline).
func TestPublisherCloseDuringStalledConnect(t *testing.T) {
	bad := stallServer(t, "afterHandshake")
	tg, _ := parseTarget("rtmp://"+bad.Addr().String()+"/live", testKey)
	p := newPublisher(tg, newStats(), nil, func(string, ...any) {})
	p.connectTimeout = time.Minute
	p.Start()
	time.Sleep(200 * time.Millisecond) // inside connect, waiting for the reply
	start := time.Now()
	p.Close()
	select {
	case <-p.stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("the writer goroutine didn't stop")
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("Close took %s", d)
	}
}

// startSinkAt is startSink on a given address (to replace a stalling server).
func startSinkAt(t *testing.T, addr string) *sink {
	t.Helper()
	var ln net.Listener
	var err error
	for i := 0; i < 50; i++ { // the old listener's port may take a moment to free
		if ln, err = net.Listen("tcp", addr); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	return serveSink(t, ln)
}

// P2-1: the RTP clock wraps after about 13.25 h at 90 kHz. FLV timestamps must keep increasing.
func TestVideoTimestampAcrossRTPWrap(t *testing.T) {
	v := &videoIngest{start: time.Now()}
	first := uint32(0xFFFFFFFF - 90*1000) // 1 s before the wrap
	ms0 := v.mediaMs(first)
	ms1 := v.mediaMs(first + 90*500) // +0.5 s, still before the wrap
	ms2 := v.mediaMs(first + 90*2000)
	if first+90*2000 > first {
		t.Fatal("the test value must wrap")
	}
	if ms1-ms0 != 500 || ms2-ms0 != 2000 {
		t.Fatalf("timestamps around the wrap: %d %d %d", ms0, ms1, ms2)
	}
	// 14 hours of 30 fps frames in big steps: still monotonic and exact
	v2 := &videoIngest{start: time.Now()}
	ts := uint32(12345)
	base := v2.mediaMs(ts)
	for i := 1; i <= 14*3600; i++ { // one step per second
		ts += 90000
		if got, want := v2.mediaMs(ts)-base, uint32(i*1000); got != want {
			t.Fatalf("after %d s: %d ms", i, got)
		}
	}
}

// opusPacket is a valid RTP packet carrying one 20 ms Opus silence frame (TOC 0xF8 + empty frame).
func opusPacket(seq uint16, ts uint32) *rtp.Packet {
	return &rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 111, SequenceNumber: seq, Timestamp: ts, SSRC: 1}, Payload: []byte{0xF8, 0xFF, 0xFE}}
}

// fakeFFmpeg writes a small script that stands in for ffmpeg and returns its path.
func fakeFFmpeg(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell script fake")
	}
	p := filepath.Join(t.TempDir(), "ffmpeg")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return p
}

// P2-2: when ffmpeg exits at once, or prints garbage instead of ADTS, audio stops visibly
// (Failure) and Close still returns quickly, even with Opus packets pushed after the failure.
func TestAudioFFmpegFailureDoesNotHangClose(t *testing.T) {
	cases := map[string]string{
		"exits":   "exit 0",
		"garbage": "head -c 200000 /dev/urandom; cat > /dev/null",
		"stalls":  "sleep 60", // never reads stdin, never writes: the pipe fills up
	}
	for name, script := range cases {
		t.Run(name, func(t *testing.T) {
			ff := fakeFFmpeg(t, script)
			out := newPublisher(target{key: testKey}, newStats(), nil, func(string, ...any) {})
			tr, err := newAACTranscoder(ff, out, newStats(), time.Now(), func(string, ...any) {})
			if err != nil {
				t.Fatal(err)
			}
			// push Opus packets from another goroutine, enough to fill the pipe when nobody reads
			var writing sync.WaitGroup
			writing.Add(1)
			go func() {
				defer writing.Done()
				for i := 0; i < 3000; i++ {
					pkt := opusPacket(uint16(i), uint32(i*960))
					if err := tr.WriteRTP(pkt); err != nil {
						return
					}
				}
			}()
			if name != "stalls" {
				waitFor(t, func() bool { return tr.Failure() != "" })
			} else {
				time.Sleep(300 * time.Millisecond)
			}
			done := make(chan struct{})
			start := time.Now()
			go func() { tr.Close(); writing.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("Close hung")
			}
			if d := time.Since(start); d > 3500*time.Millisecond {
				t.Fatalf("Close took %s", d)
			}
		})
	}
}

// P2-3: two offers at the same time leave exactly one live session; the other is closed.
func TestConcurrentWHIPLeavesOneSession(t *testing.T) {
	s, ts := testServer(t, false)
	token := pairToken(t, s, ts, eventsOrigin)
	if res := do(t, ts, "POST", "/v1/start", eventsOrigin, token, `{"rtmpUrl":"rtmp://127.0.0.1:1/live","streamKey":"`+testKey+`"}`); res.StatusCode != 200 {
		t.Fatalf("start: %d", res.StatusCode)
	}
	var mu sync.Mutex
	var made []*session
	var inside atomic.Int32
	s.newSession = func(id, _ string, tg target, _ string, logf func(string, ...any), onClose func(*session)) (*session, string, error) {
		if inside.Add(1) > 1 {
			t.Error("two WHIP setups ran at the same time")
		}
		defer inside.Add(-1)
		time.Sleep(100 * time.Millisecond) // a slow setup (ICE gathering) widens the race
		sess := fakeSession(id, tg, logf, onClose)
		mu.Lock()
		made = append(made, sess)
		mu.Unlock()
		return sess, "v=0\r\n", nil
	}
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest("POST", ts.URL+"/v1/whip", strings.NewReader("v=0\r\n"))
			req.Header.Set("Origin", eventsOrigin)
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("Content-Type", "application/sdp")
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Error(err)
				return
			}
			res.Body.Close()
			if res.StatusCode != 201 {
				t.Errorf("whip: %d", res.StatusCode)
			}
		}()
	}
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if len(made) != 2 {
		t.Fatalf("sessions made: %d", len(made))
	}
	live := 0
	for _, sess := range made {
		select {
		case <-sess.done:
		default:
			live++
			s.mu.Lock()
			cur := s.cur
			s.mu.Unlock()
			if cur != sess {
				t.Fatal("the live session is not the current one")
			}
		}
	}
	if live != 1 {
		t.Fatalf("live sessions: %d (want 1)", live)
	}
}

// fakeSession is a session without WebRTC or ffmpeg: a publisher to tg that is never started.
func fakeSession(id string, tg target, logf func(string, ...any), onClose func(*session)) *session {
	st := newStats()
	sess := &session{id: id, stats: st, start: time.Now(), logf: logf, done: make(chan struct{}),
		needKey: make(chan struct{}, 1), state: "live", out: newPublisher(tg, st, nil, logf)}
	if onClose != nil {
		sess.onClose = func() { onClose(sess) }
	}
	return sess
}

// P2-5: only requests addressed to the relay itself are served (a DNS-rebound name is refused).
func TestHostHeaderChecked(t *testing.T) {
	_, ts := testServer(t, false)
	port := ts.URL[strings.LastIndex(ts.URL, ":")+1:]
	for host, want := range map[string]int{
		"127.0.0.1:" + port:             200,
		"localhost:" + port:             200,
		"attacker.example:" + port:      http.StatusMisdirectedRequest,
		"127.0.0.1":                     http.StatusMisdirectedRequest,
		"evil.127.0.0.1.nip.io:" + port: http.StatusMisdirectedRequest,
	} {
		req, _ := http.NewRequest("GET", ts.URL+"/v1/hello", nil)
		req.Host = host
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != want {
			t.Errorf("Host %q: %d, want %d", host, res.StatusCode, want)
		}
	}
}

// P2-5: wrong pairing codes lock pairing for a growing time (1 s, 2 s, … up to 30 s), even with
// the right code; after the lock the right code works and resets the count.
func TestPairingLockout(t *testing.T) {
	s, ts := testServer(t, false)
	clock := time.Now()
	s.pair.now = func() time.Time { return clock }
	wrong := func() int {
		return do(t, ts, "POST", "/v1/pair", eventsOrigin, "", `{"origin":"`+eventsOrigin+`","code":"xxxxxx"}`).StatusCode
	}
	for i := 0; i < lockoutAfter; i++ {
		if c := wrong(); c != 403 {
			t.Fatalf("wrong code %d: %d", i+1, c)
		}
	}
	// now locked: even the right code is refused with 429 and Retry-After
	res := do(t, ts, "POST", "/v1/pair", eventsOrigin, "", `{"origin":"`+eventsOrigin+`","code":"`+s.pair.Code()+`"}`)
	if res.StatusCode != 429 || res.Header.Get("Retry-After") != "1" {
		t.Fatalf("locked: %d Retry-After=%q", res.StatusCode, res.Header.Get("Retry-After"))
	}
	// the lock grows with each further wrong code, capped at 30 s
	var last time.Duration
	for i := 0; i < 8; i++ {
		clock = clock.Add(time.Hour) // past any lock
		if c := wrong(); c != 403 {
			t.Fatalf("wrong after lock: %d", c)
		}
		s.pair.mu.Lock()
		wait := s.pair.lockedUntil.Sub(clock)
		s.pair.mu.Unlock()
		if wait < last || wait > lockoutMax {
			t.Fatalf("lock %s after %s", wait, last)
		}
		last = wait
	}
	if last != lockoutMax {
		t.Fatalf("the lock should reach %s, got %s", lockoutMax, last)
	}
	clock = clock.Add(time.Hour)
	if tok := pairToken(t, s, ts, eventsOrigin); tok == "" {
		t.Fatal("no token after the lock")
	}
	s.pair.mu.Lock()
	run := s.pair.wrongRun
	s.pair.mu.Unlock()
	if run != 0 {
		t.Fatalf("a right code must reset the count, got %d", run)
	}
}
