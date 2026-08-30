package torrentstream

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
)

func TestEngineServesRangesWithoutRequestingTheWholeFile(t *testing.T) {
	dataDir := t.TempDir()
	torrentPath, _, contents := createTestTorrent(t, dataDir)
	engine := newTestEngine(t, dataDir, Config{})
	defer engine.Close()

	session, err := engine.Create(context.Background(), Source{TorrentPath: torrentPath})
	if err != nil {
		t.Fatal(err)
	}
	if priority := session.file.Priority(); priority != torrent.PiecePriorityNone {
		t.Fatalf("file priority = %v, want no background download", priority)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	request := httptest.NewRequest("GET", "/stream", nil).WithContext(ctx)
	request.Header.Set("Range", "bytes=10-99")
	response := httptest.NewRecorder()
	if err := engine.ServeHTTP(response, request, session.ID); err != nil {
		t.Fatal(err)
	}

	if response.Code != 206 {
		t.Fatalf("status = %d, body = %q", response.Code, response.Body.String())
	}
	if got, want := response.Body.Bytes(), contents[10:100]; !bytes.Equal(got, want) {
		t.Fatalf("range body mismatch: got %d bytes, want %d", len(got), len(want))
	}
}

func TestEngineReturnsFullyDownloadedLocalFilePath(t *testing.T) {
	dataDir := t.TempDir()
	torrentPath, videoPath, _ := createTestTorrent(t, dataDir)
	engine := newTestEngine(t, dataDir, Config{})
	defer engine.Close()

	session, err := engine.Create(t.Context(), Source{TorrentPath: torrentPath})
	if err != nil {
		t.Fatal(err)
	}
	path, ok := engine.LocalFilePath(session.ID)
	if !ok || path != videoPath {
		t.Fatalf("local path = %q, available = %v, want %q", path, ok, videoPath)
	}
}

func TestEngineDoesNotReturnPartialLocalFilePath(t *testing.T) {
	dataDir := t.TempDir()
	torrentPath, videoPath, contents := createTestTorrent(t, dataDir)
	partPath := videoPath + ".part"
	if err := os.Rename(videoPath, partPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(partPath, int64(len(contents)/2)); err != nil {
		t.Fatal(err)
	}

	engine := newTestEngine(t, dataDir, Config{})
	defer engine.Close()
	session, err := engine.Create(t.Context(), Source{TorrentPath: torrentPath})
	if err != nil {
		t.Fatal(err)
	}
	if path, ok := engine.LocalFilePath(session.ID); ok {
		t.Fatalf("partial torrent exposed as local file %q", path)
	}
}

func TestCleanupProtectsTorrentUntilSeedingRequirementIsMet(t *testing.T) {
	dataDir := t.TempDir()
	torrentPath, videoPath, _ := createTestTorrent(t, dataDir)
	engine := newTestEngine(t, dataDir, Config{
		CacheLimitBytes: 1,
		IdleGrace:       time.Millisecond,
		SeedMaxAge:      3 * time.Hour,
		CleanupInterval: time.Hour,
	})
	defer engine.Close()

	session, err := engine.Create(t.Context(), Source{TorrentPath: torrentPath})
	if err != nil {
		t.Fatal(err)
	}
	var cleanupID, cleanupReason string
	engine.SetCleanupHandler(func(id, reason string) {
		cleanupID, cleanupReason = id, reason
	})
	if _, ok := engine.beginStream(session.ID, true); !ok {
		t.Fatal("could not mark stream active")
	}
	now := time.Now().UTC()
	engine.cleanup(now.Add(2 * time.Hour))
	if _, ok := engine.Get(session.ID); !ok {
		t.Fatal("active stream was cleaned up")
	}

	engine.endStream(session.ID)
	engine.cleanup(now.Add(2*time.Hour + time.Second))
	if _, ok := engine.Get(session.ID); !ok {
		t.Fatal("torrent was evicted before its seeding requirement was met")
	}

	engine.cleanup(now.Add(4 * time.Hour))
	if _, ok := engine.Get(session.ID); ok {
		t.Fatal("torrent was not retired after its seeding requirement was met")
	}
	if cleanupID != session.ID || cleanupReason != "seed-time-target" {
		t.Fatalf("cleanup = %q, %q", cleanupID, cleanupReason)
	}
	if _, err := os.Stat(videoPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cached video still exists: %v", err)
	}
}

func TestTorrentMetainfoCanRecreatePlayback(t *testing.T) {
	dataDir := t.TempDir()
	torrentPath, _, _ := createTestTorrent(t, dataDir)
	engine := newTestEngine(t, dataDir, Config{})
	defer engine.Close()

	first, err := engine.Create(t.Context(), Source{TorrentPath: torrentPath})
	if err != nil {
		t.Fatal(err)
	}
	contents, err := engine.TorrentMetainfo(first.ID)
	if err != nil {
		t.Fatal(err)
	}
	cachedPath := filepath.Join(t.TempDir(), "cached.torrent")
	if err := os.WriteFile(cachedPath, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := engine.Create(t.Context(), Source{TorrentPath: cachedPath})
	if err != nil {
		t.Fatal(err)
	}
	if second.Name != first.Name || second.FileName != first.FileName {
		t.Fatalf("recreated playback = %+v, want source %+v", second, first)
	}
}

func TestDropRemovesInactivePlayback(t *testing.T) {
	dataDir := t.TempDir()
	torrentPath, videoPath, _ := createTestTorrent(t, dataDir)
	engine := newTestEngine(t, dataDir, Config{})
	defer engine.Close()

	session, err := engine.Create(context.Background(), Source{TorrentPath: torrentPath})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Drop(session.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := engine.Get(session.ID); ok {
		t.Fatal("dropped playback is still available")
	}
	if _, err := os.Stat(videoPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dropped playback data still exists: %v", err)
	}
}

func TestDropRetainsStartedTorrentForSeeding(t *testing.T) {
	dataDir := t.TempDir()
	torrentPath, videoPath, _ := createTestTorrent(t, dataDir)
	engine := newTestEngine(t, dataDir, Config{SeedMaxAge: 3 * time.Hour})
	defer engine.Close()

	session, err := engine.Create(t.Context(), Source{TorrentPath: torrentPath})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := engine.beginStream(session.ID, true); !ok {
		t.Fatal("could not mark stream active")
	}
	engine.endStream(session.ID)
	if err := engine.Drop(session.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := engine.Get(session.ID); ok {
		t.Fatal("rejected playback ID is still available")
	}
	if len(engine.sessions) != 1 || len(engine.managed) != 1 {
		t.Fatalf("retained sessions = %d, managed torrents = %d", len(engine.sessions), len(engine.managed))
	}
	if _, err := os.Stat(videoPath); err != nil {
		t.Fatalf("started torrent data was removed: %v", err)
	}
}

func TestDropKeepsSharedTorrentPlayback(t *testing.T) {
	dataDir := t.TempDir()
	torrentPath, videoPath, _ := createTestTorrent(t, dataDir)
	engine := newTestEngine(t, dataDir, Config{})
	defer engine.Close()

	first, err := engine.Create(context.Background(), Source{TorrentPath: torrentPath})
	if err != nil {
		t.Fatal(err)
	}
	second, err := engine.Create(context.Background(), Source{TorrentPath: torrentPath})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Drop(first.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := engine.Get(second.ID); !ok {
		t.Fatal("shared torrent playback was removed")
	}
	if _, err := os.Stat(videoPath); err != nil {
		t.Fatalf("shared torrent data was removed: %v", err)
	}
}

func TestDropRejectsActivePlayback(t *testing.T) {
	dataDir := t.TempDir()
	torrentPath, _, _ := createTestTorrent(t, dataDir)
	engine := newTestEngine(t, dataDir, Config{})
	defer engine.Close()

	session, err := engine.Create(context.Background(), Source{TorrentPath: torrentPath})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := engine.beginStream(session.ID, true); !ok {
		t.Fatal("could not mark stream active")
	}
	defer engine.endStream(session.ID)
	if err := engine.Drop(session.ID); err == nil {
		t.Fatal("active playback was dropped")
	}
}

func TestEngineUsesConfiguredListenPort(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}

	engine := newTestEngine(t, t.TempDir(), Config{ListenPort: port})
	defer engine.Close()
	if got := engine.ListenPort(); got != port {
		t.Fatalf("listen port = %d, want %d", got, port)
	}
}

func TestEngineRejectsInvalidListenPort(t *testing.T) {
	for _, port := range []int{-1, 65536} {
		if engine, err := New(Config{DataDir: t.TempDir(), ListenPort: port}); err == nil {
			engine.Close()
			t.Fatalf("listen port %d was accepted", port)
		}
	}
}

func TestCleanOnStartRemovesUnmanagedTorrentData(t *testing.T) {
	dataDir := t.TempDir()
	stalePath := filepath.Join(dataDir, "torrents", "old", "movie.mkv.part")
	if err := os.MkdirAll(filepath.Dir(stalePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stalePath, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	engine := newTestEngine(t, dataDir, Config{CleanOnStart: true})
	defer engine.Close()
	if _, err := os.Stat(stalePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale cache still exists: %v", err)
	}
}

func TestEngineVerifiesLocallyPresentPiecesWithoutPeers(t *testing.T) {
	dataDir := t.TempDir()
	torrentPath, videoPath, contents := createTestTorrent(t, dataDir)
	// A replay can mount data that only exists as a part file: the client then
	// opens every piece as incomplete and, with no peers, reads would stall
	// forever unless local data is re-verified.
	partPath := videoPath + ".part"
	if err := os.Rename(videoPath, partPath); err != nil {
		t.Fatal(err)
	}

	engine := newTestEngine(t, dataDir, Config{})
	defer engine.Close()
	session, err := engine.Create(t.Context(), Source{TorrentPath: torrentPath})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/stream", nil)
	response := httptest.NewRecorder()
	if err := engine.ServeHTTP(response, request, session.ID); err != nil {
		t.Fatalf("read from locally present pieces: %v", err)
	}
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", response.Code, response.Body.String())
	}
	if got, want := response.Body.Bytes(), contents; !bytes.Equal(got, want) {
		t.Fatalf("served %d bytes, want %d", len(got), len(want))
	}
	if _, err := os.Stat(partPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("verified part file was not promoted: %v", err)
	}
	if _, err := os.Stat(videoPath); err != nil {
		t.Fatalf("verified media file missing: %v", err)
	}
	if path, ok := engine.LocalFilePath(session.ID); !ok || path != videoPath {
		t.Fatalf("verified local path = %q, available = %v, want %q", path, ok, videoPath)
	}
}

func TestEngineRemovesPartFileShadowingCompleteMedia(t *testing.T) {
	dataDir := t.TempDir()
	torrentPath, videoPath, contents := createTestTorrent(t, dataDir)
	// Chunk writes for pieces shared with adjacent files can recreate a part
	// file after the media file was promoted. Reads prefer the part file, so
	// the stale shadow must be dropped at startup.
	partPath := videoPath + ".part"
	if err := os.WriteFile(partPath, []byte("shadow"), 0o644); err != nil {
		t.Fatal(err)
	}

	engine := newTestEngine(t, dataDir, Config{})
	defer engine.Close()
	if _, err := os.Stat(partPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale part file still shadows complete media: %v", err)
	}

	session, err := engine.Create(t.Context(), Source{TorrentPath: torrentPath})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/stream", nil)
	response := httptest.NewRecorder()
	if err := engine.ServeHTTP(response, request, session.ID); err != nil {
		t.Fatal(err)
	}
	if got, want := response.Body.Bytes(), contents; !bytes.Equal(got, want) {
		t.Fatalf("served shadowed media: got %d bytes, want %d", len(got), len(want))
	}
}

func TestEngineFailsFastWhenSwarmCannotServeReads(t *testing.T) {
	dataDir := t.TempDir()
	torrentPath, videoPath, _ := createTestTorrent(t, dataDir)
	// A first-play mount with no local data and no peers can never serve the
	// read; it must fail fast with a clear error instead of stalling until the
	// client's request times out.
	if err := os.Remove(videoPath); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	engine := newTestEngine(t, dataDir, Config{
		ServeReadinessWait: 200 * time.Millisecond,
		Logger:             slog.New(slog.NewTextHandler(&logs, nil)),
	})
	defer engine.Close()
	session, err := engine.Create(t.Context(), Source{TorrentPath: torrentPath})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	request := httptest.NewRequest(http.MethodGet, "/stream", nil).WithContext(ctx)
	response := httptest.NewRecorder()
	started := time.Now()
	err = engine.ServeHTTP(response, request, session.ID)
	if err == nil {
		t.Fatal("read without data or peers should fail fast")
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("failure took %s; it should be bounded well below client timeouts", elapsed)
	}
	if !errors.Is(err, ErrSourceUnavailable) || !strings.Contains(err.Error(), "no peers connected") {
		t.Fatalf("error = %v, want a typed, clear no-peers failure", err)
	}
	status, ok := engine.Status(session.ID)
	if !ok || !status.SourceUnavailable || status.ServeDeadline == nil || status.CachedPercent != 0 {
		t.Fatalf("source status = %+v, found = %v", status, ok)
	}

	// FFprobe and FFmpeg issue multiple range requests. Once the first request
	// spends the shared budget, later startup reads must fail immediately rather
	// than receiving another complete readiness window.
	secondStarted := time.Now()
	secondErr := engine.ServeHTTP(
		httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/stream", nil), session.ID,
	)
	if !errors.Is(secondErr, ErrSourceUnavailable) {
		t.Fatalf("second source read error = %v", secondErr)
	}
	if elapsed := time.Since(secondStarted); elapsed > 100*time.Millisecond {
		t.Fatalf("second source read took %s after the shared deadline", elapsed)
	}
	if output := logs.String(); !strings.Contains(output, "no_peers_returned_or_retained") ||
		!strings.Contains(output, "tracker_outcome=inferred_from_peer_counters") {
		t.Fatalf("unavailable-swarm log lacks announce visibility: %s", output)
	}
}

func TestEngineDemotesSparseCompletedMediaBeforeRestore(t *testing.T) {
	dataDir := t.TempDir()
	videoPath := filepath.Join(dataDir, "torrents", "season", "episode.mkv")
	if err := os.MkdirAll(filepath.Dir(videoPath), 0o755); err != nil {
		t.Fatal(err)
	}
	file, err := os.Create(videoPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Seek(16<<20, 0); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if _, err := file.Write([]byte{1}); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	engine := newTestEngine(t, dataDir, Config{})
	defer engine.Close()
	if _, err := os.Stat(videoPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("sparse completed path still exists: %v", err)
	}
	if _, err := os.Stat(videoPath + ".part"); err != nil {
		t.Fatalf("recovered partial path does not exist: %v", err)
	}
}

func TestCleanOnCloseRemovesManagedTorrentData(t *testing.T) {
	dataDir := t.TempDir()
	engine := newTestEngine(t, dataDir, Config{CleanOnClose: true})
	path := filepath.Join(dataDir, "torrents", "temporary.part")
	if err := os.WriteFile(path, []byte("temporary"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("managed data still exists after shutdown: %v", err)
	}
}

func TestRatioTargetRequiresDownloadedData(t *testing.T) {
	if ratioTargetMet(0, 0, 1) {
		t.Fatal("empty transfer should not satisfy a positive ratio target")
	}
	if !ratioTargetMet(100, 1, 1) {
		t.Fatal("1:1 transfer did not satisfy the ratio target")
	}
}

func TestEngineRestoresManagedTorrentForSeeding(t *testing.T) {
	dataDir := t.TempDir()
	torrentPath, _, _ := createTestTorrent(t, dataDir)
	first := newTestEngine(t, dataDir, Config{})
	session, err := first.Create(t.Context(), Source{TorrentPath: torrentPath, FileHint: "sample"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := first.beginStream(session.ID, true); !ok {
		t.Fatal("could not mark torrent started")
	}
	first.endStream(session.ID)
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second := newTestEngine(t, dataDir, Config{})
	defer second.Close()
	second.restoreWG.Wait()
	if len(second.sessions) != 1 || len(second.managed) != 1 {
		t.Fatalf("restored sessions = %d, managed torrents = %d", len(second.sessions), len(second.managed))
	}
	for _, restored := range second.sessions {
		status, ok := second.Status(restored.ID)
		if !ok || status.State != "seeding" {
			t.Fatalf("restored status = %+v, found = %v", status, ok)
		}
	}
}

func TestEngineLocksItsDataDirectory(t *testing.T) {
	dataDir := t.TempDir()
	first := newTestEngine(t, dataDir, Config{})
	if second, err := New(testConfig(dataDir, Config{})); err == nil {
		second.Close()
		t.Fatal("second engine unexpectedly acquired the data directory")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	third := newTestEngine(t, dataDir, Config{})
	third.Close()
}

func createTestTorrent(t *testing.T, dataDir string) (torrentPath, videoPath string, contents []byte) {
	t.Helper()
	torrentDataDir := filepath.Join(dataDir, "torrents")
	if err := os.MkdirAll(torrentDataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	contents = bytes.Repeat([]byte("filmstream-range-test-"), 2048)
	videoPath = filepath.Join(torrentDataDir, "sample.mp4")
	if err := os.WriteFile(videoPath, contents, 0o644); err != nil {
		t.Fatal(err)
	}

	meta := metainfo.MetaInfo{}
	meta.SetDefaults()
	info := metainfo.Info{PieceLength: 16 << 10}
	if err := info.BuildFromFilePath(videoPath); err != nil {
		t.Fatal(err)
	}
	var err error
	meta.InfoBytes, err = bencode.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	torrentPath = filepath.Join(dataDir, "sample.torrent")
	file, err := os.Create(torrentPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := meta.Write(file); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return torrentPath, videoPath, contents
}

func newTestEngine(t *testing.T, dataDir string, overrides Config) *Engine {
	t.Helper()
	engine, err := New(testConfig(dataDir, overrides))
	if err != nil {
		t.Fatal(err)
	}
	return engine
}

func testConfig(dataDir string, overrides Config) Config {
	cfg := Config{
		DataDir:         dataDir,
		MaxTorrentBytes: 1 << 30,
		ReadaheadBytes:  1 << 20,
		MetadataTimeout: 5 * time.Second,
		SeedRatioTarget: 1,
	}
	if overrides.ListenPort != 0 {
		cfg.ListenPort = overrides.ListenPort
	}
	if overrides.CacheLimitBytes != 0 {
		cfg.CacheLimitBytes = overrides.CacheLimitBytes
	}
	if overrides.IdleGrace != 0 {
		cfg.IdleGrace = overrides.IdleGrace
	}
	if overrides.SeedMaxAge != 0 {
		cfg.SeedMaxAge = overrides.SeedMaxAge
	}
	if overrides.CleanupInterval != 0 {
		cfg.CleanupInterval = overrides.CleanupInterval
	}
	cfg.CleanOnStart = overrides.CleanOnStart
	cfg.CleanOnClose = overrides.CleanOnClose
	if overrides.ServeReadinessWait != 0 {
		cfg.ServeReadinessWait = overrides.ServeReadinessWait
	}
	cfg.Logger = overrides.Logger
	return cfg
}
