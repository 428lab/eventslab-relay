package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
	"github.com/pion/webrtc/v4/pkg/media/h264reader"
	"github.com/pion/webrtc/v4/pkg/media/oggreader"
	"github.com/yutopp/go-rtmp"
	rtmpmsg "github.com/yutopp/go-rtmp/message"
)

const testKey = "abcd-1234-SECRET-efgh"

// sink is a tiny RTMP server that records what a publisher sends (an FLV file plus counts).
type sink struct {
	mu         sync.Mutex
	key        string
	video, aud int
	keyframes  int
	flv        bytes.Buffer
	publishes  int
	ln         net.Listener
	srv        *rtmp.Server
	conns      []net.Conn
}

// dropConns closes every open publisher connection (simulates the server dropping us).
func (s *sink) dropConns() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.conns {
		_ = c.Close()
	}
	s.conns = nil
}

type sinkHandler struct {
	rtmp.DefaultHandler
	s *sink
}

func (h *sinkHandler) OnPublish(_ *rtmp.StreamContext, _ uint32, cmd *rtmpmsg.NetStreamPublish) error {
	h.s.mu.Lock()
	h.s.key = cmd.PublishingName
	h.s.publishes++
	h.s.mu.Unlock()
	return nil
}

func (h *sinkHandler) OnVideo(ts uint32, payload io.Reader) error {
	b, _ := io.ReadAll(payload)
	h.s.mu.Lock()
	defer h.s.mu.Unlock()
	if !isVideoConfig(b) {
		h.s.video++
		if isVideoKeyframe(b) {
			h.s.keyframes++
		}
	}
	h.s.writeTag(9, ts, b)
	return nil
}

func (h *sinkHandler) OnAudio(ts uint32, payload io.Reader) error {
	b, _ := io.ReadAll(payload)
	h.s.mu.Lock()
	defer h.s.mu.Unlock()
	if len(b) > 1 && b[1] == 1 {
		h.s.aud++
	}
	h.s.writeTag(8, ts, b)
	return nil
}

// writeTag appends an FLV tag (the caller holds mu).
func (s *sink) writeTag(typ byte, ts uint32, body []byte) {
	if s.flv.Len() == 0 {
		s.flv.Write([]byte{'F', 'L', 'V', 1, 5, 0, 0, 0, 9, 0, 0, 0, 0})
	}
	h := []byte{typ, byte(len(body) >> 16), byte(len(body) >> 8), byte(len(body)), byte(ts >> 16), byte(ts >> 8), byte(ts), byte(ts >> 24), 0, 0, 0}
	s.flv.Write(h)
	s.flv.Write(body)
	var prev [4]byte
	binary.BigEndian.PutUint32(prev[:], uint32(11+len(body)))
	s.flv.Write(prev[:])
}

func startSink(t *testing.T) *sink {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &sink{ln: ln}
	s.srv = rtmp.NewServer(&rtmp.ServerConfig{OnConnect: func(c net.Conn) (io.ReadWriteCloser, *rtmp.ConnConfig) {
		s.mu.Lock()
		s.conns = append(s.conns, c)
		s.mu.Unlock()
		return c, &rtmp.ConnConfig{Handler: &sinkHandler{s: s}, ControlState: rtmp.StreamControlStateConfig{DefaultBandwidthWindowSize: 6 * 1024 * 1024 / 8}}
	}})
	go func() { _ = s.srv.Serve(ln) }()
	t.Cleanup(func() { _ = s.srv.Close() })
	return s
}

func (s *sink) counts() (string, int, int, int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.key, s.video, s.keyframes, s.aud, s.publishes
}

