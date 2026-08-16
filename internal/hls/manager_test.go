package hls

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestManagerCreatesAndServesIncrementalPlaylist(t *testing.T) {
	ffprobe := writeExecutable(t, "ffprobe", `#!/bin/sh
case " $* " in
  *" -read_intervals "*) printf '118.500\n'; exit 0 ;;
esac
cat <<'JSON'
{"streams":[{"index":0,"codec_name":"h264","codec_type":"video","side_data_list":[]},{"index":3,"codec_name":"subrip","codec_type":"subtitle","tags":{"language":"eng"}}],"format":{"duration":"7200.5"}}
JSON
`)
	ffmpeg := writeExecutable(t, "ffmpeg", `#!/bin/sh
for last do :; done
dir=$(dirname "$last")
case "$last" in
  *.vtt)
    cat > "$last" <<'SUBTITLES'
WEBVTT

00:02.000 --> 00:04.000
Hello
SUBTITLES
    ;;
  *)
    printf 'init' > "$dir/init.mp4"
    printf 'segment' > "$dir/segment-000000.m4s"
    cat > "$dir/index.m3u8" <<'PLAYLIST'
#EXTM3U
#EXT-X-VERSION:7
#EXT-X-MAP:URI="init.mp4"
#EXTINF:4.0,
segment-000000.m4s
PLAYLIST
    ;;
esac
while :; do sleep 1; done
`)
	manager, err := New(Config{
		DataDir:        t.TempDir(),
		FFmpegPath:     ffmpeg,
		FFprobePath:    ffprobe,
		SourceBaseURL:  "http://127.0.0.1:8943",
		StartupTimeout: 5 * time.Second,
		BufferSeconds:  4,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()

	stream, err := manager.Start(context.Background(), "playback-1", 120, []string{"en", "english"})
	if err != nil {
		t.Fatal(err)
	}
	if stream.VideoCodec != "h264" || stream.DurationSeconds != 7200.5 || stream.StartSeconds != 118.5 || len(stream.Subtitles) != 1 {
		t.Fatalf("stream = %+v", stream)
	}
	path, err := manager.AssetPath(stream.PlaybackID, "index.m3u8")
	if err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(contents), "segment-000000.m4s") {
		t.Fatalf("playlist = %q", contents)
	}
	if _, err := manager.AssetPath(stream.PlaybackID, "subtitle-3.vtt"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("subtitle existed before selection: %v", err)
	}
	if err := manager.StartSubtitle(context.Background(), stream.PlaybackID, 3); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		path, err = manager.AssetPath(stream.PlaybackID, "subtitle-3.vtt")
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("subtitle was not created: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	manager.Stop(stream.PlaybackID)
	if _, err := manager.AssetPath(stream.PlaybackID, "index.m3u8"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("asset remained after stop: %v", err)
	}
}

func TestManagerParksAndReusesPreparedStream(t *testing.T) {
	probeCount := filepath.Join(t.TempDir(), "probe-count")
	packagerCount := filepath.Join(t.TempDir(), "packager-count")
	ffprobe := writeExecutable(t, "ffprobe", fmt.Sprintf(`#!/bin/sh
printf 'x\n' >> %q
cat <<'JSON'
{"streams":[{"index":0,"codec_name":"h264","codec_type":"video","side_data_list":[]}],"format":{"duration":"7200"}}
JSON
`, probeCount))
	ffmpeg := writeExecutable(t, "ffmpeg", fmt.Sprintf(`#!/bin/sh
printf 'x\n' >> %q
for last do :; done
dir=$(dirname "$last")
printf 'init' > "$dir/init.mp4"
printf 'segment' > "$dir/segment-000000.m4s"
cat > "$dir/index.m3u8" <<'PLAYLIST'
#EXTM3U
#EXT-X-VERSION:7
#EXT-X-MAP:URI="init.mp4"
#EXTINF:4.0,
segment-000000.m4s
PLAYLIST
while :; do sleep 1; done
`, packagerCount))
	manager, err := New(Config{
		DataDir: t.TempDir(), FFmpegPath: ffmpeg, FFprobePath: ffprobe,
		SourceBaseURL: "http://127.0.0.1:8943", StartupTimeout: 5 * time.Second,
		BufferSeconds: 4, ParkedTTL: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()

	first, err := manager.Start(t.Context(), "playback-1", 0, []string{"en"})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Park(first.PlaybackID); err != nil {
		t.Fatal(err)
	}
	second, err := manager.Start(t.Context(), "playback-1", 0, []string{"en"})
	if err != nil {
		t.Fatal(err)
	}
	if second.PlaybackID != first.PlaybackID || second.StartSeconds != first.StartSeconds ||
		second.VideoCodec != first.VideoCodec || second.DurationSeconds != first.DurationSeconds {
		t.Fatalf("reused stream = %+v, want %+v", second, first)
	}
	probeCalls, err := os.ReadFile(probeCount)
	if err != nil {
		t.Fatal(err)
	}
	packagerCalls, err := os.ReadFile(packagerCount)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(probeCalls), "x") != 1 || strings.Count(string(packagerCalls), "x") != 1 {
		t.Fatalf("probe calls = %q, packager calls = %q", probeCalls, packagerCalls)
	}

	if err := manager.Park(first.PlaybackID); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		_, err = manager.AssetPath(first.PlaybackID, "index.m3u8")
		if errors.Is(err, os.ErrNotExist) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("parked stream did not expire: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestManagerProbesTextSubtitlesWithoutStartingPlayback(t *testing.T) {
	ffprobe := writeExecutable(t, "ffprobe", `#!/bin/sh
cat <<'JSON'
{"streams":[{"index":2,"codec_name":"subrip","codec_type":"subtitle","tags":{"language":"eng","title":"SDH"}},{"index":3,"codec_name":"hdmv_pgs_subtitle","codec_type":"subtitle"}],"format":{"duration":"7200"}}
JSON
`)
	ffmpeg := writeExecutable(t, "ffmpeg", "#!/bin/sh\nexit 1\n")
	manager, err := New(Config{
		DataDir: t.TempDir(), FFmpegPath: ffmpeg, FFprobePath: ffprobe,
		SourceBaseURL: "http://127.0.0.1:8943",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()

	tracks, err := manager.ProbeSubtitles(t.Context(), "playback-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(tracks) != 1 || tracks[0].Index != 2 || tracks[0].Language != "en" || tracks[0].Title != "SDH" {
		t.Fatalf("tracks = %+v", tracks)
	}
}

func TestManagerRejectsDolbyVision(t *testing.T) {
	ffprobe := writeExecutable(t, "ffprobe", `#!/bin/sh
cat <<'JSON'
{"streams":[{"index":0,"codec_name":"hevc","codec_type":"video","side_data_list":[{"side_data_type":"DOVI configuration record"}]}],"format":{"duration":"100"}}
JSON
`)
	ffmpeg := writeExecutable(t, "ffmpeg", "#!/bin/sh\nexit 1\n")
	manager, err := New(Config{
		DataDir: t.TempDir(), FFmpegPath: ffmpeg, FFprobePath: ffprobe,
		SourceBaseURL: "http://127.0.0.1:8943",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	if _, err := manager.Start(context.Background(), "playback-1", 0, nil); err == nil || !strings.Contains(err.Error(), "Dolby Vision") {
		t.Fatalf("error = %v", err)
	}
}

func TestFFmpegArgsPaceInputAndTagHEVC(t *testing.T) {
	manager := &Manager{bufferSeconds: 16, readRate: 1.25, segmentSeconds: 4}
	args := strings.Join(manager.ffmpegArgs("http://source", t.TempDir(), "hevc", 30, 2), " ")
	for _, expected := range []string{
		"-readrate 1.25", "-readrate_catchup 1.25", "-readrate_initial_burst 20", "-noaccurate_seek -ss 30.000", "-map 0:2?", "-c:v copy", "-tag:v hvc1", "-c:a aac",
	} {
		if !strings.Contains(args, expected) {
			t.Fatalf("FFmpeg arguments do not contain %q: %s", expected, args)
		}
	}
	if strings.Contains(args, "-c:s") {
		t.Fatalf("video packager includes subtitle conversion: %s", args)
	}
}

func TestSubtitleArgsAlignToKeyframeTimeline(t *testing.T) {
	manager := &Manager{bufferSeconds: 16, readRate: 1.25, segmentSeconds: 4}
	stream := &runningStream{
		info:           Stream{StartSeconds: 118.5},
		dir:            t.TempDir(),
		sourceURL:      "http://source",
		requestedStart: 120,
	}
	args := strings.Join(manager.subtitleArgs(stream, 4), " ")
	for _, expected := range []string{
		"-readrate_catchup 1.25", "-noaccurate_seek -ss 120.000", "-map 0:4", "-c:s webvtt",
		"-output_ts_offset 1.500", "-flush_packets 1 -f webvtt",
	} {
		if !strings.Contains(args, expected) {
			t.Fatalf("subtitle arguments do not contain %q: %s", expected, args)
		}
	}
}

func TestPreferredAudioStreamUsesRequestedLanguageBeforeDefault(t *testing.T) {
	probe := mediaProbe{Streams: []mediaStream{
		{Index: 1, CodecType: "audio", Tags: struct {
			Language string `json:"language"`
			Title    string `json:"title"`
		}{Language: "ita", Title: "Italian"}, Disposition: struct {
			Default int `json:"default"`
			Forced  int `json:"forced"`
		}{Default: 1}},
		{Index: 2, CodecType: "audio", Tags: struct {
			Language string `json:"language"`
			Title    string `json:"title"`
		}{Language: "eng", Title: "English"}},
	}}
	selected, found := preferredAudioStream(probe, []string{"en", "english"})
	if !found || selected.Index != 2 {
		t.Fatalf("selected audio = %+v, found = %v", selected, found)
	}
	fallback, found := preferredAudioStream(probe, nil)
	if !found || fallback.Index != 1 {
		t.Fatalf("fallback audio = %+v, found = %v", fallback, found)
	}
}

func TestSupportedSubtitlesFiltersImageTracksAndNormalizesLanguages(t *testing.T) {
	probe := mediaProbe{Streams: []mediaStream{
		{Index: 0, CodecName: "h264", CodecType: "video"},
		{Index: 3, CodecName: "subrip", CodecType: "subtitle", Tags: struct {
			Language string `json:"language"`
			Title    string `json:"title"`
		}{Language: "eng", Title: "SDH"}},
		{Index: 4, CodecName: "hdmv_pgs_subtitle", CodecType: "subtitle"},
	}}
	tracks := supportedSubtitles(probe)
	if len(tracks) != 1 || tracks[0].Index != 3 || tracks[0].Language != "en" || tracks[0].Title != "SDH" {
		t.Fatalf("tracks = %+v", tracks)
	}
}

func TestPlaylistReadyWaitsForStartupBuffer(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "segment-000000.m4s"), []byte("segment"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.m3u8"), []byte("#EXTM3U\n#EXTINF:10.5,\nsegment-000000.m4s\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if playlistReady(dir, 16) {
		t.Fatal("playlist was ready before the startup buffer was available")
	}
	if err := os.WriteFile(filepath.Join(dir, "segment-000001.m4s"), []byte("segment"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.m3u8"), []byte("#EXTM3U\n#EXTINF:10.5,\nsegment-000000.m4s\n#EXTINF:10.5,\nsegment-000001.m4s\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !playlistReady(dir, 16) {
		t.Fatal("playlist was not ready after the startup buffer was available")
	}
}

func TestManagerRejectsUnsafeAssetNames(t *testing.T) {
	manager := &Manager{streams: make(map[string]*runningStream)}
	for _, name := range []string{"../config.json", "ffmpeg.log", "segment-one.ts", "subtitle-all.vtt", "subtitle--1.vtt"} {
		if _, err := manager.AssetPath("playback-1", name); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("asset %q error = %v", name, err)
		}
	}
	if !validAssetName("subtitle-6.vtt") {
		t.Fatal("valid subtitle asset was rejected")
	}
}

func writeExecutable(t *testing.T, name, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(contents), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}
