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

// findFFmpeg looks next to the relay binary first (a bundled build), then on PATH.
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
	if exe, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(exe), name)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	return ""
}

var errNoFFmpeg = errors.New("ffmpeg not found (needed for audio: Opus -> AAC). Put ffmpeg next to eventslab-relay or on PATH")

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
	once     sync.Once
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
	t := &aacTranscoder{cmd: cmd, stdin: stdin, ogg: ogg, out: out, stats: st, start: start, logf: logf}
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
func (t *aacTranscoder) readADTS(r io.Reader) {
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
	if err != nil {
		t.logf("audio: %v", err)
	}
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

func (t *aacTranscoder) Close() {
	t.once.Do(func() {
		_ = t.ogg.Close()
		_ = t.stdin.Close()
		done := make(chan struct{})
		go func() { _ = t.cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			_ = t.cmd.Process.Kill()
			<-done
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