// fakeBrowser publishes the H.264 test pattern and the Opus tone over WHIP, like Chrome would.
func fakeBrowser(t *testing.T, whipURL, origin, token string, d time.Duration) (stop func()) {
	t.Helper()
	m := &webrtc.MediaEngine{}
	if err := m.RegisterDefaultCodecs(); err != nil {
		t.Fatal(err)
	}
	// The fake browser offers only loopback candidates: CI machines and dev Macs can have dozens of
	// interfaces (VPN, IPv6), and checking every pair is slow (too slow under -race).
	se := webrtc.SettingEngine{}
	se.SetIncludeLoopbackCandidate(true)
	se.SetIPFilter(func(ip net.IP) bool { return ip.IsLoopback() })
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	api := webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithSettingEngine(se))
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	vt, _ := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000,
		SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f"}, "video", "test")
	at, _ := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}, "audio", "test")
	for _, tr := range []webrtc.TrackLocal{vt, at} {
		if _, err := pc.AddTransceiverFromTrack(tr, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendonly}); err != nil {
			t.Fatal(err)
		}
	}
	offer, _ := pc.CreateOffer(nil)
	gathered := webrtc.GatheringCompletePromise(pc)
	_ = pc.SetLocalDescription(offer)
	<-gathered
	req, _ := http.NewRequest("POST", whipURL, strings.NewReader(pc.LocalDescription().SDP))
	req.Header.Set("Content-Type", "application/sdp")
	req.Header.Set("Origin", origin)
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	answer, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 201 {
		t.Fatalf("whip: %d %s", res.StatusCode, answer)
	}
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: string(answer)}); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { // video: loop the 2 s pattern (Main, no B-frames) at 30 fps
		defer wg.Done()
		data := readTestdata(t, "pattern.h264")
		end := time.Now().Add(d)
		tick := time.NewTicker(time.Second / 30)
		defer tick.Stop()
		for time.Now().Before(end) {
			rd, _ := h264reader.NewReader(bytes.NewReader(data))
			var au []byte
			for {
				nal, err := rd.NextNAL()
				if err != nil {
					break
				}
				au = append(au, 0, 0, 0, 1)
				au = append(au, nal.Data...)
				if typ := nal.UnitType; typ == h264reader.NalUnitTypeCodedSliceIdr || typ == h264reader.NalUnitTypeCodedSliceNonIdr {
					select {
					case <-done:
						return
					case <-tick.C:
					}
					_ = vt.WriteSample(media.Sample{Data: au, Duration: time.Second / 30})
					au = nil
				}
			}
		}
	}()
	go func() { // audio: loop the 2 s Opus tone, 20 ms pages
		defer wg.Done()
		end := time.Now().Add(d)
		for time.Now().Before(end) {
			f, _ := os.Open("testdata/pattern.ogg")
			ogg, _, err := oggreader.NewWith(f)
			if err != nil {
				f.Close()
				return
			}
			var last uint64
			for {
				page, hdr, err := ogg.ParseNextPage()
				if err != nil {
					break
				}
				if hdr.GranulePosition == 0 || len(page) < 2 {
					continue // header pages
				}
				dur := time.Duration(hdr.GranulePosition-last) * time.Second / 48000
				last = hdr.GranulePosition
				select {
				case <-done:
					f.Close()
					return
				case <-time.After(dur):
				}
				_ = at.WriteSample(media.Sample{Data: page, Duration: dur})
			}
			f.Close()
		}
	}()
	return func() {
		close(done)
		wg.Wait()
		_ = pc.Close()
	}
}

// lockedLog collects log lines for the leak check.
type lockedLog struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedLog) logf(format string, args ...any) {
	l.mu.Lock()
	fmt.Fprintf(&l.b, format+"\n", args...)
	l.mu.Unlock()
}

func (l *lockedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// End to end: a Pion "browser" publishes H.264 + Opus over WHIP; the relay copies the video,
// turns the audio into AAC with ffmpeg and publishes RTMP with the key. The output is checked with
// ffprobe when available. The stream key must never reach the logs, and the RTMP publish must
// survive a sink restart (reconnect).
func TestEndToEndWHIPToRTMP(t *testing.T) {
	ffmpeg := findFFmpeg("")
	if ffmpeg == "" {
		t.Skip("ffmpeg not found (needed for the audio path)")
	}
	logs := &lockedLog{}
	sk := startSink(t)
	p, _ := newPairing(filepath.Join(t.TempDir(), "tokens.json"), nil)
	s := &server{pair: p, ffmpeg: ffmpeg, logf: logs.logf, version: "test"}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: s.handler()}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	base := "http://" + ln.Addr().String()
	ts := &testHTTP{base: base}

	token := ts.pair(t, s)
	rtmpURL := "rtmp://" + sk.ln.Addr().String() + "/live"
	if code := ts.post(t, "/v1/start", token, `{"rtmpUrl":"`+rtmpURL+`","streamKey":"`+testKey+`"}`); code != 200 {
		t.Fatalf("start: %d", code)
	}
	stop := fakeBrowser(t, base+"/v1/whip", eventsOrigin, token, 15*time.Second)
	defer stop()

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if _, v, k, a, _ := sk.counts(); v > 60 && k > 0 && a > 50 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	key, v, k, a, _ := sk.counts()
	if v == 0 {
		t.Logf("relay log:\n%s", logs.String())
	}
	if key != testKey {
		t.Fatalf("published with key %q", key)
	}
	if v <= 60 || k == 0 || a <= 50 {
		t.Fatalf("too little media: video=%d keyframes=%d audio=%d", v, k, a)
	}
	var st statusResponse
	ts.getJSON(t, "/v1/status", token, &st)
	if st.State != "live" || st.RTMP != rtmpOK || st.VideoFrames == 0 || st.AudioFrames == 0 {
		t.Fatalf("status: %+v", st)
	}

	// Check the recording with ffprobe: H.264 + AAC, decodes without errors.
	sk.mu.Lock()
	flv := append([]byte(nil), sk.flv.Bytes()...)
	sk.mu.Unlock()
	path := filepath.Join(t.TempDir(), "out.flv")
	_ = os.WriteFile(path, flv, 0o600)
	probe := strings.TrimSuffix(ffmpeg, "ffmpeg") + "ffprobe"
	if out, err := exec.Command(probe, "-v", "error", "-show_entries", "stream=codec_name", "-of", "csv=p=0", path).Output(); err == nil {
		if got := strings.Fields(string(out)); len(got) != 2 || !strings.Contains(string(out), "h264") || !strings.Contains(string(out), "aac") {
			t.Fatalf("ffprobe streams: %q", out)
		}
		if errs, _ := exec.Command(ffmpeg, "-v", "error", "-i", path, "-f", "null", "-").CombinedOutput(); len(bytes.TrimSpace(errs)) > 0 {
			t.Fatalf("decode errors: %s", errs)
		}
	} else {
		t.Logf("ffprobe not available: %v", err)
	}

	if strings.Contains(logs.String(), testKey) {
		t.Fatalf("the stream key leaked into the logs:\n%s", logs.String())
	}
}

