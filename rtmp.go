package main

import (
	"bytes"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/yutopp/go-rtmp"
	rtmpmsg "github.com/yutopp/go-rtmp/message"
)

// RTMP connection states shown in /v1/status.
const (
	rtmpIdle     = "idle"
	rtmpOK       = "ok"
	rtmpRetrying = "retrying"
	rtmpFailed   = "failed"
)

// target is where to publish: rtmp(s)://host/app plus the stream key. The key is kept in
// memory only and never printed (String hides it).
type target struct {
	scheme, host, app, key string
}

func parseTarget(rtmpURL, streamKey string) (target, error) {
	u, err := url.Parse(strings.TrimSpace(rtmpURL))
	if err != nil || (u.Scheme != "rtmp" && u.Scheme != "rtmps") || u.Host == "" {
		return target{}, errors.New("rtmpUrl must be rtmp:// or rtmps:// with a host")
	}
	app := strings.Trim(u.Path, "/")
	key := strings.TrimSpace(streamKey)
	if key == "" {
		// Accept a full URL with the key as the last path element (rtmp://host/app/KEY).
		i := strings.LastIndex(app, "/")
		if i < 0 {
			return target{}, errors.New("streamKey is required")
		}
		app, key = app[:i], app[i+1:]
	}
	if app == "" || key == "" {
		return target{}, errors.New("rtmpUrl needs an app path and a streamKey")
	}
	host := u.Host
	if u.Port() == "" {
		if u.Scheme == "rtmps" {
			host += ":443"
		} else {
			host += ":1935"
		}
	}
	return target{scheme: u.Scheme, host: host, app: app, key: key}, nil
}

// String is safe to log: the stream key is replaced by ****.
func (t target) String() string {
	return fmt.Sprintf("%s://%s/%s/****", t.scheme, t.host, t.app)
}

// The RTMP library also logs through logrus's global logger (chunk size changes, and errors that
// may echo command bodies such as the stream name). The relay doesn't use logrus, so silence it.
func init() { logrus.SetOutput(io.Discard) }

// rtmpConnectTimeout bounds one whole connect attempt: dial, RTMP handshake, connect,
// createStream, publish and the metadata write. go-rtmp waits for the server's replies without a
// timeout (context.TODO), so a server that stalls or drops the connection mid-handshake would
// otherwise block the publisher forever and stop every later reconnect.
const rtmpConnectTimeout = 15 * time.Second

var errPublisherClosed = errors.New("publisher closed")

