package main

import (
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media/samplebuilder"
)

// keyframeInterval: WebRTC encoders send keyframes only on demand, while YouTube wants one at
// least every 4 s. The relay asks with a PLI every 2 s (and right away after a gap).
const keyframeInterval = 2 * time.Second

// session is one WHIP publish: a PeerConnection feeding one RTMP publisher.
type session struct {
	id    string
	pc    *webrtc.PeerConnection
	out   *publisher
	audio *aacTranscoder
	stats *stats
	start time.Time
	logf  func(string, ...any)

	closeOnce sync.Once
	done      chan struct{}
	needKey   chan struct{} // an immediate PLI is wanted
	onClose   func()

	mu      sync.Mutex
	state   string // connecting, live, error
	lastErr string
}

// mediaEngine offers only H.264 (copied into FLV) and Opus: VP8/VP9/AV1 can't go into RTMP/FLV
// without a transcode, so the browser must negotiate H.264 or fail.
func mediaEngine() (*webrtc.MediaEngine, error) {
	m := &webrtc.MediaEngine{}
	// transport-cc: Pion's default interceptors answer with TWCC feedback, which lets the browser's
	// bandwidth estimate climb to the configured bitrate (without it Chrome stays around 1 Mbps).
	fb := []webrtc.RTCPFeedback{{Type: "nack"}, {Type: "nack", Parameter: "pli"}, {Type: "ccm", Parameter: "fir"}, {Type: "transport-cc"}}
	pt := webrtc.PayloadType(102)
	for _, profile := range []string{"640034", "64001f", "4d001f", "42e01f", "42001f"} {
		if err := m.RegisterCodec(webrtc.RTPCodecParameters{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType: webrtc.MimeTypeH264, ClockRate: 90000,
				SDPFmtpLine:  "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=" + profile,
				RTCPFeedback: fb,
			},
			PayloadType: pt,
		}, webrtc.RTPCodecTypeVideo); err != nil {
			return nil, err
		}
		pt++
	}
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2,
			SDPFmtpLine: "minptime=10;useinbandfec=1",
		},
		PayloadType: 111,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		return nil, err
	}
	return m, nil
}

func newAPI() (*webrtc.API, error) {
	m, err := mediaEngine()
	if err != nil {
		return nil, err
	}
	ir := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(m, ir); err != nil {
		return nil, err
	}
	se := webrtc.SettingEngine{}
	// Browser and relay are on the same PC. Chrome often offers only LAN/mDNS host candidates, so
	// the relay gathers on all interfaces plus loopback (as in the prototype). ICE checks are
	// authenticated with the SDP's ufrag/pwd, so other machines can't use these ports.
	se.SetIncludeLoopbackCandidate(true)
	return webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(ir), webrtc.WithSettingEngine(se)), nil
}

// newSession answers the browser's SDP offer and starts publishing to t.
func newSession(id, offer string, t target, ffmpeg string, logf func(string, ...any)) (*session, string, error) {
	api, err := newAPI()
	if err != nil {
		return nil, "", err
	}
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return nil, "", err
	}
	s := &session{id: id, pc: pc, stats: newStats(), start: time.Now(), logf: logf,
		done: make(chan struct{}), needKey: make(chan struct{}, 1), state: "connecting"}
	s.out = newPublisher(t, s.stats, s.requestKeyframe, logf)
	s.audio, err = newAACTranscoder(ffmpeg, s.out, s.stats, s.start, logf)
	if err != nil {
		_ = pc.Close()
		return nil, "", err
	}
	s.out.Start()

	pc.OnTrack(func(track *webrtc.TrackRemote, recv *webrtc.RTPReceiver) {
		codec := track.Codec()
		logf("track: %s", codec.MimeType)
		go drainRTCP(recv)
		switch {
		case strings.EqualFold(codec.MimeType, webrtc.MimeTypeH264):
			go s.keyframeRequests(uint32(track.SSRC()))
			go s.readVideo(track)
		case strings.EqualFold(codec.MimeType, webrtc.MimeTypeOpus):
			go s.readAudio(track)
		}
	})
	pc.OnConnectionStateChange(func(st webrtc.PeerConnectionState) {
		logf("webrtc: %s", st)
		switch st {
		case webrtc.PeerConnectionStateConnected:
			s.setState("live", "")
		case webrtc.PeerConnectionStateFailed:
			s.setState("error", "webrtc connection failed")
			s.Close()
		case webrtc.PeerConnectionStateClosed:
			s.Close()
		}
	})

	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offer}); err != nil {
		s.Close()
		return nil, "", err
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		s.Close()
		return nil, "", err
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		s.Close()
		return nil, "", err
	}
	select { // no trickle ICE: one request, one answer
	case <-gathered:
	case <-time.After(5 * time.Second):
		s.Close()
		return nil, "", errors.New("ICE gathering timed out")
	}
	if !strings.Contains(pc.LocalDescription().SDP, "H264") {
		s.Close()
		return nil, "", errors.New("the browser did not offer H.264")
	}
	go s.tick()
	return s, pc.LocalDescription().SDP, nil
}

