package main

import (
	"bytes"
	"encoding/binary"
)

// tag is one FLV tag body ready for RTMP (a video or an audio message).
type tag struct {
	video bool
	ts    uint32 // milliseconds
	data  []byte
}

// avcConverter turns Annex-B H.264 access units (as WebRTC delivers them) into FLV/RTMP
// AVC tags: an AVCDecoderConfigurationRecord whenever SPS/PPS change, then length-prefixed
// NAL units. Nothing is re-encoded. Frames before the first real IDR are skipped.
type avcConverter struct {
	sps, pps []byte
	sentSPS  []byte
	sentPPS  []byte
	gotKey   bool
	skipNext bool // the next access unit follows a packet gap: drop it even if it is an IDR
	badIDR   int  // IDR NAL units with a non-intra slice that were dropped
}

// resync is called after RTP loss: the reference chain is broken, so drop everything until the
// next IDR. The access unit right after the gap may itself be a truncated IDR, so skip it too.
func (c *avcConverter) resync() {
	c.gotKey = false
	c.skipNext = true
}

// intraSlice reports whether a slice NAL unit carries an I (or SI) slice: it reads
// first_mb_in_slice and slice_type (both exp-Golomb) after the 1-byte NAL header.
func intraSlice(nal []byte) bool {
	if len(nal) < 2 {
		return false
	}
	r := bitReader{b: nal[1:]}
	if _, ok := r.ue(); !ok {
		return false
	}
	t, ok := r.ue()
	if !ok {
		return false
	}
	t %= 5
	return t == 2 || t == 4 // I or SI
}

type bitReader struct {
	b   []byte
	pos int
}

func (r *bitReader) bit() (uint, bool) {
	if r.pos/8 >= len(r.b) {
		return 0, false
	}
	v := uint(r.b[r.pos/8]>>(7-r.pos%8)) & 1
	r.pos++
	return v, true
}

func (r *bitReader) ue() (uint, bool) {
	zeros := 0
	for {
		b, ok := r.bit()
		if !ok || zeros > 31 {
			return 0, false
		}
		if b == 1 {
			break
		}
		zeros++
	}
	v := uint(0)
	for i := 0; i < zeros; i++ {
		b, ok := r.bit()
		if !ok {
			return 0, false
		}
		v = v<<1 | b
	}
	return (1<<zeros - 1) + v, true
}

func splitAnnexB(b []byte) [][]byte {
	var nalus [][]byte
	i, start := 0, -1
	for i+3 <= len(b) {
		sc := 0
		if b[i] == 0 && b[i+1] == 0 && b[i+2] == 1 {
			sc = 3
		} else if i+4 <= len(b) && b[i] == 0 && b[i+1] == 0 && b[i+2] == 0 && b[i+3] == 1 {
			sc = 4
		}
		if sc == 0 {
			i++
			continue
		}
		if start >= 0 && i > start {
			nalus = append(nalus, b[start:i])
		}
		i += sc
		start = i
	}
	if start >= 0 && start < len(b) {
		nalus = append(nalus, b[start:])
	} else if start < 0 && len(b) > 0 {
		nalus = append(nalus, b) // no start code: a single NAL unit
	}
	return nalus
}

// Convert returns the tags for one access unit and whether it is a keyframe.
func (c *avcConverter) Convert(au []byte, ts uint32) ([]tag, bool) {
	var body bytes.Buffer
	key := false
	for _, n := range splitAnnexB(au) {
		if len(n) == 0 {
			continue
		}
		switch n[0] & 0x1f {
		case 7: // SPS
			c.sps = append([]byte(nil), n...)
			continue
		case 8: // PPS
			c.pps = append([]byte(nil), n...)
			continue
		case 9, 6: // AUD, SEI: not needed in FLV
			continue
		case 5:
			// VideoToolbox (Chrome on macOS) can emit an IDR NAL whose slice is a P slice right
			// after a keyframe request: decoders reject it ("non-intra slice in an IDR"). Only a
			// real I slice starts a clean picture; drop a bogus "IDR" until a real one arrives.
			if !intraSlice(n) {
				c.badIDR++
				c.gotKey = false
				continue
			}
			key = true
		}
		var l [4]byte
		binary.BigEndian.PutUint32(l[:], uint32(len(n)))
		body.Write(l[:])
		body.Write(n)
	}
	var out []tag
	if len(c.sps) >= 4 && c.pps != nil && (!bytes.Equal(c.sps, c.sentSPS) || !bytes.Equal(c.pps, c.sentPPS)) {
		out = append(out, tag{video: true, ts: ts, data: avcSequenceHeader(c.sps, c.pps)})
		c.sentSPS, c.sentPPS = c.sps, c.pps
	}
	if c.skipNext {
		c.skipNext = false
		return out, false
	}
	if key {
		c.gotKey = true
	}
	if body.Len() == 0 || !c.gotKey || c.sentSPS == nil {
		return out, false
	}
	frameType := byte(2) // inter frame
	if key {
		frameType = 1
	}
	hdr := []byte{frameType<<4 | 7, 1, 0, 0, 0} // AVC NALU, composition time 0 (WebRTC has no B-frames)
	out = append(out, tag{video: true, ts: ts, data: append(hdr, body.Bytes()...)})
	return out, key
}

func avcSequenceHeader(sps, pps []byte) []byte {
	b := []byte{1<<4 | 7, 0, 0, 0, 0} // keyframe + AVC, sequence header
	b = append(b, 1, sps[1], sps[2], sps[3], 0xff, 0xe1)
	b = binary.BigEndian.AppendUint16(b, uint16(len(sps)))
	b = append(b, sps...)
	b = append(b, 1)
	b = binary.BigEndian.AppendUint16(b, uint16(len(pps)))
	return append(b, pps...)
}

// h264Profile names the profile in an SPS (for /v1/status), e.g. "High".
func h264Profile(sps []byte) string {
	if len(sps) < 2 {
		return ""
	}
	switch sps[1] {
	case 66:
		return "Baseline"
	case 77:
		return "Main"
	case 100:
		return "High"
	default:
		return "other"
	}
}

// aacSequenceHeader is the FLV AAC sequence header with an AudioSpecificConfig
// for AAC-LC (object type 2).
func aacSequenceHeader(sampleRateIndex, channels byte) []byte {
	asc0 := 2<<3 | sampleRateIndex>>1
	asc1 := (sampleRateIndex&1)<<7 | channels<<3
	return []byte{0xaf, 0, asc0, asc1}
}

func aacRaw(frame []byte) []byte {
	return append([]byte{0xaf, 1}, frame...)
}
