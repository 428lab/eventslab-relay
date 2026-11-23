package main

import (
	"os"
	"testing"
)

func readTestdata(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func annexB(nals ...[]byte) []byte {
	var b []byte
	for _, n := range nals {
		b = append(b, 0, 0, 0, 1)
		b = append(b, n...)
	}
	return b
}

var (
	testSPS = []byte{0x67, 0x64, 0x00, 0x1f, 0xac, 0xd9}
	testPPS = []byte{0x68, 0xeb, 0xe3, 0xcb}
	// a non-IDR P slice: nal type 1, first_mb_in_slice=0, slice_type=0
	testPSlice = []byte{0x41, 0b1_1_000000, 0, 0}
)

// A real VideoToolbox "IDR" carrying a P slice must be dropped (and counted), and video must wait for
// the next real IDR; a real IDR from the same recording starts the picture.
func TestBogusIDRFromVideoToolboxIsDropped(t *testing.T) {
	bad := readTestdata(t, "vt_bad_idr_head.bin")
	good := readTestdata(t, "vt_good_idr_head.bin")
	if bad[0]&0x1f != 5 || good[0]&0x1f != 5 {
		t.Fatal("testdata must be IDR NAL units")
	}
	if intraSlice(bad) {
		t.Fatal("the captured bogus IDR has a P slice: intraSlice must be false")
	}
	if !intraSlice(good) {
		t.Fatal("the captured real IDR has an I slice: intraSlice must be true")
	}

	var c avcConverter
	tags, key := c.Convert(annexB(testSPS, testPPS, bad), 0)
	if key || c.badIDR != 1 {
		t.Fatalf("bogus IDR: key=%v badIDR=%d", key, c.badIDR)
	}
	for _, tg := range tags {
		if !isVideoConfig(tg.data) {
			t.Fatal("no picture may be sent for a bogus IDR (only the sequence header)")
		}
	}
	if tags, _ := c.Convert(annexB(testPSlice), 33); len(tags) != 0 {
		t.Fatal("P frames after a bogus IDR must wait for a real keyframe")
	}
	tags, key = c.Convert(annexB(good), 66)
	if !key || len(tags) != 1 || !isVideoKeyframe(tags[0].data) {
		t.Fatalf("real IDR: key=%v tags=%d", key, len(tags))
	}
	if tags, _ := c.Convert(annexB(testPSlice), 99); len(tags) != 1 {
		t.Fatal("P frames after a real IDR are sent")
	}
}

// After RTP loss the access unit right after the gap is skipped (it may be a truncated IDR) and video
// waits for the next keyframe.
func TestResyncAfterLoss(t *testing.T) {
	good := readTestdata(t, "vt_good_idr_head.bin")
	var c avcConverter
	c.Convert(annexB(testSPS, testPPS, good), 0)
	c.resync()
	if tags, key := c.Convert(annexB(good), 33); key || len(tags) != 0 {
		t.Fatal("the AU right after a gap must be skipped even if it looks like an IDR")
	}
	if tags, _ := c.Convert(annexB(testPSlice), 66); len(tags) != 0 {
		t.Fatal("P frames after a gap must wait for a keyframe")
	}
	if _, key := c.Convert(annexB(good), 99); !key {
		t.Fatal("the next IDR restarts the picture")
	}
}

func TestSequenceHeaderAndProfile(t *testing.T) {
	var c avcConverter
	tags, _ := c.Convert(annexB(testSPS, testPPS, readTestdata(t, "vt_good_idr_head.bin")), 0)
	if len(tags) != 2 || !isVideoConfig(tags[0].data) {
		t.Fatalf("want a sequence header then the keyframe, got %d tags", len(tags))
	}
	if h264Profile(c.sentSPS) != "High" {
		t.Fatalf("profile: %s", h264Profile(c.sentSPS))
	}
}
