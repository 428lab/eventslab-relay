package main

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

type stats struct {
	videoBytesIn, audioBytesIn atomic.Int64
	videoFrames, keyframes     atomic.Int64
	audioFrames                atomic.Int64
	videoDropped               atomic.Int64 // RTP packets the browser sent that never arrived
	tagsDropped                atomic.Int64 // tags dropped because RTMP was too slow
	resyncs                    atomic.Int64 // waits for an IDR after packet loss
	badIDR                     atomic.Int64 // IDR NAL units carrying a P slice, dropped
	bytesOut                   atomic.Int64
	rtmpReconnects             atomic.Int64

	mu      sync.Mutex
	profile string // H.264 profile seen in the SPS, e.g. "High"
	width   int

	// rate is the last 2-second window, for /v1/status
	rateAt                    time.Time
	rateVideoIn, rateAudioIn  int64
	rateFrames                int64
	videoKbps, audioKbps, fps float64
	lastLineAt                time.Time
	lastLineBytes             int64
}

func newStats() *stats {
	now := time.Now()
	return &stats{rateAt: now, lastLineAt: now}
}

func (s *stats) setProfile(p string) {
	s.mu.Lock()
	s.profile = p
	s.mu.Unlock()
}

// tick updates the rates; called every 2 s by the session.
func (s *stats) tick() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	sec := now.Sub(s.rateAt).Seconds()
	if sec <= 0 {
		return
	}
	v, a, f := s.videoBytesIn.Load(), s.audioBytesIn.Load(), s.videoFrames.Load()
	s.videoKbps = float64(v-s.rateVideoIn) * 8 / 1000 / sec
	s.audioKbps = float64(a-s.rateAudioIn) * 8 / 1000 / sec
	s.fps = float64(f-s.rateFrames) / sec
	s.rateAt, s.rateVideoIn, s.rateAudioIn, s.rateFrames = now, v, a, f
}

type statusSnapshot struct {
	VideoKbps     int    `json:"videoKbps"`
	AudioKbps     int    `json:"audioKbps"`
	FPS           int    `json:"fps"`
	VideoFrames   int64  `json:"videoFrames"`
	Keyframes     int64  `json:"keyframes"`
	AudioFrames   int64  `json:"audioFrames"`
	DroppedFrames int64  `json:"droppedFrames"` // tags dropped because RTMP was too slow
	RTPLost       int64  `json:"rtpLost"`
	Resyncs       int64  `json:"resyncs"`
	BadIDR        int64  `json:"badIdr"`
	Reconnects    int64  `json:"rtmpReconnects"`
	H264Profile   string `json:"h264Profile,omitempty"`
}

func (s *stats) snapshot() statusSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return statusSnapshot{
		VideoKbps: int(s.videoKbps + 0.5), AudioKbps: int(s.audioKbps + 0.5), FPS: int(s.fps + 0.5),
		VideoFrames: s.videoFrames.Load(), Keyframes: s.keyframes.Load(), AudioFrames: s.audioFrames.Load(),
		DroppedFrames: s.tagsDropped.Load(), RTPLost: s.videoDropped.Load(), Resyncs: s.resyncs.Load(),
		BadIDR: s.badIDR.Load(), Reconnects: s.rtmpReconnects.Load(), H264Profile: s.profile,
	}
}

// Line is the periodic log line (kbps out since the previous call).
func (s *stats) Line(elapsed time.Duration) string {
	s.mu.Lock()
	now := time.Now()
	out := s.bytesOut.Load()
	kbps := float64(out-s.lastLineBytes) * 8 / 1000 / now.Sub(s.lastLineAt).Seconds()
	s.lastLineAt, s.lastLineBytes = now, out
	s.mu.Unlock()
	return fmt.Sprintf("t=%.0fs out=%.0fkbps vframes=%d key=%d aframes=%d rtpLost=%d resync=%d badIDR=%d tagDrop=%d reconnects=%d",
		elapsed.Seconds(), kbps, s.videoFrames.Load(), s.keyframes.Load(), s.audioFrames.Load(),
		s.videoDropped.Load(), s.resyncs.Load(), s.badIDR.Load(), s.tagsDropped.Load(), s.rtmpReconnects.Load())
}