// connect runs one connect attempt with a deadline that done can cut short. On timeout the
// half-open connection is closed and an error returned, so the caller's backoff loop goes on.
// go-rtmp offers no way to cancel its pending wait, so a goroutine stuck inside the library is
// left behind (it closes its connection itself if it ever finishes).
func (t target) connect(done <-chan struct{}, timeout time.Duration) (*rtmp.ClientConn, *rtmp.Stream, error) {
	type result struct {
		conn   *rtmp.ClientConn
		stream *rtmp.Stream
		err    error
	}
	var (
		mu        sync.Mutex
		cur       *rtmp.ClientConn
		abandoned bool
	)
	ch := make(chan result, 1)
	go func() {
		conn, err := t.dial()
		if err != nil {
			ch <- result{err: err}
			return
		}
		mu.Lock()
		if abandoned {
			mu.Unlock()
			_ = conn.Close()
			return
		}
		cur = conn
		mu.Unlock()
		stream, err := t.setup(conn)
		mu.Lock()
		late := abandoned
		mu.Unlock()
		if late { // the caller gave up meanwhile
			_ = conn.Close()
			return
		}
		if err != nil {
			ch <- result{err: err}
			return
		}
		ch <- result{conn: conn, stream: stream}
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	var why error
	select {
	case r := <-ch:
		return r.conn, r.stream, r.err
	case <-timer.C:
		why = fmt.Errorf("rtmp connect to %s timed out after %s", t.host, timeout)
	case <-done:
		why = errPublisherClosed
	}
	mu.Lock()
	abandoned = true
	c := cur
	mu.Unlock()
	if c != nil {
		_ = c.Close()
	}
	select { // it may have finished right at the deadline
	case r := <-ch:
		if r.conn != nil {
			_ = r.conn.Close()
		}
	default:
	}
	return nil, nil, why
}

// dial opens the TCP (or TLS) connection and does the RTMP handshake.
func (t target) dial() (*rtmp.ClientConn, error) {
	quiet := logrus.New()
	quiet.SetOutput(io.Discard) // the library logs the stream name (= the key) at info level
	cfg := &rtmp.ConnConfig{Logger: quiet}
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	var conn *rtmp.ClientConn
	var err error
	if t.scheme == "rtmps" {
		serverName, _, _ := net.SplitHostPort(t.host)
		conn, err = rtmp.DialWithTLSDialer(&tls.Dialer{NetDialer: dialer, Config: &tls.Config{ServerName: serverName, MinVersion: tls.VersionTLS12}}, "rtmps", t.host, cfg)
	} else {
		conn, err = rtmp.DialWithDialer(dialer, "rtmp", t.host, cfg)
	}
	if err != nil {
		return nil, fmt.Errorf("rtmp dial %s: %w", t.host, scrub(err, t.key))
	}
	return conn, nil
}

// setup sends connect, createStream, publish and the metadata on an open connection. On error
// the connection is closed.
func (t target) setup(conn *rtmp.ClientConn) (*rtmp.Stream, error) {
	fail := func(step string, err error) (*rtmp.Stream, error) {
		_ = conn.Close()
		return nil, fmt.Errorf("rtmp %s: %w", step, scrub(err, t.key))
	}
	tcURL := fmt.Sprintf("%s://%s/%s", t.scheme, strings.TrimSuffix(strings.TrimSuffix(t.host, ":1935"), ":443"), t.app)
	if err := conn.Connect(&rtmpmsg.NetConnectionConnect{Command: rtmpmsg.NetConnectionConnectCommand{
		App: t.app, Type: "nonprivate", FlashVer: "FMLE/3.0 (compatible; eventslab-relay)", TCURL: tcURL,
	}}); err != nil {
		return fail("connect", err)
	}
	stream, err := conn.CreateStream(nil, 4096)
	if err != nil {
		return fail("createStream", err)
	}
	if err := stream.Publish(&rtmpmsg.NetStreamPublish{PublishingName: t.key, PublishingType: "live"}); err != nil {
		return fail("publish", err)
	}
	if err := stream.WriteDataMessage(4, 0, "@setDataFrame", &rtmpmsg.NetStreamSetDataFrame{
		AmfData: map[string]interface{}{"videocodecid": 7.0, "audiocodecid": 10.0, "encoder": "eventslab-relay"},
	}); err != nil {
		return fail("metadata", err)
	}
	return stream, nil
}

// scrub removes the key from an error text (libraries may echo the stream name).
func scrub(err error, key string) error {
	if err == nil || key == "" || !strings.Contains(err.Error(), key) {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), key, "****"))
}

// publisher sends FLV tags to RTMP from one writer goroutine and reconnects with backoff
// while the session is alive. If the server is slower than the browser, tags are dropped
// instead of blocking WebRTC (counted in stats).
//
// After a reconnect the AVC/AAC sequence headers are sent again and video waits for the next
// keyframe (needKey is called so the browser sends one now).
type publisher struct {
	target  target
	stats   *stats
	needKey func()
	logf    func(string, ...any)

	ch      chan tag
	done    chan struct{}
	stopped chan struct{} // closed when the writer goroutine has returned
	once    sync.Once

	connectTimeout time.Duration // per connect attempt (rtmpConnectTimeout; shorter in tests)

	mu       sync.Mutex
	state    string
	lastErr  string
	avcCfg   *tag // last AVC sequence header
	aacCfg   *tag // last AAC sequence header
	waitKey  bool // after a reconnect: drop video until a keyframe
	backoffs []time.Duration
}

var defaultBackoffs = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second}

