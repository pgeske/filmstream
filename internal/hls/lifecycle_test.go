package hls

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The producer emits a valid startup buffer, then makes no further progress.
// Gates let tests stop it during timeline verification without real media/network I/O.
func newLifecycleTestManager(t *testing.T, sourceStalled func(string, error) error, probeGate string) *Manager {
	t.Helper()
	ffprobe := writeExecutable(t, "ffprobe", fmt.Sprintf(`#!/bin/sh
case " $* " in
  *" concat:"*)
    if [ -n %q ]; then
      touch %q
      while [ ! -f %q ]; do sleep 0.01; done
    fi
    printf '{"packets":[{"stream_index":0,"pts_time":"0.000","dts_time":"0.000","flags":"K__"}],"streams":[{"index":0,"codec_type":"video","start_time":"0.000"}]}\n'
    ;;
  *) printf '{"streams":[{"index":0,"codec_name":"h264","codec_type":"video"}],"format":{"duration":"7200"}}\n' ;;
esac
`, probeGate, probeGate+".entered", probeGate))
	ffmpeg := writeExecutable(t, "ffmpeg", `#!/bin/sh
for last do :; done
dir=$(dirname "$last")
printf init > "$dir/init.mp4"
printf '#EXTM3U\n#EXT-X-MAP:URI="init.mp4"\n' > "$dir/index.m3u8"
for number in 000000 000001 000002; do
  printf segment > "$dir/segment-$number.m4s"
  printf '#EXTINF:4.0,\nsegment-%s.m4s\n' "$number" >> "$dir/index.m3u8"
done
exec sleep 60
`)
	manager, err := New(Config{
		DataDir: t.TempDir(), FFmpegPath: ffmpeg, FFprobePath: ffprobe,
		SourceBaseURL: "http://127.0.0.1:1", StartupTimeout: 3 * time.Second,
		BufferSeconds: 4, ParkedResumeTimeout: 150 * time.Millisecond,
		SourceStalled: sourceStalled,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	return manager
}

func TestCanceledRecoveryDoesNotDestroySharedProducer(t *testing.T) {
	for _, position := range []float64{0, 6} {
		for _, deadline := range []bool{false, true} {
			t.Run(fmt.Sprintf("position=%g/deadline=%v", position, deadline), func(t *testing.T) {
				marked := false
				manager := newLifecycleTestManager(t, func(string, error) error {
					marked = true
					return errors.New("source quarantined")
				}, "")
				if _, err := manager.Start(t.Context(), "shared", 0, nil, -1); err != nil {
					t.Fatal(err)
				}
				original := manager.streams["shared"]
				path, err := manager.AssetPath("shared", "index.m3u8")
				if err != nil {
					t.Fatal(err)
				}
				stale := time.Now().Add(-time.Minute)
				if err := os.Chtimes(path, stale, stale); err != nil {
					t.Fatal(err)
				}

				// A client gives up during recovery; that is not a producer failure.
				ctx, cancel := context.WithCancel(t.Context())
				want := context.Canceled
				if deadline {
					cancel()
					ctx, cancel = context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
					want = context.DeadlineExceeded
				}
				defer cancel()
				cancel()
				_, err = manager.Start(ctx, "shared", position, nil, -1)
				if !errors.Is(err, want) {
					t.Fatalf("recovery error = %v, want %v", err, want)
				}
				if marked || manager.streams["shared"] != original {
					t.Fatal("canceled waiter quarantined or replaced another consumer's producer")
				}
				if original.ctx.Err() != nil {
					t.Fatal("canceled waiter canceled the producer")
				}
			})
		}
	}
}

func TestStopCancelsUnpublishedStartupAndAllowsReplay(t *testing.T) {
	gate := filepath.Join(t.TempDir(), "verify")
	manager := newLifecycleTestManager(t, nil, gate)
	result := make(chan error, 1)
	go func() {
		_, err := manager.Start(t.Context(), "replay", 0, nil, -1)
		result <- err
	}()
	waitForLifecycleFile(t, gate+".entered")
	manager.Stop("replay")
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("stopped startup returned %v, want cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("stop did not cancel in-flight timeline verification")
	}
	if _, err := manager.AssetPath("replay", "index.m3u8"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stopped startup published assets: %v", err)
	}
	if _, err := manager.Start(t.Context(), "replay", 0, nil, -1); err != nil {
		t.Fatalf("new generation could not replay: %v", err)
	}
}

func TestLateStopCleanupCannotDeleteReplayAssets(t *testing.T) {
	manager := newLifecycleTestManager(t, nil, "")
	if _, err := manager.Start(t.Context(), "replay", 0, nil, -1); err != nil {
		t.Fatal(err)
	}
	old := manager.streams["replay"]
	stopping, release := make(chan struct{}), make(chan struct{})
	cancel := old.cancel
	// Model a slow child-process teardown after its generation is detached.
	old.cancel = func() {
		close(stopping)
		<-release
		cancel()
	}
	stopped := make(chan struct{})
	go func() { manager.Stop("replay"); close(stopped) }()
	<-stopping
	_, err := manager.Start(t.Context(), "replay", 0, nil, -1)
	close(release)
	<-stopped
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AssetPath("replay", "index.m3u8"); err != nil {
		t.Fatalf("old generation's cleanup removed replacement assets: %v", err)
	}
	if manager.streams["replay"].ctx.Err() != nil {
		t.Fatal("old generation canceled the replacement producer")
	}
}

func TestExitedProducerCannotPassReadinessFromCachedBuffer(t *testing.T) {
	manager := newLifecycleTestManager(t, nil, "")
	if _, err := manager.Start(t.Context(), "failed", 0, nil, -1); err != nil {
		t.Fatal(err)
	}
	stream := manager.streams["failed"]
	if err := stream.command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	<-stream.done
	if err := manager.waitUntilReady(t.Context(), stream); err == nil {
		t.Fatal("dead producer's startup buffer was reported ready")
	}
	if _, err := manager.AssetPath("failed", "index.m3u8"); err == nil {
		t.Fatal("dead producer's live playlist was served as healthy")
	}
}

func TestSlowProducerRecoveryKeepsConsumerAndProducerOutcomesSeparate(t *testing.T) {
	for _, outcome := range []string{"advances", "waiter_cancels", "producer_stalls"} {
		t.Run(outcome, func(t *testing.T) {
			unavailable := errors.New("fixture source unavailable")
			var marked string
			manager := newLifecycleTestManager(t, func(id string, _ error) error {
				marked = id
				return unavailable
			}, "")
			manager.parkedResumeTimeout = time.Second
			for _, id := range []string{"slow", "unrelated"} {
				if _, err := manager.Start(t.Context(), id, 0, nil, -1); err != nil {
					t.Fatal(err)
				}
			}
			original := manager.streams["slow"]
			unrelated := manager.streams["unrelated"]
			path, err := manager.AssetPath("slow", "index.m3u8")
			if err != nil {
				t.Fatal(err)
			}
			stale := time.Now().Add(-time.Minute)
			if err := os.Chtimes(path, stale, stale); err != nil {
				t.Fatal(err)
			}
			if status, ok := manager.Status("slow"); !ok || status.State != "buffering" || status.PackagedSeconds != 12 {
				t.Fatalf("slow producer status = %+v, exists=%v", status, ok)
			}
			if manager.Prepared("slow", 0, nil, -1, 4) {
				t.Fatal("old startup buffer was reported prepared despite no live progress")
			}

			// The growth check is a synchronization point, not a timing guess.
			checking := make(chan struct{})
			var once sync.Once
			manager.logger = slog.New(slog.NewTextHandler(lifecycleLogWriter(func(line string) {
				if strings.Contains(line, "checking HLS playlist growth") {
					once.Do(func() { close(checking) })
				}
			}), nil))
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			result := make(chan error, 1)
			go func() {
				_, err := manager.Start(ctx, "slow", 0, nil, -1)
				result <- err
			}()
			select {
			case <-checking:
			case <-time.After(2 * time.Second):
				t.Fatal("recovery did not check the cached producer's progress")
			}
			if outcome == "waiter_cancels" {
				cancel()
			}
			if outcome == "advances" {
				// Publish another committed segment, just as a slow packager would.
				if err := os.WriteFile(filepath.Join(original.dir, "segment-000003.m4s"), []byte("segment"), 0o600); err != nil {
					t.Fatal(err)
				}
				contents, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				contents = append(contents, []byte("#EXTINF:4.0,\nsegment-000003.m4s\n")...)
				if err := os.WriteFile(path+".tmp", contents, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(path+".tmp", path); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case err = <-result:
			case <-time.After(2 * time.Second):
				t.Fatal("recovery did not finish")
			}
			switch outcome {
			case "advances":
				if err != nil || manager.streams["slow"] != original || marked != "" {
					t.Fatalf("slow, healthy producer was rebuilt or quarantined: %v, marked=%q", err, marked)
				}
			case "waiter_cancels":
				if !errors.Is(err, context.Canceled) || manager.streams["slow"] != original || marked != "" || original.ctx.Err() != nil {
					t.Fatalf("canceling recovery damaged the retained producer: %v, marked=%q", err, marked)
				}
			case "producer_stalls":
				if !errors.Is(err, unavailable) || manager.streams["slow"] != nil || marked != "slow" {
					t.Fatalf("stalled cached producer was reused: %v, marked=%q", err, marked)
				}
			}
			if manager.streams["unrelated"] != unrelated || unrelated.ctx.Err() != nil {
				t.Fatal("recovery disturbed an unrelated playback")
			}
		})
	}
}

func TestCompletedProducerRemainsPlayableAndReusable(t *testing.T) {
	manager := newLifecycleTestManager(t, nil, "")
	manager.ffmpegPath = writeExecutable(t, "completed-ffmpeg", `#!/bin/sh
for last do :; done
dir=$(dirname "$last")
printf init > "$dir/init.mp4"
printf segment > "$dir/segment-000000.m4s"
printf '#EXTM3U\n#EXT-X-MAP:URI="init.mp4"\n#EXTINF:2.0,\nsegment-000000.m4s\n#EXT-X-ENDLIST\n' > "$dir/index.m3u8"
`)
	if _, err := manager.Start(t.Context(), "complete", 0, nil, -1); err != nil {
		t.Fatal(err)
	}
	stream := manager.streams["complete"]
	<-stream.done
	if status, _ := manager.Status("complete"); status.State != "complete" {
		t.Fatalf("completed producer status = %+v", status)
	}
	if !manager.Prepared("complete", 0, nil, -1, 30) {
		t.Fatal("short, complete media was not prepared")
	}
	if err := manager.Park(t.Context(), "complete", 30); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Start(t.Context(), "complete", 0, nil, -1); err != nil {
		t.Fatal(err)
	}
	if manager.streams["complete"] != stream {
		t.Fatal("a successful completed producer was needlessly rebuilt")
	}
}

func TestRetiredLifetimeCannotQuarantineReplay(t *testing.T) {
	marked := false
	manager := newLifecycleTestManager(t, func(string, error) error {
		marked = true
		return errors.New("quarantined")
	}, "")
	old := manager.lifetimeForPlayback("replay")
	manager.Stop("replay")
	current := manager.lifetimeForPlayback("replay")
	if old == current {
		t.Fatal("replay reused a stopped lifetime")
	}
	if err := manager.sourceStallError(t.Context(), "replay", old, errors.New("late failure")); !errors.Is(err, context.Canceled) {
		t.Fatalf("retired failure = %v, want cancellation", err)
	}
	if marked || current.ctx.Err() != nil {
		t.Fatal("retired callback poisoned the replacement")
	}
}

func TestCanceledProbeWaiterDoesNotCancelSharedProbe(t *testing.T) {
	manager := newLifecycleTestManager(t, nil, "")
	gate := filepath.Join(t.TempDir(), "probe")
	manager.ffprobePath = writeExecutable(t, "gated-probe", fmt.Sprintf(`#!/bin/sh
touch %q
while [ ! -f %q ]; do sleep 0.01; done
printf '{"streams":[{"index":2,"codec_name":"subrip","codec_type":"subtitle"}]}'
`, gate+".entered", gate))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := manager.ProbeSubtitles(ctx, "shared")
		result <- err
	}()
	waitForLifecycleFile(t, gate+".entered")
	manager.probeMu.Lock()
	original := manager.probes["shared"]
	manager.probeMu.Unlock()
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("probe waiter = %v", err)
	}
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	tracks, err := manager.ProbeSubtitles(t.Context(), "shared")
	if err != nil || len(tracks) != 1 {
		t.Fatalf("remaining probe consumer: tracks=%v error=%v", tracks, err)
	}
	manager.probeMu.Lock()
	retained := manager.probes["shared"] == original
	manager.probeMu.Unlock()
	if !retained {
		t.Fatal("canceled waiter discarded the shared probe")
	}
}

type lifecycleLogWriter func(string)

func (write lifecycleLogWriter) Write(contents []byte) (int, error) {
	write(string(contents))
	return len(contents), nil
}

func waitForLifecycleFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("fixture did not reach %s", filepath.Base(path))
}
