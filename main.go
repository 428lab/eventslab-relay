// eventslab-relay: a small local relay for streaming from events lab to YouTube.
//
// The browser (events lab's live screen) publishes WebRTC (WHIP) to this program on
// 127.0.0.1. Video arrives as H.264 from the browser's hardware encoder and is copied into
// FLV/RTMP as is (no transcode). Audio arrives as Opus and becomes AAC through a child ffmpeg
// (audio only). The stream key is kept in memory and never printed.
package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// version is set at build time with -ldflags "-X main.version=v0.1.0".
var version = "dev"

const (
	basePort = 47801
	lastPort = 47809
)

func main() {
	dev := flag.Bool("dev", false, "also allow http://localhost:* and http://127.0.0.1:* pages (development)")
	port := flag.Int("port", 0, "listen on this port only (default: 47801, then 47802-47809)")
	ffmpegPath := flag.String("ffmpeg", "", "path to ffmpeg (default: next to this program, then PATH)")
	rtmpTest := flag.String("rtmp-test", "", "publish a local test pattern to this RTMP URL and exit (smoke test; needs ffmpeg)")
	testSeconds := flag.Int("rtmp-test-seconds", 10, "length of the --rtmp-test pattern in seconds")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	logger := log.New(os.Stderr, "", log.LstdFlags)
	logf := logger.Printf
	if *showVersion {
		fmt.Println(version)
		return
	}
	ffmpeg := findFFmpeg(*ffmpegPath)

	if *rtmpTest != "" {
		if err := runRTMPTest(*rtmpTest, ffmpeg, time.Duration(*testSeconds)*time.Second, logf); err != nil {
			logf("rtmp-test: %v", err)
			os.Exit(1)
		}
		return
	}

	dir, err := configDir()
	if err != nil {
		logger.Fatalf("config dir: %v", err)
	}
	p, err := newPairing(filepath.Join(dir, "tokens.json"), func(code string) {
		fmt.Printf("ペアリング番号 / Pairing code: %s\n", code)
	})
	if err != nil {
		logger.Fatalf("pairing: %v", err)
	}
	s := &server{pair: p, dev: *dev, ffmpeg: ffmpeg, logf: logf, version: version}

	ln, err := listen(*port)
	if err != nil {
		logger.Fatalf("listen: %v", err)
	}
	fmt.Printf("eventslab-relay %s\n", version)
	fmt.Printf("待ち受け中 / Listening on http://%s\n", ln.Addr())
	fmt.Printf("ペアリング番号 / Pairing code: %s\n", p.Code())
	fmt.Println("events lab の配信画面で「中継アプリとつなぐ」を押し、この番号を入れてください。")
	fmt.Println("In events lab's live screen, press \"Connect relay\" and enter this code.")
	if ffmpeg == "" {
		fmt.Println("注意: ffmpeg が見つかりません（音声の変換に必要です）。/ Warning: ffmpeg not found (needed for audio).")
	}
	if *dev {
		fmt.Println("開発モード: localhost のページからも使えます。/ Dev mode: localhost pages are allowed.")
	}
	srv := &http.Server{Handler: s.handler(), ReadHeaderTimeout: 10 * time.Second}
	logger.Fatal(srv.Serve(ln))
}

// listen binds 127.0.0.1 on the given port, or the first free one of 47801-47809.
func listen(port int) (net.Listener, error) {
	if port != 0 {
		return net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	}
	var errs []string
	for p := basePort; p <= lastPort; p++ {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err == nil {
			return ln, nil
		}
		errs = append(errs, err.Error())
	}
	return nil, errors.New("no free port in 47801-47809: " + strings.Join(errs, "; "))
}