func newPublisher(t target, st *stats, needKey func(), logf func(string, ...any)) *publisher {
	p := &publisher{target: t, stats: st, needKey: needKey, logf: logf,
		ch: make(chan tag, 512), done: make(chan struct{}), stopped: make(chan struct{}), state: rtmpIdle,
		backoffs: defaultBackoffs, connectTimeout: rtmpConnectTimeout}
	return p
}

// Start launches the writer goroutine (which dials first).
func (p *publisher) Start() { go p.loop() }

func (p *publisher) State() (string, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.state, p.lastErr
}

func (p *publisher) setState(s, errText string) {
	p.mu.Lock()
	p.state, p.lastErr = s, errText
	p.mu.Unlock()
}

// Send queues a tag. Sequence headers are remembered so they can be replayed after a reconnect.
func (p *publisher) Send(t tag, isConfig bool) {
	if isConfig {
		p.mu.Lock()
		c := t
		if t.video {
			p.avcCfg = &c
		} else {
			p.aacCfg = &c
		}
		p.mu.Unlock()
	}
	select {
	case <-p.done:
	case p.ch <- t:
	default:
		p.stats.tagsDropped.Add(1)
	}
}

func (p *publisher) loop() {
	defer close(p.stopped)
	attempt := 0
	for {
		select {
		case <-p.done:
			return
		default:
		}
		conn, stream, err := p.target.connect(p.done, p.connectTimeout)
		if errors.Is(err, errPublisherClosed) {
			return
		}
		if err != nil {
			wait := p.backoffs[min(attempt, len(p.backoffs)-1)]
			attempt++
			p.setState(rtmpRetrying, err.Error())
			p.logf("rtmp: %v (retry in %s)", err, wait)
			select {
			case <-p.done:
				return
			case <-time.After(wait):
			}
			continue
		}
		attempt = 0
		p.setState(rtmpOK, "")
		p.logf("rtmp: publishing to %s", p.target)
		p.replayConfig()
		err = p.write(stream)
		_ = stream.Close()
		_ = conn.Close()
		if err == nil { // closed on purpose
			return
		}
		p.stats.rtmpReconnects.Add(1)
		p.setState(rtmpRetrying, err.Error())
		p.logf("rtmp: write failed: %v (reconnecting)", err)
	}
}

// replayConfig queues the remembered sequence headers first and makes video wait for a keyframe.
func (p *publisher) replayConfig() {
	p.mu.Lock()
	avc, aac := p.avcCfg, p.aacCfg
	p.waitKey = true
	p.mu.Unlock()
	// drain stale tags from before the reconnect
	for {
		select {
		case <-p.ch:
			continue
		default:
		}
		break
	}
	if avc != nil {
		p.ch <- *avc
	}
	if aac != nil {
		p.ch <- *aac
	}
	if p.needKey != nil {
		p.needKey()
	}
}

// write sends queued tags until the publisher closes (nil) or a write fails (error).
func (p *publisher) write(stream *rtmp.Stream) error {
	for {
		select {
		case <-p.done:
			return nil
		case t := <-p.ch:
			if t.video && !isVideoConfig(t.data) {
				p.mu.Lock()
				if p.waitKey && !isVideoKeyframe(t.data) {
					p.mu.Unlock()
					continue
				}
				p.waitKey = false
				p.mu.Unlock()
			}
			var msg rtmpmsg.Message
			csid := 5
			if t.video {
				msg, csid = &rtmpmsg.VideoMessage{Payload: bytes.NewReader(t.data)}, 6
			} else {
				msg = &rtmpmsg.AudioMessage{Payload: bytes.NewReader(t.data)}
			}
			if err := stream.Write(csid, t.ts, msg); err != nil {
				return scrub(err, p.target.key)
			}
			p.stats.bytesOut.Add(int64(len(t.data)))
		}
	}
}

func isVideoConfig(d []byte) bool   { return len(d) >= 2 && d[1] == 0 }
func isVideoKeyframe(d []byte) bool { return len(d) >= 1 && d[0]>>4 == 1 }

// Close stops the publisher for good.
func (p *publisher) Close() {
	p.once.Do(func() {
		close(p.done)
		p.setState(rtmpIdle, "")
	})
}
