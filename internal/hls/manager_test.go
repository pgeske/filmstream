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
  *" -read_intervals "*)
    printf '{"packets":[{"pts_time":"118.500","flags":"K__"}]}\n'
    exit 0
    ;;
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

	stream, err := manager.Start(context.Background(), "playback-1", 120, []string{"en", "english"}, -1)
	if err != nil {
		t.Fatal(err)
	}
	if stream.VideoCodec != "h264" || stream.DurationSeconds != 7200.5 ||
		stream.RequestedStartSeconds != 120 || stream.StartSeconds != 118.5 || len(stream.Subtitles) != 1 {
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

func TestManagerUsesCompleteLocalFileForMediaAndKeyframeProbes(t *testing.T) {
	probeSources := filepath.Join(t.TempDir(), "probe-sources")
	ffprobe := writeExecutable(t, "ffprobe", fmt.Sprintf(`#!/bin/sh
for last do :; done
printf '%%s\n' "$last" >> %q
case " $* " in
  *" -read_intervals "*)
    printf '{"packets":[{"pts_time":"118.500","flags":"K__"}]}\n'
    exit 0
    ;;
esac
cat <<'JSON'
{"streams":[{"index":0,"codec_name":"h264","codec_type":"video","side_data_list":[]}],"format":{"duration":"7200"}}
JSON
`, probeSources))
	ffmpeg := writeExecutable(t, "ffmpeg", `#!/bin/sh
for last do :; done
dir=$(dirname "$last")
printf 'init' > "$dir/init.mp4"
printf 'segment' > "$dir/segment-000000.m4s"
cat > "$dir/index.m3u8" <<'PLAYLIST'
#EXTM3U
#EXT-X-MAP:URI="init.mp4"
#EXTINF:4.0,
segment-000000.m4s
PLAYLIST
while :; do sleep 1; done
`)
	manager, err := New(Config{
		DataDir: t.TempDir(), FFmpegPath: ffmpeg, FFprobePath: ffprobe,
		SourceBaseURL: "http://127.0.0.1:8943", StartupTimeout: 5 * time.Second,
		BufferSeconds: 4,
		LocalSourcePath: func(playbackID string) (string, bool) {
			return "/data/complete-episode.mkv", playbackID == "playback-1"
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()

	stream, err := manager.Start(t.Context(), "playback-1", 120, nil, -1)
	if err != nil {
		t.Fatal(err)
	}
	if stream.StartSeconds != 118.5 {
		t.Fatalf("timeline start = %v, want 118.5", stream.StartSeconds)
	}
	sources, err := os.ReadFile(probeSources)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Fields(string(sources)); len(got) != 2 ||
		got[0] != "/data/complete-episode.mkv" || got[1] != "/data/complete-episode.mkv" {
		t.Fatalf("probe sources = %q, want two direct local probes", sources)
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
sleep 0.2
printf 'segment' > "$dir/segment-000001.m4s"
cat >> "$dir/index.m3u8" <<'PLAYLIST'
#EXTINF:4.0,
segment-000001.m4s
PLAYLIST
while :; do sleep 1; done
`, packagerCount))
	manager, err := New(Config{
		DataDir: t.TempDir(), FFmpegPath: ffmpeg, FFprobePath: ffprobe,
		SourceBaseURL: "http://127.0.0.1:8943", StartupTimeout: 5 * time.Second,
		BufferSeconds: 4, ParkedTTL: 50 * time.Millisecond,
		ParkedResumeTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()

	first, err := manager.Start(t.Context(), "playback-1", 0, []string{"en"}, -1)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Park(t.Context(), first.PlaybackID, 4); err != nil {
		t.Fatal(err)
	}
	if !manager.Prepared(first.PlaybackID, 0, []string{"en"}, -1, 4) {
		t.Fatal("parked stream was not reported as prepared")
	}
	second, err := manager.Start(t.Context(), "playback-1", 0, []string{"en"}, -1)
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

	if err := manager.Park(t.Context(), first.PlaybackID, 4); err != nil {
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

func TestManagerRebuildsPreparedStreamWhenSourceDoesNotResume(t *testing.T) {
	packagerCount := filepath.Join(t.TempDir(), "packager-count")
	probeCount := filepath.Join(t.TempDir(), "probe-count")
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
#EXTINF:4.0,
segment-000000.m4s
PLAYLIST
while :; do sleep 1; done
`, packagerCount))
	manager, err := New(Config{
		DataDir: t.TempDir(), FFmpegPath: ffmpeg, FFprobePath: ffprobe,
		SourceBaseURL: "http://127.0.0.1:8943", StartupTimeout: 5 * time.Second,
		BufferSeconds: 4, ParkedResumeTimeout: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()

	first, err := manager.Start(t.Context(), "playback-1", 0, []string{"en"}, -1)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Park(t.Context(), first.PlaybackID, 4); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Start(t.Context(), first.PlaybackID, 0, []string{"en"}, -1); err != nil {
		t.Fatal(err)
	}

	packagerCalls, err := os.ReadFile(packagerCount)
	if err != nil {
		t.Fatal(err)
	}
	probeCalls, err := os.ReadFile(probeCount)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(packagerCalls), "x") != 2 || strings.Count(string(probeCalls), "x") != 1 {
		t.Fatalf("packager calls = %q, probe calls = %q", packagerCalls, probeCalls)
	}
}

func TestManagerJoinsInProgressPreparedStream(t *testing.T) {
	packagerCount := filepath.Join(t.TempDir(), "packager-count")
	ffprobe := writeExecutable(t, "ffprobe", `#!/bin/sh
case " $* " in
  *" -read_intervals "*)
    printf '{"packets":[{"pts_time":"59.000","flags":"K__"}]}\n'
    exit 0
    ;;
esac
cat <<'JSON'
{"streams":[{"index":0,"codec_name":"h264","codec_type":"video","side_data_list":[]}],"format":{"duration":"7200"}}
JSON
`)
	ffmpeg := writeExecutable(t, "ffmpeg", fmt.Sprintf(`#!/bin/sh
printf 'x\n' >> %q
for last do :; done
dir=$(dirname "$last")
printf 'init' > "$dir/init.mp4"
printf 'segment' > "$dir/segment-000000.m4s"
cat > "$dir/index.m3u8" <<'PLAYLIST'
#EXTM3U
#EXT-X-MAP:URI="init.mp4"
#EXTINF:2.0,
segment-000000.m4s
PLAYLIST
sleep 0.2
printf 'segment' > "$dir/segment-000001.m4s"
cat >> "$dir/index.m3u8" <<'PLAYLIST'
#EXTINF:2.0,
segment-000001.m4s
PLAYLIST
while :; do sleep 1; done
`, packagerCount))
	manager, err := New(Config{
		DataDir: t.TempDir(), FFmpegPath: ffmpeg, FFprobePath: ffprobe,
		SourceBaseURL: "http://127.0.0.1:8943", StartupTimeout: 5 * time.Second,
		BufferSeconds: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()

	firstResult := make(chan error, 1)
	go func() {
		_, startErr := manager.Start(t.Context(), "playback-1", 60, []string{"en"}, -1)
		firstResult <- startErr
	}()
	deadline := time.Now().Add(time.Second)
	for {
		if contents, readErr := os.ReadFile(packagerCount); readErr == nil && len(contents) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("initial packager did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}

	if _, err := manager.Start(t.Context(), "playback-1", 60, []string{"en"}, -1); err != nil {
		t.Fatal(err)
	}
	if err := <-firstResult; err != nil {
		t.Fatal(err)
	}
	packagerCalls, err := os.ReadFile(packagerCount)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(packagerCalls), "x") != 1 {
		t.Fatalf("packager calls = %q", packagerCalls)
	}
}

func TestManagerReusesPreparedStreamForPositionWithinPackagedRange(t *testing.T) {
	packagerCount := filepath.Join(t.TempDir(), "packager-count")
	probeCount := filepath.Join(t.TempDir(), "probe-count")
	ffprobe := writeExecutable(t, "ffprobe", fmt.Sprintf(`#!/bin/sh
case " $* " in
  *" -read_intervals "*)
    printf '{"packets":[{"pts_time":"59.000","flags":"K__"}]}\n'
    exit 0
    ;;
esac
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
printf 'segment' > "$dir/segment-000001.m4s"
printf 'segment' > "$dir/segment-000002.m4s"
cat > "$dir/index.m3u8" <<'PLAYLIST'
#EXTM3U
#EXT-X-VERSION:7
#EXT-X-MAP:URI="init.mp4"
#EXTINF:4.0,
segment-000000.m4s
#EXTINF:4.0,
segment-000001.m4s
#EXTINF:4.0,
segment-000002.m4s
PLAYLIST
while :; do sleep 1; done
`, packagerCount))
	manager, err := New(Config{
		DataDir: t.TempDir(), FFmpegPath: ffmpeg, FFprobePath: ffprobe,
		SourceBaseURL: "http://127.0.0.1:8943", StartupTimeout: 5 * time.Second,
		BufferSeconds: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()

	first, err := manager.Start(t.Context(), "playback-1", 0, []string{"en"}, -1)
	if err != nil {
		t.Fatal(err)
	}
	// A recovery request inside the packaged range must reuse the running
	// packager instead of discarding the buffered segments.
	covered, err := manager.Start(t.Context(), "playback-1", 6, []string{"en"}, -1)
	if err != nil {
		t.Fatal(err)
	}
	if covered.StartSeconds != first.StartSeconds || covered.RequestedStartSeconds != first.RequestedStartSeconds {
		t.Fatalf("covered stream = %+v, want original %+v", covered, first)
	}

	// A parked stream resumes for in-range requests instead of rebuilding.
	if err := manager.Park(t.Context(), "playback-1", 4); err != nil {
		t.Fatal(err)
	}
	resumed, err := manager.Start(t.Context(), "playback-1", 3, []string{"en"}, -1)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.StartSeconds != first.StartSeconds || resumed.RequestedStartSeconds != 0 {
		t.Fatalf("resumed covered stream = %+v, want %+v", resumed, first)
	}

	// Positions outside the packaged range still rebuild at the new position.
	restarted, err := manager.Start(t.Context(), "playback-1", 60, []string{"en"}, -1)
	if err != nil {
		t.Fatal(err)
	}
	if restarted.RequestedStartSeconds != 60 {
		t.Fatalf("restarted stream = %+v, want requested start 60", restarted)
	}

	packagerCalls, err := os.ReadFile(packagerCount)
	if err != nil {
		t.Fatal(err)
	}
	probeCalls, err := os.ReadFile(probeCount)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(packagerCalls), "x") != 2 {
		t.Fatalf("packager calls = %q, want one per stream build", packagerCalls)
	}
	if strings.Count(string(probeCalls), "x") != 1 {
		t.Fatalf("probe calls = %q, want the cached source probe only", probeCalls)
	}
}

func TestManagerBoundsProbeAndPackagerWithSourceReadinessFailure(t *testing.T) {
	errUnavailable := errors.New("torrent source readiness budget exhausted")

	t.Run("subtitle probe", func(t *testing.T) {
		ffprobe := writeExecutable(t, "ffprobe", "#!/bin/sh\nexec sleep 10\n")
		ffmpeg := writeExecutable(t, "ffmpeg", "#!/bin/sh\nexit 1\n")
		deadline := time.Now().Add(150 * time.Millisecond)
		manager, err := New(Config{
			DataDir: t.TempDir(), FFmpegPath: ffmpeg, FFprobePath: ffprobe,
			SourceBaseURL: "http://127.0.0.1:8943", StartupTimeout: 5 * time.Second,
			SourceUnavailable: func(string) error {
				if time.Now().After(deadline) {
					return errUnavailable
				}
				return nil
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		defer manager.Close()

		started := time.Now()
		_, err = manager.ProbeSubtitles(t.Context(), "playback-1")
		if !errors.Is(err, errUnavailable) {
			t.Fatalf("subtitle probe error = %v", err)
		}
		if elapsed := time.Since(started); elapsed > time.Second {
			t.Fatalf("subtitle probe ignored source deadline for %s", elapsed)
		}
	})

	t.Run("packager first read", func(t *testing.T) {
		ffprobe := writeExecutable(t, "ffprobe", `#!/bin/sh
cat <<'JSON'
{"streams":[{"index":0,"codec_name":"h264","codec_type":"video","side_data_list":[]}],"format":{"duration":"7200"}}
JSON
`)
		ffmpeg := writeExecutable(t, "ffmpeg", "#!/bin/sh\nexec sleep 10\n")
		deadline := time.Now().Add(150 * time.Millisecond)
		manager, err := New(Config{
			DataDir: t.TempDir(), FFmpegPath: ffmpeg, FFprobePath: ffprobe,
			SourceBaseURL: "http://127.0.0.1:8943", StartupTimeout: 5 * time.Second,
			BufferSeconds: 4,
			SourceUnavailable: func(string) error {
				if time.Now().After(deadline) {
					return errUnavailable
				}
				return nil
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		defer manager.Close()

		started := time.Now()
		_, err = manager.Start(t.Context(), "playback-1", 0, nil, -1)
		if !errors.Is(err, errUnavailable) {
			t.Fatalf("packager start error = %v", err)
		}
		if elapsed := time.Since(started); elapsed > time.Second {
			t.Fatalf("packager ignored source deadline for %s", elapsed)
		}
	})
}

func TestManagerProbesTextAndBitmapSubtitlesWithoutStartingPlayback(t *testing.T) {
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
	if len(tracks) != 2 || tracks[0].Index != 2 || tracks[0].Language != "en" ||
		tracks[0].Title != "SDH" || tracks[0].Kind != "text" ||
		tracks[1].Index != 3 || tracks[1].Kind != "bitmap" {
		t.Fatalf("tracks = %+v", tracks)
	}
}

func TestManagerReusesSubtitleProbeWhenStartingPlayback(t *testing.T) {
	probeCount := filepath.Join(t.TempDir(), "probe-count")
	ffprobe := writeExecutable(t, "ffprobe", fmt.Sprintf(`#!/bin/sh
printf 'x\n' >> %q
cat <<'JSON'
{"streams":[{"index":0,"codec_name":"h264","codec_type":"video","side_data_list":[]},{"index":3,"codec_name":"subrip","codec_type":"subtitle","tags":{"language":"eng"}}],"format":{"duration":"7200"}}
JSON
`, probeCount))
	ffmpeg := writeExecutable(t, "ffmpeg", `#!/bin/sh
for last do :; done
dir=$(dirname "$last")
printf 'init' > "$dir/init.mp4"
printf 'segment' > "$dir/segment-000000.m4s"
cat > "$dir/index.m3u8" <<'PLAYLIST'
#EXTM3U
#EXT-X-MAP:URI="init.mp4"
#EXTINF:4.0,
segment-000000.m4s
PLAYLIST
while :; do sleep 1; done
`)
	manager, err := New(Config{
		DataDir: t.TempDir(), FFmpegPath: ffmpeg, FFprobePath: ffprobe,
		SourceBaseURL: "http://127.0.0.1:8943", StartupTimeout: 5 * time.Second,
		BufferSeconds: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()

	tracks, err := manager.ProbeSubtitles(t.Context(), "playback-1")
	if err != nil || len(tracks) != 1 {
		t.Fatalf("tracks = %+v, error = %v", tracks, err)
	}
	stream, err := manager.Start(t.Context(), "playback-1", 0, []string{"en"}, -1)
	if err != nil {
		t.Fatal(err)
	}
	if len(stream.Subtitles) != 1 || stream.Subtitles[0].Index != tracks[0].Index {
		t.Fatalf("stream subtitles = %+v, probed tracks = %+v", stream.Subtitles, tracks)
	}
	probeCalls, err := os.ReadFile(probeCount)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(probeCalls), "x") != 1 {
		t.Fatalf("probe calls = %q", probeCalls)
	}
}

func TestManagerSharesConcurrentProbeAndClearsItOnStop(t *testing.T) {
	probeCount := filepath.Join(t.TempDir(), "probe-count")
	ffprobe := writeExecutable(t, "ffprobe", fmt.Sprintf(`#!/bin/sh
printf 'x\n' >> %q
sleep 0.1
cat <<'JSON'
{"streams":[{"index":2,"codec_name":"subrip","codec_type":"subtitle"}],"format":{"duration":"7200"}}
JSON
`, probeCount))
	ffmpeg := writeExecutable(t, "ffmpeg", "#!/bin/sh\nexit 1\n")
	manager, err := New(Config{
		DataDir: t.TempDir(), FFmpegPath: ffmpeg, FFprobePath: ffprobe,
		SourceBaseURL: "http://127.0.0.1:8943",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()

	const callers = 8
	start := make(chan struct{})
	results := make(chan error, callers)
	for range callers {
		go func() {
			<-start
			_, probeErr := manager.ProbeSubtitles(t.Context(), "playback-1")
			results <- probeErr
		}()
	}
	close(start)
	for range callers {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	probeCalls, err := os.ReadFile(probeCount)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(probeCalls), "x") != 1 {
		t.Fatalf("concurrent probe calls = %q", probeCalls)
	}

	manager.Stop("playback-1")
	if _, err := manager.ProbeSubtitles(t.Context(), "playback-1"); err != nil {
		t.Fatal(err)
	}
	probeCalls, err = os.ReadFile(probeCount)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(probeCalls), "x") != 2 {
		t.Fatalf("probe calls after stop = %q", probeCalls)
	}
}

func TestManagerStopCancelsInProgressProbe(t *testing.T) {
	probeCount := filepath.Join(t.TempDir(), "probe-count")
	ffprobe := writeExecutable(t, "ffprobe", fmt.Sprintf(`#!/bin/sh
printf 'x\n' >> %q
calls=$(wc -l < %q)
if [ "$calls" -eq 1 ]; then exec sleep 10; fi
cat <<'JSON'
{"streams":[{"index":2,"codec_name":"subrip","codec_type":"subtitle"}],"format":{"duration":"7200"}}
JSON
`, probeCount, probeCount))
	ffmpeg := writeExecutable(t, "ffmpeg", "#!/bin/sh\nexit 1\n")
	manager, err := New(Config{
		DataDir: t.TempDir(), FFmpegPath: ffmpeg, FFprobePath: ffprobe,
		SourceBaseURL: "http://127.0.0.1:8943",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()

	result := make(chan error, 1)
	go func() {
		_, probeErr := manager.ProbeSubtitles(t.Context(), "playback-1")
		result <- probeErr
	}()
	deadline := time.Now().Add(time.Second)
	for {
		if contents, readErr := os.ReadFile(probeCount); readErr == nil && len(contents) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("media probe did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}

	manager.Stop("playback-1")
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("canceled probe succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("canceled probe did not stop")
	}
	if _, err := manager.ProbeSubtitles(t.Context(), "playback-1"); err != nil {
		t.Fatal(err)
	}
	probeCalls, err := os.ReadFile(probeCount)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(probeCalls), "x") != 2 {
		t.Fatalf("probe calls after cancellation = %q", probeCalls)
	}
}

func TestCanceledParkCannotStopPlaybackAfterAutoplayStarts(t *testing.T) {
	ffprobe := writeExecutable(t, "ffprobe", "#!/bin/sh\nexit 1\n")
	ffmpeg := writeExecutable(t, "ffmpeg", "#!/bin/sh\nexit 1\n")
	manager, err := New(Config{
		DataDir: t.TempDir(), FFmpegPath: ffmpeg, FFprobePath: ffprobe,
		SourceBaseURL: "http://127.0.0.1:8943",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()

	// Holding the lifecycle lock models autoplay entering Start before the
	// prewarmer reaches Park. Once the claim cancels Park, it must not run late.
	unlockStart := manager.lockPlaybackStart("next-playback")
	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)
	go func() {
		result <- manager.Park(ctx, "next-playback", 30)
	}()
	cancel()
	unlockStart()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("park error = %v, want context cancellation", err)
	}
}

func TestManagerDefaultsToTwelveSecondStartupBuffer(t *testing.T) {
	ffprobe := writeExecutable(t, "ffprobe", "#!/bin/sh\nexit 1\n")
	ffmpeg := writeExecutable(t, "ffmpeg", "#!/bin/sh\nexit 1\n")
	manager, err := New(Config{
		DataDir: t.TempDir(), FFmpegPath: ffmpeg, FFprobePath: ffprobe,
		SourceBaseURL: "http://127.0.0.1:8943",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	if manager.bufferSeconds != 12 {
		t.Fatalf("startup buffer = %d", manager.bufferSeconds)
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
	if _, err := manager.Start(context.Background(), "playback-1", 0, nil, -1); err == nil || !strings.Contains(err.Error(), "Dolby Vision") {
		t.Fatalf("error = %v", err)
	}
}

func TestFFmpegArgsPaceInputAndTagHEVC(t *testing.T) {
	manager := &Manager{bufferSeconds: 16, readRate: 1.25, segmentSeconds: 4}
	args := strings.Join(manager.ffmpegArgs("http://source", t.TempDir(), "hevc", 30, 2, -1), " ")
	for _, expected := range []string{
		"-copyts -start_at_zero", "-readrate 1.25", "-readrate_initial_burst 20",
		"-noaccurate_seek -ss 30.000", "-map 0:2?", "-c:v copy", "-tag:v hvc1",
		"-c:a aac", "-output_ts_offset -30.000", "-avoid_negative_ts make_zero",
	} {
		if !strings.Contains(args, expected) {
			t.Fatalf("FFmpeg arguments do not contain %q: %s", expected, args)
		}
	}
	if strings.Contains(args, "-c:s") {
		t.Fatalf("video packager includes subtitle conversion: %s", args)
	}
}

func TestFFmpegArgsBurnBitmapSubtitlesWithNVENC(t *testing.T) {
	manager := &Manager{
		bufferSeconds: 16, readRate: 1.25, segmentSeconds: 4,
		bitmapSubtitleEncoder: "h264_nvenc",
	}
	args := strings.Join(manager.ffmpegArgs("http://source", t.TempDir(), "hevc", 30, 2, 5), " ")
	for _, expected := range []string{
		"-copyts -start_at_zero", "-filter_complex [0:v:0][0:5]overlay=eof_action=pass[v]",
		"-map [v]", "-map 0:2?", "-c:v h264_nvenc", "-preset p5", "-cq 18",
		"-force_key_frames expr:if(isnan(prev_forced_t),1,gte(t,prev_forced_t+4))",
		"-output_ts_offset -30.000",
	} {
		if !strings.Contains(args, expected) {
			t.Fatalf("bitmap subtitle arguments do not contain %q: %s", expected, args)
		}
	}
	if strings.Contains(args, "-c:v copy") || strings.Contains(args, "-tag:v hvc1") {
		t.Fatalf("bitmap subtitle packager stream-copies video: %s", args)
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
		"-readrate 1.25", "-noaccurate_seek -ss 120.000", "-map 0:4", "-c:s webvtt",
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
		{Index: 3, CodecType: "audio", Tags: struct {
			Language string `json:"language"`
			Title    string `json:"title"`
		}{Language: "jpn", Title: "Japanese"}},
	}}
	japanese, found := preferredAudioStream(probe, []string{"ja", "en", "english"})
	if !found || japanese.Index != 3 {
		t.Fatalf("selected Japanese audio = %+v, found = %v", japanese, found)
	}
	selected, found := preferredAudioStream(probe, []string{"en", "english"})
	if !found || selected.Index != 2 {
		t.Fatalf("selected audio = %+v, found = %v", selected, found)
	}
	fallback, found := preferredAudioStream(probe, nil)
	if !found || fallback.Index != 1 {
		t.Fatalf("fallback audio = %+v, found = %v", fallback, found)
	}
}

func TestManagerCorrectsTimelineStartWhenKeyframeProbeFails(t *testing.T) {
	// Model a cold torrent source: the keyframe probe fails until the packager
	// has read through the seek region, after which the probe sees the keyframe
	// the -ss seek actually landed on.
	marker := filepath.Join(t.TempDir(), "warmed")
	ffprobe := writeExecutable(t, "ffprobe", fmt.Sprintf(`#!/bin/sh
case " $* " in
  *" -read_intervals "*)
    if [ ! -f %q ]; then
      # Cold source: ranging this far into the file fails until the packager warms it.
      exit 1
    fi
    printf '{"packets":[{"pts_time":"42.000","flags":"K__"}]}\n'
    exit 0
    ;;
esac
cat <<'JSON'
{"streams":[{"index":0,"codec_name":"h264","codec_type":"video","side_data_list":[]},{"index":3,"codec_name":"subrip","codec_type":"subtitle","tags":{"language":"eng"}}],"format":{"duration":"7200"}}
JSON
`, marker))
	ffmpeg := writeExecutable(t, "ffmpeg", fmt.Sprintf(`#!/bin/sh
for last do :; done
dir=$(dirname "$last")
printf warmed > %q
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
`, marker))
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

	stream, err := manager.Start(t.Context(), "playback-1", 43.7, []string{"en"}, -1)
	if err != nil {
		t.Fatal(err)
	}
	// The packager seeked to 43.7 and landed on the 42.0 keyframe; the reported
	// timeline start must match the landing point or subtitles desync by the gap.
	if stream.RequestedStartSeconds != 43.7 || stream.StartSeconds != 42.0 {
		t.Fatalf("stream = %+v, want start 42.0 for requested 43.7", stream)
	}
	manager.mu.RLock()
	packaged := manager.streams["playback-1"]
	manager.mu.RUnlock()
	args := strings.Join(manager.subtitleArgs(packaged, 3), " ")
	for _, expected := range []string{"-noaccurate_seek -ss 43.700", "-output_ts_offset 1.700"} {
		if !strings.Contains(args, expected) {
			t.Fatalf("subtitle arguments do not contain %q: %s", expected, args)
		}
	}
}

func TestManagerDoesNotReturnUnsynchronizedStreamAfterReprobeFailure(t *testing.T) {
	ffprobe := writeExecutable(t, "ffprobe", `#!/bin/sh
case " $* " in
  *" -read_intervals "*) exit 1 ;;
esac
cat <<'JSON'
{"streams":[{"index":0,"codec_name":"h264","codec_type":"video","side_data_list":[]}],"format":{"duration":"7200"}}
JSON
`)
	ffmpeg := writeExecutable(t, "ffmpeg", `#!/bin/sh
for last do :; done
dir=$(dirname "$last")
printf 'init' > "$dir/init.mp4"
printf 'segment' > "$dir/segment-000000.m4s"
cat > "$dir/index.m3u8" <<'PLAYLIST'
#EXTM3U
#EXT-X-MAP:URI="init.mp4"
#EXTINF:4.0,
segment-000000.m4s
PLAYLIST
while :; do sleep 1; done
`)
	manager, err := New(Config{
		DataDir: t.TempDir(), FFmpegPath: ffmpeg, FFprobePath: ffprobe,
		SourceBaseURL: "http://127.0.0.1:8943", StartupTimeout: 5 * time.Second,
		BufferSeconds: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()

	_, err = manager.Start(t.Context(), "playback-1", 43.7, nil, -1)
	if err == nil || !strings.Contains(err.Error(), "reprobe HLS keyframe start") {
		t.Fatalf("start error = %v, want reprobe failure", err)
	}
	manager.mu.RLock()
	stream := manager.streams["playback-1"]
	manager.mu.RUnlock()
	if stream != nil {
		t.Fatal("unsynchronized HLS stream remained available after reprobe failure")
	}
}

func TestManagerDoesNotRetryDeterministicInvalidKeyframe(t *testing.T) {
	probeCount := filepath.Join(t.TempDir(), "probe-count")
	packagerCount := filepath.Join(t.TempDir(), "packager-count")
	ffprobe := writeExecutable(t, "ffprobe", fmt.Sprintf(`#!/bin/sh
case " $* " in
  *" -read_intervals "*)
    printf 'x\n' >> %q
    printf '{"packets":[{"pts_time":"2830.828","flags":"K__"}]}\n'
    exit 0
    ;;
esac
cat <<'JSON'
{"streams":[{"index":0,"codec_name":"h264","codec_type":"video","side_data_list":[]}],"format":{"duration":"2830.828"}}
JSON
`, probeCount))
	ffmpeg := writeExecutable(t, "ffmpeg", fmt.Sprintf("#!/bin/sh\nprintf 'x\\n' >> %q\nexit 1\n", packagerCount))
	manager, err := New(Config{
		DataDir: t.TempDir(), FFmpegPath: ffmpeg, FFprobePath: ffprobe,
		SourceBaseURL: "http://127.0.0.1:8943", StartupTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()

	_, err = manager.Start(t.Context(), "playback-1", 1875.616, nil, -1)
	if err == nil || !strings.Contains(err.Error(), `invalid video keyframe timestamp "2830.828"`) {
		t.Fatalf("start error = %v, want invalid keyframe", err)
	}
	probeCalls, readErr := os.ReadFile(probeCount)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if strings.Count(string(probeCalls), "x") != 1 {
		t.Fatalf("keyframe probe calls = %q, want one", probeCalls)
	}
	if _, statErr := os.Stat(packagerCount); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("packager ran after deterministic probe failure: %v", statErr)
	}
}

func TestProbeTimelineStartSelectsLatestValidKeyframeInOnePass(t *testing.T) {
	probeCount := filepath.Join(t.TempDir(), "probe-count")
	ffprobe := writeExecutable(t, "ffprobe", fmt.Sprintf(`#!/bin/sh
printf 'x\n' >> %q
case " $* " in
  *" 509.000%%569.500 "*) ;;
  *) exit 2 ;;
esac
cat <<'JSON'
{"packets":[{"pts_time":"540.540","flags":"K__"},{"pts_time":"568.568","flags":"K__"},{"pts_time":"632.382","flags":"K__"}]}
JSON
`, probeCount))
	ffmpeg := writeExecutable(t, "ffmpeg", "#!/bin/sh\nexit 0\n")
	manager, err := New(Config{
		DataDir: t.TempDir(), FFmpegPath: ffmpeg, FFprobePath: ffprobe,
		SourceBaseURL: "http://127.0.0.1:8943",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()

	start, err := manager.probeTimelineStart(t.Context(), "http://source", 569)
	if err != nil || start != 568.568 {
		t.Fatalf("timeline start = %v, error = %v", start, err)
	}
	probeCalls, err := os.ReadFile(probeCount)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(probeCalls), "x") != 1 {
		t.Fatalf("keyframe probe calls = %q, want one", probeCalls)
	}
}

func TestSupportedSubtitlesIncludesImageTracksAndNormalizesLanguages(t *testing.T) {
	probe := mediaProbe{Streams: []mediaStream{
		{Index: 0, CodecName: "h264", CodecType: "video"},
		{Index: 3, CodecName: "subrip", CodecType: "subtitle", Tags: struct {
			Language string `json:"language"`
			Title    string `json:"title"`
		}{Language: "eng", Title: "SDH"}},
		{Index: 4, CodecName: "hdmv_pgs_subtitle", CodecType: "subtitle"},
	}}
	tracks := supportedSubtitles(probe)
	if len(tracks) != 2 || tracks[0].Index != 3 || tracks[0].Language != "en" ||
		tracks[0].Title != "SDH" || tracks[0].Kind != "text" ||
		tracks[1].Index != 4 || tracks[1].Kind != "bitmap" {
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

func TestPlaylistReadyAcceptsShortCompletedStream(t *testing.T) {
	dir := t.TempDir()
	playlist := "#EXTM3U\n#EXTINF:2.0,\nsegment-000000.m4s\n#EXT-X-ENDLIST\n"
	if err := os.WriteFile(filepath.Join(dir, "index.m3u8"), []byte(playlist), 0o600); err != nil {
		t.Fatal(err)
	}
	if playlistReady(dir, 24) {
		t.Fatal("playlist was ready before its completed segment existed")
	}
	if err := os.WriteFile(filepath.Join(dir, "segment-000000.m4s"), []byte("segment"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !playlistReady(dir, 24) {
		t.Fatal("completed playlist near the end of the media was not ready")
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