// The relay keeps publishing after the RTMP server drops the connection (reconnect with backoff),
// and errors never carry the key.
func TestPublisherReconnects(t *testing.T) {
	logs := &lockedLog{}
	sk := startSink(t)
	tg, _ := parseTarget("rtmp://"+sk.ln.Addr().String()+"/live", testKey)
	st := newStats()
	keyReq := make(chan struct{}, 4)
	p := newPublisher(tg, st, func() { keyReq <- struct{}{} }, logs.logf)
	p.backoffs = []time.Duration{50 * time.Millisecond}
	p.Start()
	defer p.Close()
	waitFor(t, func() bool { _, _, _, _, n := sk.counts(); return n == 1 })
	var conv avcConverter
	sendKey := func(ms uint32) {
		tags, _ := conv.Convert(annexB(testSPS, testPPS, readTestdata(t, "vt_good_idr_head.bin")), ms)
		for _, x := range tags {
			p.Send(x, isVideoConfig(x.data))
		}
	}
	sendKey(0)
	waitFor(t, func() bool { _, _, k, _, _ := sk.counts(); return k == 1 })

	// the server drops the connection
	sk.dropConns()
	// keep sending until a write fails and the publisher reconnects
	waitFor(t, func() bool {
		sendKey(40)
		_, _, _, _, n := sk.counts()
		return n >= 2
	})
	select {
	case <-keyReq: // a keyframe was requested after reconnecting
	case <-time.After(2 * time.Second):
		t.Fatal("no keyframe request after reconnect")
	}
	if st.rtmpReconnects.Load() == 0 {
		t.Fatal("reconnect not counted")
	}
	if strings.Contains(logs.String(), testKey) {
		t.Fatalf("the key leaked: %s", logs.String())
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("timed out")
}

// The key is hidden in target.String and scrubbed from errors.
func TestKeyNeverPrinted(t *testing.T) {
	tg, err := parseTarget("rtmp://a.rtmp.youtube.com/live2", testKey)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(tg.String(), testKey) || !strings.HasSuffix(tg.String(), "/live2/****") {
		t.Fatalf("String: %s", tg.String())
	}
	if e := scrub(errors.New("publish "+testKey+" rejected"), testKey); strings.Contains(e.Error(), testKey) {
		t.Fatalf("scrub: %v", e)
	}
	// a key inside the URL is split off and hidden too
	tg2, err := parseTarget("rtmp://a.rtmp.youtube.com/live2/"+testKey, "")
	if err != nil || tg2.key != testKey || strings.Contains(tg2.String(), testKey) {
		t.Fatalf("key in url: %v %s", err, tg2.String())
	}
}

type testHTTP struct{ base string }

func (h *testHTTP) req(t *testing.T, method, path, token, body, ct string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, h.base+path, strings.NewReader(body))
	req.Header.Set("Origin", eventsOrigin)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func (h *testHTTP) post(t *testing.T, path, token, body string) int {
	res := h.req(t, "POST", path, token, body, "application/json")
	res.Body.Close()
	return res.StatusCode
}

func (h *testHTTP) getJSON(t *testing.T, path, token string, v any) {
	res := h.req(t, "GET", path, token, "", "")
	defer res.Body.Close()
	_ = json.NewDecoder(res.Body).Decode(v)
}

func (h *testHTTP) pair(t *testing.T, s *server) string {
	res := h.req(t, "POST", "/v1/pair", "", `{"origin":"`+eventsOrigin+`","code":"`+s.pair.Code()+`"}`, "application/json")
	defer res.Body.Close()
	var out struct{ Token string }
	_ = json.NewDecoder(res.Body).Decode(&out)
	if out.Token == "" {
		t.Fatalf("pair: %d", res.StatusCode)
	}
	return out.Token
}
