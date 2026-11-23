package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"sync"
	"time"

	"github.com/pion/webrtc/v4/pkg/media/h264reader"
)

// runRTMPTest publishes a short test pattern to rawURL through the relay's own RTMP path
// (FLV conversion and publisher), without a browser. ffmpeg makes the pattern: H.264 from a
// hardware encoder (VideoToolbox on macOS; NVENC/QSV/AMF/MF on Windows) and AAC. For smoke tests.
func runRTMPTest(rawURL, ffmpeg string, d time.Duration, logf func(string, ...any)) error {
	if ffmpeg == "" {
		return errNoFFmpeg
	}
	t, err := parseTarget(rawURL, "")
	if err != nil {
		return err
	}
	enc, err := pickHardwareEncoder(ffmpeg)
	if err != nil {
		return err
	}
	logf("rtmp-test: %s for %s with %s", t, d, enc)
	st := newStats()
	start := time.Now()
	out := newPublisher(t, st, nil, logf)
	out.backoffs = []time.Duration{500 * time.Millisecond}
	out.Start()
	defer out.Close()

	secs := fmt.Sprint(int(d.Seconds()))
	video := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-nostdin", "-re",
		"-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=30:duration="+secs,
		"-c:v", enc, "-b:v", "2500k", "-g", "60", "-bf", "0", "-profile:v", "high",
		"-bsf:v", "h264_mp4toannexb", "-f", "h264", "pipe:1")
	audio := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-nostdin", "-re",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration="+secs,
		"-ac", "2", "-c:a", "aac", "-b:a", "128k", "-f", "adts", "pipe:1")
	for _, c := range []*exec.Cmd{video, audio} {
		hideWindow(c)
		c.Stderr = prefixWriter{prefix: "ffmpeg", logf: logf}
	}
	vout, err := video.StdoutPipe()
	if err != nil {
		return err
	}
	aout, err := audio.StdoutPipe()
	if err != nil {
		return err
	}
	if err := video.Start(); err != nil {
		return err
	}
	if err := audio.Start(); err != nil {
		_ = video.Process.Kill()
		return err
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		var conv avcConverter
		frame := 0
		splitAccessUnits(vout, func(au []byte) {
			ms := uint32(frame * 1000 / 30)
			frame++
			tags, key := conv.Convert(au, ms)
			for _, tg := range tags {
				cfg := isVideoConfig(tg.data)
				if !cfg {
					st.videoFrames.Add(1)
					if key {
						st.keyframes.Add(1)
					}
				}
				out.Send(tg, cfg)
			}
		})
	}()
	go func() {
		defer wg.Done()
		var n uint64
		sent := false
		_ = readADTSFrames(aout, func(sr, ch byte, body []byte) {
			ts := uint32(n * 1024 * 1000 / aacSampleRate)
			if !sent {
				out.Send(tag{ts: ts, data: aacSequenceHeader(sr, ch)}, true)
				sent = true
			}
			out.Send(tag{ts: ts, data: aacRaw(body)}, false)
			st.audioFrames.Add(1)
			n++
		})
	}()
	wg.Wait()
	_ = video.Wait()
	_ = audio.Wait()
	time.Sleep(500 * time.Millisecond) // let the writer flush
	logf("rtmp-test: done %s", st.Line(time.Since(start)))
	if rs, rerr := out.State(); rs != rtmpOK {
		return fmt.Errorf("rtmp not connected at the end (%s): %s", rs, rerr)
	}
	return nil
}

// pickHardwareEncoder returns the first hardware H.264 encoder that works with this ffmpeg.
// It never falls back to libx264 (owner requirement 2026-11-23: use the hardware encoder).
func pickHardwareEncoder(ffmpeg string) (string, error) {
	candidates := []string{"h264_videotoolbox"}
	if runtime.GOOS == "windows" {
		candidates = []string{"h264_nvenc", "h264_qsv", "h264_amf", "h264_mf"}
	} else if runtime.GOOS == "linux" {
		candidates = []string{"h264_nvenc", "h264_qsv", "h264_vaapi"}
	}
	for _, enc := range candidates {
		cmd := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-nostdin",
			"-f", "lavfi", "-i", "testsrc2=size=320x180:rate=30:duration=0.2", "-c:v", enc, "-f", "null", "-")
		hideWindow(cmd)
		if err := cmd.Run(); err == nil {
			return enc, nil
		}
	}
	return "", errors.New("no hardware H.264 encoder works with this ffmpeg (tried " + fmt.Sprint(candidates) + ")")
}

// splitAccessUnits reads an Annex-B H.264 stream as it arrives and calls fn once per access unit
// (a new AU starts at an AUD or SPS, or at a slice with first_mb_in_slice == 0).
func splitAccessUnits(r io.Reader, fn func([]byte)) {
	rd, err := h264reader.NewReader(r)
	if err != nil {
		return
	}
	var au bytes.Buffer
	hasSlice := false
	flush := func() {
		if au.Len() > 0 && hasSlice {
			fn(append([]byte(nil), au.Bytes()...))
		}
		au.Reset()
		hasSlice = false
	}
	for {
		nal, err := rd.NextNAL()
		if err != nil {
			break
		}
		n := nal.Data
		if len(n) == 0 {
			continue
		}
		typ := n[0] & 0x1f
		newAU := false
		switch typ {
		case 9, 7:
			newAU = hasSlice
		case 1, 5:
			if hasSlice {
				br := bitReader{b: n[1:]}
				if first, ok := br.ue(); ok && first == 0 {
					newAU = true
				}
			}
		}
		if newAU {
			flush()
		}
		au.Write([]byte{0, 0, 0, 1})
		au.Write(n)
		if typ == 1 || typ == 5 {
			hasSlice = true
		}
	}
	flush()
}
