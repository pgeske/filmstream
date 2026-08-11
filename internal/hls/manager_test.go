package hls

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestManagerCreatesAndServesIncrementalPlaylist(t *testing.T) {
	ffprobe := writeExecutable(t, "ffprobe", `#!/bin/sh
cat <<'JSON'
{"streams":[{"index":0,"codec_name":"h264","codec_type":"video","side_data_list":[]}],"format":{"duration":"7200.5"}}
JSON
`)
	ffmpeg := writeExecutable(t, "ffmpeg", `#!/bin/sh
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

	stream, err := manager.Start(context.Background(), "playback-1", 120)
	if err != nil {
		t.Fatal(err)
	}
	if stream.VideoCodec != "h264" || stream.DurationSeconds != 7200.5 || stream.StartSeconds != 120 {
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
	manager.Stop(stream.PlaybackID)
	if _, err := manager.AssetPath(stream.PlaybackID, "index.m3u8"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("asset remained after stop: %v", err)
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
	if _, err := manager.Start(context.Background(), "playback-1", 0); err == nil || !strings.Contains(err.Error(), "Dolby Vision") {
		t.Fatalf("error = %v", err)
	}
}

func TestFFmpegArgsPaceInputAndTagHEVC(t *testing.T) {
	manager := &Manager{bufferSeconds: 16, segmentSeconds: 4}
	args := strings.Join(manager.ffmpegArgs("http://source", t.TempDir(), "hevc", 30, []SubtitleTrack{{Index: 4}}), " ")
	for _, expected := range []string{
		"-readrate 1.05", "-readrate_initial_burst 20", "-noaccurate_seek -ss 30.000", "-c:v copy", "-tag:v hvc1", "-c:a aac",
		"-map 0:4 -c:s webvtt -flush_packets 1 -f webvtt",
	} {
		if !strings.Contains(args, expected) {
			t.Fatalf("FFmpeg arguments do not contain %q: %s", expected, args)
		}
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
