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

// The producer emits a 12 second buffer, then makes no further progress.
// Gates let tests stop it during timeline verification without real media/network I/O.
func newLifecycleTestManager(t *testing.T, sourceUnavailable func(string) error, probeGate string) *Manager {
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
		BufferSeconds: 4, SourceUnavailable: sourceUnavailable,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	return manager
}

func lifecycleStream(manager *Manager, playbackID string) *runningStream {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	return manager.streams[playbackID]
}

func TestCanceledRecoveryDoesNotDestroySharedProducer(t *testing.T) {
	for _, position := range []float64{0, 6} {
		for _, deadline := range []bool{false, true} {
			t.Run(fmt.Sprintf("position=%g/deadline=%v", position, deadline), func(t *testing.T) {
				manager := newLifecycleTestManager(t, nil, "")
				if _, err := manager.Start(t.Context(), "shared", 0, nil, -1, -1); err != nil {
					t.Fatal(err)
				}
				original := lifecycleStream(manager, "shared")

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
				_, err := manager.Start(ctx, "shared", position, nil, -1, -1)
				if !errors.Is(err, want) {
					t.Fatalf("recovery error = %v, want %v", err, want)
				}
				if lifecycleStream(manager, "shared") != original {
					t.Fatal("canceled waiter replaced another consumer's producer")
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
		_, err := manager.Start(t.Context(), "replay", 0, nil, -1, -1)
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
	if _, err := manager.Asset("replay", "index.m3u8"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stopped startup published assets: %v", err)
	}
	if _, err := manager.Start(t.Context(), "replay", 0, nil, -1, -1); err != nil {
		t.Fatalf("new generation could not replay: %v", err)
	}
}

func TestLateStopCleanupCannotDeleteReplayAssets(t *testing.T) {
	manager := newLifecycleTestManager(t, nil, "")
	if _, err := manager.Start(t.Context(), "replay", 0, nil, -1, -1); err != nil {
		t.Fatal(err)
	}
	old := lifecycleStream(manager, "replay")
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
	_, err := manager.Start(t.Context(), "replay", 0, nil, -1, -1)
	close(release)
	<-stopped
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Asset("replay", "index.m3u8"); err != nil {
		t.Fatalf("old generation's cleanup removed replacement assets: %v", err)
	}
	if lifecycleStream(manager, "replay").ctx.Err() != nil {
		t.Fatal("old generation canceled the replacement producer")
	}
}

func TestExitedProducerCannotPassReadinessFromCachedBuffer(t *testing.T) {
	manager := newLifecycleTestManager(t, nil, "")
	if _, err := manager.Start(t.Context(), "failed", 0, nil, -1, -1); err != nil {
		t.Fatal(err)
	}
	stream := lifecycleStream(manager, "failed")
	if err := stream.command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	<-stream.done
	if err := manager.waitForMedia(t.Context(), stream, 4, "test"); !errors.Is(err, ErrProducerStopped) {
		t.Fatalf("dead producer's cached buffer wait = %v, want %v", err, ErrProducerStopped)
	}
	if _, err := manager.Asset("failed", "index.m3u8"); !errors.Is(err, ErrProducerStopped) {
		t.Fatalf("dead producer's live playlist = %v, want %v", err, ErrProducerStopped)
	}
	if status, _ := manager.Status("failed"); status.State != "failed" || status.Error == "" {
		t.Fatalf("dead producer status = %+v", status)
	}
	if manager.Prepared("failed", 0, nil, -1, -1, 4) {
		t.Fatal("dead producer was reported prepared")
	}
	// A dead producer is never joined; the next start rebuilds it.
	if _, err := manager.Start(t.Context(), "failed", 0, nil, -1, -1); err != nil {
		t.Fatal(err)
	}
	if lifecycleStream(manager, "failed") == stream {
		t.Fatal("dead producer was joined instead of rebuilt")
	}
}

// A producer that stops making progress is only reported as buffering. The
// consumer's outcome depends on what it asked for and on what happens next;
// the producer is removed only when it dies or the engine declares the source
// unavailable, and unrelated playbacks are never touched.
func TestSlowProducerRecoveryKeepsConsumerAndProducerOutcomesSeparate(t *testing.T) {
	unavailable := errors.New("fixture source unavailable")
	tests := []struct {
		name string
		// Resume position; the fixture packaged 12 seconds from 0.
		position float64
		// Applied once the resume waits for the running producer.
		act     func(t *testing.T, stream *runningStream, cancel context.CancelFunc, verdict chan<- struct{})
		timeout time.Duration
		// Checks the Start outcome and the retained producer.
		check func(t *testing.T, err error, original, current *runningStream)
	}{
		{
			name: "beyond packaged edge keeps buffering", position: 30, timeout: 300 * time.Millisecond,
			check: func(t *testing.T, err error, original, current *runningStream) {
				if !errors.Is(err, ErrBuffering) {
					t.Fatalf("resume error = %v, want %v", err, ErrBuffering)
				}
				if current != original || original.ctx.Err() != nil || original.producerError() != nil {
					t.Fatal("a slow producer was stopped or replaced")
				}
			},
		},
		{
			name: "packaged position plays before buffer refills", position: 10, timeout: 300 * time.Millisecond,
			check: func(t *testing.T, err error, original, current *runningStream) {
				if err != nil || current != original || original.ctx.Err() != nil {
					t.Fatalf("packaged resume = %v, replaced=%v", err, current != original)
				}
			},
		},
		{
			name: "producer advances", position: 30, timeout: 3 * time.Second,
			act: func(t *testing.T, stream *runningStream, _ context.CancelFunc, _ chan<- struct{}) {
				appendLifecycleSegments(t, stream.dir, 3, 9)
			},
			check: func(t *testing.T, err error, original, current *runningStream) {
				if err != nil || current != original {
					t.Fatalf("slow, healthy producer was rebuilt: %v", err)
				}
			},
		},
		{
			name: "waiter cancels", position: 30, timeout: 3 * time.Second,
			act: func(_ *testing.T, _ *runningStream, cancel context.CancelFunc, _ chan<- struct{}) {
				cancel()
			},
			check: func(t *testing.T, err error, original, current *runningStream) {
				if !errors.Is(err, context.Canceled) || current != original || original.ctx.Err() != nil {
					t.Fatalf("canceling recovery damaged the retained producer: %v", err)
				}
			},
		},
		{
			name: "producer dies", position: 30, timeout: 3 * time.Second,
			act: func(t *testing.T, stream *runningStream, _ context.CancelFunc, _ chan<- struct{}) {
				if err := stream.command.Process.Kill(); err != nil {
					t.Fatal(err)
				}
			},
			check: func(t *testing.T, err error, _, current *runningStream) {
				if !errors.Is(err, ErrProducerStopped) || current != nil {
					t.Fatalf("dead producer outcome = %v, retained=%v", err, current != nil)
				}
			},
		},
		{
			name: "engine declares source unavailable", position: 30, timeout: 3 * time.Second,
			act: func(_ *testing.T, _ *runningStream, _ context.CancelFunc, verdict chan<- struct{}) {
				close(verdict)
			},
			check: func(t *testing.T, err error, _, current *runningStream) {
				if !errors.Is(err, unavailable) || current != nil {
					t.Fatalf("unavailable source outcome = %v, retained=%v", err, current != nil)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			verdict := make(chan struct{})
			manager := newLifecycleTestManager(t, func(id string) error {
				select {
				case <-verdict:
					if id == "slow" {
						return unavailable
					}
				default:
				}
				return nil
			}, "")
			manager.stallWindow = 100 * time.Millisecond
			for _, id := range []string{"slow", "unrelated"} {
				if _, err := manager.Start(t.Context(), id, 0, nil, -1, -1); err != nil {
					t.Fatal(err)
				}
			}
			original := lifecycleStream(manager, "slow")
			unrelated := lifecycleStream(manager, "unrelated")

			// A stall is an observation, not a failure: the stream stays prepared.
			deadline := time.Now().Add(2 * time.Second)
			for {
				status, ok := manager.Status("slow")
				if ok && status.State == "buffering" && status.PackagedSeconds == 12 && status.Error == "" {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("stalled producer status = %+v, exists=%v", status, ok)
				}
				time.Sleep(10 * time.Millisecond)
			}
			if !manager.Prepared("slow", 0, nil, -1, -1, 4) {
				t.Fatal("a stalled but live buffer was not prepared")
			}

			manager.startupTimeout = test.timeout
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			result := make(chan error, 1)
			go func() {
				_, err := manager.Start(ctx, "slow", test.position, nil, -1, -1)
				result <- err
			}()
			if test.act != nil {
				// The join records the playhead immediately before it waits.
				deadline := time.Now().Add(2 * time.Second)
				for original.observe().playheadSeconds != test.position {
					if time.Now().After(deadline) {
						t.Fatal("resume did not join the running producer")
					}
					time.Sleep(10 * time.Millisecond)
				}
				test.act(t, original, cancel, verdict)
			}
			var err error
			select {
			case err = <-result:
			case <-time.After(5 * time.Second):
				t.Fatal("resume did not finish")
			}
			test.check(t, err, original, lifecycleStream(manager, "slow"))
			if lifecycleStream(manager, "unrelated") != unrelated || unrelated.ctx.Err() != nil {
				t.Fatal("recovery disturbed an unrelated playback")
			}
		})
	}
}

func TestResumeBeyondJoinReachRebuildsPackager(t *testing.T) {
	manager := newLifecycleTestManager(t, nil, "")
	if _, err := manager.Start(t.Context(), "seek", 0, nil, -1, -1); err != nil {
		t.Fatal(err)
	}
	original := lifecycleStream(manager, "seek")
	position := 12 + joinReachSeconds + 1
	stream, err := manager.Start(t.Context(), "seek", position, nil, -1, -1)
	if err != nil {
		t.Fatal(err)
	}
	if stream.RequestedStartSeconds != position || lifecycleStream(manager, "seek") == original {
		t.Fatalf("far seek joined the old packager: %+v", stream)
	}
	if original.ctx.Err() == nil {
		t.Fatal("replaced packager kept running")
	}
}

func appendLifecycleSegments(t *testing.T, dir string, first, end int) {
	t.Helper()
	path := filepath.Join(dir, "index.m3u8")
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var playlist strings.Builder
	playlist.Write(contents)
	for number := first; number < end; number++ {
		name := fmt.Sprintf("segment-%06d.m4s", number)
		if err := os.WriteFile(filepath.Join(dir, name), []byte("segment"), 0o600); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&playlist, "#EXTINF:4.0,\n%s\n", name)
	}
	if err := os.WriteFile(path+".tmp", []byte(playlist.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".tmp", path); err != nil {
		t.Fatal(err)
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
	if _, err := manager.Start(t.Context(), "complete", 0, nil, -1, -1); err != nil {
		t.Fatal(err)
	}
	stream := lifecycleStream(manager, "complete")
	<-stream.done
	if status, _ := manager.Status("complete"); status.State != "complete" {
		t.Fatalf("completed producer status = %+v", status)
	}
	if !manager.Prepared("complete", 0, nil, -1, -1, 30) {
		t.Fatal("short, complete media was not prepared")
	}
	if err := manager.Park(t.Context(), "complete", 30); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Start(t.Context(), "complete", 0, nil, -1, -1); err != nil {
		t.Fatal(err)
	}
	if lifecycleStream(manager, "complete") != stream {
		t.Fatal("a successful completed producer was needlessly rebuilt")
	}
}

func TestParkedPrewarmIsNotProducerFailure(t *testing.T) {
	manager := newLifecycleTestManager(t, nil, "")
	if _, err := manager.Start(t.Context(), "prewarm", 0, nil, -1, -1); err != nil {
		t.Fatal(err)
	}
	if err := manager.Park(t.Context(), "prewarm", 8); err != nil {
		t.Fatal(err)
	}
	status, ok := manager.Status("prewarm")
	if !ok || status.State != "parked" || status.Error != "" || status.Complete {
		t.Fatalf("parked prewarm status = %+v, exists=%v", status, ok)
	}
	if !manager.Prepared("prewarm", 0, nil, -1, -1, 8) {
		t.Fatal("parked buffer was not prepared")
	}
	if _, err := manager.Asset("prewarm", "index.m3u8"); err != nil {
		t.Fatalf("parked playlist was treated as failed: %v", err)
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

func TestStoppedProbePreservesCancellationWhenSourceAlsoFails(t *testing.T) {
	manager := newLifecycleTestManager(t, nil, "")
	lifetime := manager.lifetimeForPlayback("stopped")
	// A retired session may also report a source error. That must not turn
	// the old operation's cancellation into a 502/replacement on the client.
	manager.sourceUnavailable = func(string) error {
		if lifetime.ctx.Err() != nil {
			return errors.New("fixture source unavailable")
		}
		return nil
	}
	gate := filepath.Join(t.TempDir(), "probe")
	manager.ffprobePath = writeExecutable(t, "stopped-probe", fmt.Sprintf(`#!/bin/sh
touch %q
exec sleep 60
`, gate))
	result := make(chan error, 1)
	go func() {
		_, err := manager.ProbeSubtitles(t.Context(), "stopped")
		result <- err
	}()
	waitForLifecycleFile(t, gate)
	manager.Stop("stopped")
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("retired probe returned %v, want cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("retired probe did not finish")
	}
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
