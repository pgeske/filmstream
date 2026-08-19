package torrentstream

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	return cfg
}
