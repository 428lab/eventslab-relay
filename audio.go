package main

import (
	"bufio"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4/pkg/media/oggwriter"
)

// findFFmpeg returns the ffmpeg to use: --ffmpeg if given, else the one on PATH, else one next to
// the relay binary (for a future bundled build). PATH comes first so that a stray "ffmpeg" that
// lands in the Downloads folder next to the relay isn't picked over the user's installed one.
// It returns "" when there is none; /v1/status then reports the problem.
func findFFmpeg(explicit string) string {
	if explicit != "" {
		if _, err := os.Stat(explicit); err == nil {
			return explicit
		}
		return ""
	}
	name := "ffmpeg"
	if runtime.GOOS == "windows" {
		name = "ffmpeg.exe"
	}
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	if exe, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(exe), name)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

var errNoFFmpeg = errors.New("ffmpeg not found (needed for audio: Opus -> AAC). Install ffmpeg on PATH or start with --ffmpeg /path/to/ffmpeg")

// aacTranscoder turns the browser's Opus into AAC-LC with a child ffmpeg (audio only):
// RTP Opus -> Ogg (stdin) -> ffmpeg -> ADTS AAC (stdout) -> FLV AAC tags.
// Browsers cannot send AAC over WebRTC and there is no pure-Go AAC encoder worth shipping,
// so this is the only transcode in the relay (CPU, small). Video never passes through ffmpeg.
type aacTranscoder struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	ogg   *oggwriter.OggWriter
	out   *publisher
	stats *stats
	start time.Time
	logf  func(string, ...any)

	mu       sync.Mutex
	anchorMs uint32
	anchored bool
	closing  bool   // Close was called; an end of ffmpeg's output is expected
	failure  string // why audio stopped, if it stopped on its own (shown in /v1/status)
	once     sync.Once
	readDone chan struct{} // closed when readADTS has returned
}

const aacSampleRate = 48000

func newAACTranscoder(ffmpeg string, out *publisher, st *stats, start time.Time, logf func(string, ...any)) (*aacTranscoder, error) {
	if ffmpeg == "" {
		return nil, errNoFFmpeg
	}
	cmd := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-nostdin",
		"-fflags", "+nobuffer", "-probesize", "4096", "-analyzeduration", "0",
		"-f", "ogg", "-i", "pipe:0",
		"-vn", "-af", "aresample=async=1:first_pts=0", // fill gaps from packet loss so the clock keeps going
		"-c:a", "aac", "-b:a", "128k", "-ar", "48000", "-ac", "2",
		"-flush_packets", "1", "-f", "adts", "pipe:1")
	hideWindow(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = prefixWriter{prefix: "ffmpeg", logf: logf}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	logf("audio: ffmpeg pid=%d (Opus -> AAC)", cmd.Process.Pid)
	ogg, err := oggwriter.NewWith(stdin, 48000, 2)
	if err != nil {
		_ = cmd.Process.Kill()
		return nil, err
	}
	t := &aacTranscoder{cmd: cmd, stdin: stdin, ogg: ogg, out: out, stats: st, start: start, logf: logf, readDone: make(chan struct{})}
	go t.readADTS(stdout)
	return t, nil
}

// WriteRTP feeds one Opus RTP packet.
func (t *aacTranscoder) WriteRTP(pkt *rtp.Packet) error {
	t.mu.Lock()
	if !t.anchored {
		t.anchorMs, t.anchored = uint32(time.Since(t.start)/time.Millisecond), true
	}
	t.mu.Unlock()
	t.stats.audioBytesIn.Add(int64(len(pkt.Payload)))
	return t.ogg.WriteRTP(pkt)
}