func (s *session) setState(st, errText string) {
	s.mu.Lock()
	s.state, s.lastErr = st, errText
	s.mu.Unlock()
}

func (s *session) State() (string, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state, s.lastErr
}

func (s *session) Close() {
	s.closeOnce.Do(func() {
		close(s.done)
		_ = s.pc.Close()
		if s.audio != nil {
			s.audio.Close()
		}
		s.out.Close()
		s.logf("session closed: %s", s.stats.Line(time.Since(s.start)))
		if s.onClose != nil {
			s.onClose()
		}
	})
}

func (s *session) requestKeyframe() {
	select {
	case s.needKey <- struct{}{}:
	default:
	}
}

func drainRTCP(recv *webrtc.RTPReceiver) {
	for {
		if _, _, err := recv.ReadRTCP(); err != nil {
			return
		}
	}
}

// keyframeRequests asks for a keyframe quickly at start (every 500 ms until the first one),
// then every keyframeInterval, and at once when needKey fires.
func (s *session) keyframeRequests(ssrc uint32) {
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	last := time.Time{}
	for {
		select {
		case <-s.done:
			return
		case <-s.needKey:
		case <-t.C:
			if s.stats.keyframes.Load() > 0 && time.Since(last) < keyframeInterval {
				continue
			}
		}
		last = time.Now()
		_ = s.pc.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: ssrc}})
	}
}

func (s *session) readVideo(track *webrtc.TrackRemote) {
	v := newVideoIngest(s.start, s.stats, s.out, s.requestKeyframe)
	for {
		pkt, _, err := track.ReadRTP()
		if err != nil {
			return
		}
		v.Push(pkt)
	}
}

func (s *session) readAudio(track *webrtc.TrackRemote) {
	for {
		pkt, _, err := track.ReadRTP()
		if err != nil {
			return
		}
		if err := s.audio.WriteRTP(pkt); err != nil {
			return
		}
	}
}

func (s *session) tick() {
	rate := time.NewTicker(2 * time.Second)
	line := time.NewTicker(30 * time.Second)
	defer rate.Stop()
	defer line.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-rate.C:
			s.stats.tick()
		case <-line.C:
			s.logf("stats: %s", s.stats.Line(time.Since(s.start)))
		}
	}
}

// videoIngest turns H.264 RTP packets into FLV tags on the publisher. It drops everything after
// packet loss or a bogus IDR until a real keyframe, and asks for one at once (needKey).
type videoIngest struct {
	sb      *samplebuilder.SampleBuilder
	conv    avcConverter
	start   time.Time
	stats   *stats
	out     *publisher
	needKey func()

	started bool
	firstTS uint32
	anchor  time.Duration
}

func newVideoIngest(start time.Time, st *stats, out *publisher, needKey func()) *videoIngest {
	return &videoIngest{sb: samplebuilder.New(512, &codecs.H264Packet{}, 90000), start: start, stats: st, out: out, needKey: needKey}
}

func (v *videoIngest) Push(pkt *rtp.Packet) {
	v.stats.videoBytesIn.Add(int64(len(pkt.Payload)))
	v.sb.Push(pkt)
	for sample := v.sb.Pop(); sample != nil; sample = v.sb.Pop() {
		v.stats.videoDropped.Add(int64(sample.PrevDroppedPackets))
		if sample.PrevDroppedPackets > 0 {
			// A lost packet broke the reference chain: frames until the next IDR would decode as
			// garbage on YouTube. Drop them and ask for a keyframe now instead of waiting.
			v.conv.resync()
			v.stats.resyncs.Add(1)
			v.needKey()
		}
		if !v.started {
			v.firstTS, v.anchor, v.started = sample.PacketTimestamp, time.Since(v.start), true
		}
		ms := uint32(v.anchor/time.Millisecond) + (sample.PacketTimestamp-v.firstTS)/90
		bad := v.conv.badIDR
		tags, key := v.conv.Convert(sample.Data, ms)
		if v.conv.badIDR != bad {
			v.stats.badIDR.Add(1)
			v.needKey()
		}
		for _, tg := range tags {
			cfg := isVideoConfig(tg.data)
			if cfg {
				v.stats.setProfile(h264Profile(v.conv.sentSPS))
			} else {
				v.stats.videoFrames.Add(1)
				if key {
					v.stats.keyframes.Add(1)
				}
			}
			v.out.Send(tg, cfg)
		}
	}
}