// readADTS stamps ffmpeg's AAC frames by sample count from the first Opus packet's arrival
// (the same wall clock the video uses) and sends them to RTMP.
//
// If ffmpeg's output breaks (lost ADTS sync) or ends while the session is still running, audio
// has stopped: the reason goes to /v1/status, ffmpeg is killed so the Opus writer can't block on a
// full stdin pipe, and the rest of its output is drained so ffmpeg can't block on stdout either.
func (t *aacTranscoder) readADTS(r io.Reader) {
	defer close(t.readDone)
	var frames uint64
	sentConfig := false
	err := readADTSFrames(r, func(srIndex, channels byte, body []byte) {
		t.mu.Lock()
		anchor := t.anchorMs
		t.mu.Unlock()
		ts := anchor + uint32(frames*1024*1000/aacSampleRate)
		if !sentConfig {
			t.out.Send(tag{ts: ts, data: aacSequenceHeader(srIndex, channels)}, true)
			sentConfig = true
		}
		t.out.Send(tag{ts: ts, data: aacRaw(body)}, false)
		t.stats.audioFrames.Add(1)
		frames++
	})
	t.mu.Lock()
	closing := t.closing
	t.mu.Unlock()
	if closing && err == nil {
		return
	}
	reason := "ffmpeg stopped"
	if err != nil {
		reason = err.Error()
	}
	t.fail("audio stopped: " + reason)
	_ = t.cmd.Process.Kill()
	_, _ = io.Copy(io.Discard, r)
}

// fail records why audio stopped (the first reason wins) and logs it.
func (t *aacTranscoder) fail(reason string) {
	t.mu.Lock()
	first := t.failure == ""
	if first {
		t.failure = reason
	}
	t.mu.Unlock()
	if first {
		t.logf("audio: %s", reason)
	}
}

// Failure is why audio stopped on its own ("" while it works).
func (t *aacTranscoder) Failure() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.failure
}

// readADTSFrames splits an ADTS stream into raw AAC frames. It returns nil at EOF.
func readADTSFrames(r io.Reader, frame func(srIndex, channels byte, body []byte)) error {
	br := bufio.NewReaderSize(r, 64<<10)
	hdr := make([]byte, 7)
	for {
		if _, err := io.ReadFull(br, hdr); err != nil {
			return nil
		}
		if hdr[0] != 0xff || hdr[1]&0xf0 != 0xf0 {
			return errors.New("lost ADTS sync")
		}
		protAbsent := hdr[1] & 1
		srIndex := (hdr[2] >> 2) & 0x0f
		channels := (hdr[2]&1)<<2 | hdr[3]>>6
		frameLen := int(hdr[3]&3)<<11 | int(hdr[4])<<3 | int(hdr[5])>>5
		if frameLen < 7 {
			return errors.New("bad ADTS frame")
		}
		body := make([]byte, frameLen-7)
		if _, err := io.ReadFull(br, body); err != nil {
			return nil
		}
		if protAbsent == 0 && len(body) >= 2 {
			body = body[2:] // CRC
		}
		frame(srIndex, channels, body)
	}
}

// closeWait bounds each step of Close.
const closeWait = time.Second

// Close stops ffmpeg and returns within about 3 s whatever state ffmpeg is in. stdin is closed
// first: that wakes a WriteRTP stuck on a full pipe on macOS/Linux (it holds the Ogg writer's lock,
// which ogg.Close also needs). Where closing doesn't wake a blocked write (Windows pipes), killing
// ffmpeg does, so the kill deadline doesn't wait for ogg.Close.
func (t *aacTranscoder) Close() {
	t.once.Do(func() {
		t.mu.Lock()
		t.closing = true
		t.mu.Unlock()
		_ = t.stdin.Close()
		oggDone := make(chan struct{})
		go func() { _ = t.ogg.Close(); close(oggDone) }() // the final page can't reach ffmpeg any more; fine
		exited := make(chan struct{})
		go func() { _ = t.cmd.Wait(); close(exited) }()
		select {
		case <-exited:
		case <-time.After(closeWait):
			_ = t.cmd.Process.Kill()
			select {
			case <-exited:
			case <-time.After(closeWait): // a grandchild holding stderr open; don't wait for it
			}
		}
		select {
		case <-oggDone:
		case <-time.After(closeWait):
		}
	})
}

// prefixWriter sends a child process's stderr to the relay log, one call per write.
type prefixWriter struct {
	prefix string
	logf   func(string, ...any)
}

func (p prefixWriter) Write(b []byte) (int, error) {
	p.logf("%s: %s", p.prefix, string(b))
	return len(b), nil
}
