package torrentstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pgeske/filmstream/internal/config"
	"github.com/pgeske/filmstream/internal/torrentstream/torrentstreamtest"
)

func pattern(size int, seed byte) []byte {
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i%251) ^ seed
	}
	return data
}

func movie(name string, size int, pieceLength int64) torrentstreamtest.Torrent {
	return torrentstreamtest.Torrent{
		Name: name, PieceLength: pieceLength,
		Files: []torrentstreamtest.File{{Path: name, Data: pattern(size, 0)}},
	}
}

func newTestEngine(t *testing.T, plugin *torrentstreamtest.Plugin, cfg Config) *Engine {
	t.Helper()
	if cfg.DataDir == "" {
		cfg.DataDir = t.TempDir()
	}
	cfg.PluginURL = plugin.URL
	cfg.PluginTokenFile = plugin.TokenFile
	cfg.DownloadsDir = plugin.DownloadsDir
	if cfg.MetadataTimeout == 0 {
		cfg.MetadataTimeout = 5 * time.Second
	}
	if cfg.CleanupInterval == 0 {
		// Tests drive maintain directly.
		cfg.CleanupInterval = time.Hour
	}
	engine, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	return engine
}

func writeTorrent(t *testing.T, torrent torrentstreamtest.Torrent) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "source.torrent")
	if err := os.WriteFile(path, torrent.Metainfo(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func fetch(ctx context.Context, engine *Engine, id, rangeHeader string) (*httptest.ResponseRecorder, error) {
	request := httptest.NewRequest(http.MethodGet, "/stream", nil).WithContext(ctx)
	if rangeHeader != "" {
		request.Header.Set("Range", rangeHeader)
	}
	response := httptest.NewRecorder()
	err := engine.ServeHTTP(response, request, id)
	return response, err
}

func eventually(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestCreateSelectsHintedEpisodeAndDownloadsOnlyItFromPublicPack(t *testing.T) {
	plugin := torrentstreamtest.NewPlugin(t)
	plugin.SetAutoComplete(true)
	pack := torrentstreamtest.Torrent{Name: "Show.S01", PieceLength: 16 << 10, Files: []torrentstreamtest.File{
		{Path: "Show.S01/Show.S01E01.mkv", Data: pattern(300<<10, 1)},
		{Path: "Show.S01/Show.1x02.mkv", Data: pattern(200<<10, 2)},
		{Path: "Show.S01/Show.S01E03.mkv", Data: pattern(400<<10, 3)},
		{Path: "Show.S01/notes.txt", Data: pattern(10, 4)},
	}}
	hash := plugin.Register(pack)
	engine := newTestEngine(t, plugin, Config{})

	session, err := engine.Create(t.Context(), Source{TorrentPath: writeTorrent(t, pack), FileHint: "S01E02"})
	if err != nil {
		t.Fatal(err)
	}
	if session.FileName != "Show.1x02.mkv" || session.FileSize != 200<<10 {
		t.Fatalf("selected %q (%d bytes)", session.FileName, session.FileSize)
	}
	if got := plugin.Priorities(hash); !slices.Equal(got, []int{0, 4, 0, 0}) {
		t.Fatalf("public pack priorities = %v, want only the selected episode", got)
	}
	response, err := fetch(t.Context(), engine, session.ID, "bytes=100000-100099")
	if err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusPartialContent || !bytes.Equal(response.Body.Bytes(), pack.Files[1].Data[100000:100100]) {
		t.Fatalf("status = %d, body mismatch", response.Code)
	}
	if got := plugin.Priorities(hash); !slices.Equal(got, []int{0, 4, 0, 0}) {
		t.Fatalf("played public pack priorities = %v, want only the selected episode", got)
	}
}

func TestPrivateTorrentDownloadsEveryFileOncePlayed(t *testing.T) {
	for _, tc := range []struct {
		name    string
		bep27   bool
		indexer string
		config  []config.Indexer
	}{
		{name: "bep27 flag", bep27: true},
		{name: "configured private indexer", indexer: "tracker", config: []config.Indexer{{Name: "Tracker", Private: true}}},
		{name: "known private tracker", indexer: "torrentleech"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plugin := torrentstreamtest.NewPlugin(t)
			plugin.SetAutoComplete(true)
			pack := torrentstreamtest.Torrent{Name: "Pack", PieceLength: 16 << 10, Private: tc.bep27, Files: []torrentstreamtest.File{
				{Path: "Pack/Show.S01E01.mkv", Data: pattern(100<<10, 1)},
				{Path: "Pack/Show.S01E02.mkv", Data: pattern(100<<10, 2)},
			}}
			hash := plugin.Register(pack)
			engine := newTestEngine(t, plugin, Config{Indexers: tc.config})
			session, err := engine.Create(t.Context(), Source{TorrentPath: writeTorrent(t, pack), FileHint: "S01E02", Indexer: tc.indexer})
			if err != nil {
				t.Fatal(err)
			}
			if got := plugin.Priorities(hash); !slices.Equal(got, []int{0, 4}) {
				t.Fatalf("unplayed private priorities = %v, want only the selected file", got)
			}
			if _, err := fetch(t.Context(), engine, session.ID, "bytes=0-9"); err != nil {
				t.Fatal(err)
			}
			if got := plugin.Priorities(hash); !slices.Equal(got, []int{4, 4}) {
				t.Fatalf("played private priorities = %v, want every file", got)
			}
			if status, _ := engine.Status(session.ID); !status.Private {
				t.Fatalf("status = %+v, want private", status)
			}
		})
	}
}

func TestServeWaitsForVerifiedPiecesAndServesExactRange(t *testing.T) {
	plugin := torrentstreamtest.NewPlugin(t)
	film := movie("Film.mkv", 1<<20, 64<<10)
	hash := plugin.Register(film)
	engine := newTestEngine(t, plugin, Config{})
	session, err := engine.Create(t.Context(), Source{MagnetURI: film.Magnet()})
	if err != nil {
		t.Fatal(err)
	}

	type result struct {
		response *httptest.ResponseRecorder
		err      error
	}
	done := make(chan result, 1)
	go func() {
		response, err := fetch(t.Context(), engine, session.ID, "bytes=600000-700000")
		done <- result{response, err}
	}()
	eventually(t, "a stream window at the requested offset", func() bool {
		for stream, window := range plugin.Windows(hash) {
			if strings.HasPrefix(stream, "read-") && window.Offset == 600000 && window.Length >= minLookahead {
				return true
			}
		}
		return false
	})
	plugin.Complete(hash, 9, 9)
	select {
	case <-done:
		t.Fatal("served the range before all of its pieces were verified")
	case <-time.After(150 * time.Millisecond):
	}
	plugin.Complete(hash, 10, 10)
	var got result
	select {
	case got = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("range was not served after its pieces were verified")
	}
	if got.err != nil {
		t.Fatal(got.err)
	}
	if got.response.Code != http.StatusPartialContent || !bytes.Equal(got.response.Body.Bytes(), film.Files[0].Data[600000:700001]) {
		t.Fatalf("status = %d, %d bytes, body mismatch", got.response.Code, got.response.Body.Len())
	}
	eventually(t, "the finished reader's window to be released", func() bool {
		for stream := range plugin.Windows(hash) {
			if strings.HasPrefix(stream, "read-") {
				return false
			}
		}
		return true
	})
}

func TestStreamWindowFollowsSequentialReads(t *testing.T) {
	plugin := torrentstreamtest.NewPlugin(t)
	film := movie("Film.mkv", 24<<20, 1<<20)
	hash := plugin.Register(film)
	engine := newTestEngine(t, plugin, Config{})
	session, err := engine.Create(t.Context(), Source{TorrentPath: writeTorrent(t, film)})
	if err != nil {
		t.Fatal(err)
	}
	plugin.CompleteAll(hash)

	reader := engine.newReader(t.Context(), session)
	var read bytes.Buffer
	buffer := make([]byte, 32<<10)
	for {
		n, err := reader.Read(buffer)
		read.Write(buffer[:n])
		if err != nil {
			break
		}
	}
	if !bytes.Equal(read.Bytes(), film.Files[0].Data) {
		t.Fatal("sequential read returned wrong bytes")
	}
	window, ok := plugin.Windows(hash)[reader.stream]
	if !ok || window.Offset < 16<<20 || window.Length < minLookahead {
		t.Fatalf("window after reading = %+v (present %v), want it moved past 16 MiB", window, ok)
	}
	reader.Close()
	if _, ok := plugin.Windows(hash)[reader.stream]; ok {
		t.Fatal("closed reader kept its stream window")
	}
}

func TestLookaheadGrowsWithContiguousReadingAndResetsOnSeek(t *testing.T) {
	reader := &pieceReader{file: pluginFile{Size: 8 << 30}}
	if got := reader.lookahead(); got != minLookahead {
		t.Fatalf("initial lookahead = %d", got)
	}
	reader.pos = 300 << 20
	if got := reader.lookahead(); got != 300<<20 {
		t.Fatalf("lookahead after 300 MiB = %d", got)
	}
	reader.pos = 3 << 30
	if got := reader.lookahead(); got != maxLookahead {
		t.Fatalf("lookahead after 3 GiB = %d", got)
	}
	if _, err := reader.Seek(5<<30, 0); err != nil {
		t.Fatal(err)
	}
	if got := reader.lookahead(); got != minLookahead {
		t.Fatalf("lookahead after seek = %d", got)
	}
}

func TestCreateStopsWaitingForMetadataOnCancelAndTimeout(t *testing.T) {
	plugin := torrentstreamtest.NewPlugin(t)
	plugin.SetMetadataDelayed(true)
	slow := movie("Slow.mkv", 1<<20, 64<<10)
	slowHash := plugin.Register(slow)
	fast := movie("Fast.mkv", 1<<20, 64<<10)
	plugin.Register(fast)
	engine := newTestEngine(t, plugin, Config{MetadataTimeout: 300 * time.Millisecond})

	ctx, cancel := context.WithCancel(t.Context())
	canceled := make(chan error, 1)
	go func() {
		_, err := engine.Create(ctx, Source{MagnetURI: slow.Magnet()})
		canceled <- err
	}()
	eventually(t, "the slow magnet to be added", func() bool { return plugin.Has(slowHash) })
	// A Create waiting for metadata must not block another playback.
	if _, err := engine.Create(t.Context(), Source{TorrentPath: writeTorrent(t, fast)}); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-canceled:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled Create error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Create ignored context cancellation")
	}
	eventually(t, "the abandoned magnet to be removed", func() bool { return !plugin.Has(slowHash) })

	started := time.Now()
	if _, err := engine.Create(t.Context(), Source{MagnetURI: slow.Magnet()}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("metadata timeout error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("metadata timeout took %s", elapsed)
	}
}

func TestCreateFollowsIndexerRedirectToMagnet(t *testing.T) {
	plugin := torrentstreamtest.NewPlugin(t)
	film := movie("Film.mkv", 1<<20, 64<<10)
	hash := plugin.Register(film)
	indexer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, film.Magnet(), http.StatusFound)
	}))
	defer indexer.Close()
	engine := newTestEngine(t, plugin, Config{})
	session, err := engine.Create(t.Context(), Source{TorrentURL: indexer.URL + "/download/1"})
	if err != nil {
		t.Fatal(err)
	}
	if status, _ := engine.Status(session.ID); status.InfoHash != hash {
		t.Fatalf("info hash = %q, want %q", status.InfoHash, hash)
	}
}

func TestDropRemovesUnplayedCandidateButKeepsPrivateSnatch(t *testing.T) {
	plugin := torrentstreamtest.NewPlugin(t)
	public := movie("Public.mkv", 1<<20, 64<<10)
	publicHash := plugin.Register(public)
	private := torrentstreamtest.Torrent{Name: "Private", PieceLength: 64 << 10, Private: true, Files: []torrentstreamtest.File{
		{Path: "Private/Movie.mkv", Data: pattern(1<<20, 5)},
		{Path: "Private/Extras.mkv", Data: pattern(1<<20, 6)},
	}}
	privateHash := plugin.Register(private)
	engine := newTestEngine(t, plugin, Config{})

	publicSession, err := engine.Create(t.Context(), Source{TorrentPath: writeTorrent(t, public)})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Drop(publicSession.ID); err != nil {
		t.Fatal(err)
	}
	if plugin.Has(publicHash) || !plugin.RemovedWithData(publicHash) {
		t.Fatal("unplayed public candidate was not removed with its data")
	}

	privateSession, err := engine.Create(t.Context(), Source{TorrentPath: writeTorrent(t, private), FileHint: "Movie"})
	if err != nil {
		t.Fatal(err)
	}
	// 10% of the torrent arrived while the candidate was evaluated.
	plugin.Complete(privateHash, 0, 3)
	if err := engine.Drop(privateSession.ID); err != nil {
		t.Fatal(err)
	}
	if !plugin.Has(privateHash) {
		t.Fatal("dropped a private torrent after it downloaded enough to count as a snatch")
	}
	if got := plugin.Priorities(privateHash); !slices.Equal(got, []int{4, 4}) {
		t.Fatalf("retained private snatch priorities = %v, want every file", got)
	}
}

func TestDropRemovesPrivateCandidateWhoseProbeReceivedNoData(t *testing.T) {
	plugin := torrentstreamtest.NewPlugin(t)
	private := movie("Private.mkv", 1<<20, 64<<10)
	private.Private = true
	hash := plugin.Register(private)
	engine := newTestEngine(t, plugin, Config{PeerlessStartupWait: 50 * time.Millisecond})

	session, err := engine.Create(t.Context(), Source{TorrentPath: writeTorrent(t, private)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fetch(t.Context(), engine, session.ID, "bytes=0-"); !errors.Is(err, ErrSourceUnavailable) {
		t.Fatalf("probe of a peerless torrent: error = %v", err)
	}
	if err := engine.Drop(session.ID); err != nil {
		t.Fatal(err)
	}
	if plugin.Has(hash) {
		t.Fatal("kept a dead private candidate whose probe never received data")
	}
}

func TestCleanupKeepsPrivateTorrentUntilSeedRuleIsMet(t *testing.T) {
	plugin := torrentstreamtest.NewPlugin(t)
	plugin.SetAutoComplete(true)
	private := movie("Private.mkv", 1<<20, 64<<10)
	private.Private = true
	privateHash := plugin.Register(private)
	public := movie("Public.mkv", 1<<20, 64<<10)
	publicHash := plugin.Register(public)
	engine := newTestEngine(t, plugin, Config{CacheLimitBytes: 1, IdleGrace: time.Minute, SeedRatioTarget: 1})
	var cleaned []string
	engine.SetCleanupHandler(func(id, reason string) { cleaned = append(cleaned, reason) })

	for _, source := range []Source{
		{TorrentPath: writeTorrent(t, private), Indexer: "avistaz"},
		{TorrentPath: writeTorrent(t, public)},
	} {
		session, err := engine.Create(t.Context(), source)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fetch(t.Context(), engine, session.ID, "bytes=0-9"); err != nil {
			t.Fatal(err)
		}
	}
	later := time.Now().Add(time.Hour)
	engine.maintain(t.Context(), later)
	if !plugin.Has(privateHash) {
		t.Fatal("cache limit evicted a private torrent before its seed rule was met")
	}
	if plugin.Has(publicHash) || !slices.Equal(cleaned, []string{"cache-limit"}) {
		t.Fatalf("public torrent present = %v, cleanups = %v; want it evicted for the cache limit", plugin.Has(publicHash), cleaned)
	}

	// AvistaZ: 72 h + 2 h/GiB of seeding while complete.
	plugin.SetStats(privateHash, func(stats *torrentstreamtest.Stats) { stats.SeedingSeconds = 71 * 3600 })
	engine.maintain(t.Context(), later)
	if !plugin.Has(privateHash) {
		t.Fatal("private torrent removed before its seeding time was met")
	}
	plugin.SetStats(privateHash, func(stats *torrentstreamtest.Stats) { stats.SeedingSeconds = 73 * 3600 })
	engine.maintain(t.Context(), later)
	if plugin.Has(privateHash) {
		t.Fatal("private torrent kept after its seed rule was met")
	}
}

func TestCleanupNeverRetiresActivePlayback(t *testing.T) {
	plugin := torrentstreamtest.NewPlugin(t)
	film := movie("Film.mkv", 1<<20, 64<<10)
	hash := plugin.Register(film)
	engine := newTestEngine(t, plugin, Config{IdleGrace: time.Millisecond})
	session, err := engine.Create(t.Context(), Source{TorrentPath: writeTorrent(t, film)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go fetch(ctx, engine, session.ID, "bytes=0-9")
	eventually(t, "the stream to start", func() bool {
		status, _ := engine.Status(session.ID)
		return status.ActiveStreams == 1
	})
	engine.maintain(t.Context(), time.Now().Add(24*time.Hour))
	if !plugin.Has(hash) {
		t.Fatal("cleanup removed a torrent with an active stream")
	}
}

func TestSeedStateRules(t *testing.T) {
	engine := &Engine{seedRatioTarget: 1, seedMaxAge: 168 * time.Hour}
	gibibyte := int64(1 << 30)
	complete := pluginTorrent{TotalSize: 10 * gibibyte, TotalDone: 10 * gibibyte, IsSeeding: true, AllTimeDownload: 10 * gibibyte}
	with := func(detail pluginTorrent, edit func(*pluginTorrent)) pluginTorrent {
		edit(&detail)
		return detail
	}
	torrentleech := &config.SeedRule{Ratio: 1, Hours: 240}
	avistaz := &config.SeedRule{Hours: 72, HoursPerGiB: 2}
	for _, tc := range []struct {
		name   string
		record managedTorrent
		detail pluginTorrent
		met    bool
	}{
		{"ratio one met", managedTorrent{Private: true, Seed: torrentleech},
			with(complete, func(d *pluginTorrent) { d.AllTimeUpload = 10 * gibibyte }), true},
		{"ratio below one and short seeding", managedTorrent{Private: true, Seed: torrentleech},
			with(complete, func(d *pluginTorrent) { d.AllTimeUpload = 9 * gibibyte; d.SeedingSeconds = 239 * 3600 }), false},
		{"240 hours seeding", managedTorrent{Private: true, Seed: torrentleech},
			with(complete, func(d *pluginTorrent) { d.SeedingSeconds = 240 * 3600 }), true},
		{"ratio needs downloaded data", managedTorrent{Private: true, Seed: torrentleech},
			with(complete, func(d *pluginTorrent) { d.AllTimeDownload = 0; d.AllTimeUpload = gibibyte }), false},
		{"incomplete never satisfies", managedTorrent{Private: true, Seed: torrentleech},
			with(complete, func(d *pluginTorrent) {
				d.IsSeeding = false
				d.TotalDone = gibibyte
				d.AllTimeUpload = 20 * gibibyte
				d.SeedingSeconds = 1000 * 3600
			}), false},
		{"size scaled seeding short", managedTorrent{Private: true, Seed: avistaz},
			with(complete, func(d *pluginTorrent) { d.SeedingSeconds = 91 * 3600; d.AllTimeUpload = 50 * gibibyte }), false},
		{"size scaled seeding met", managedTorrent{Private: true, Seed: avistaz},
			with(complete, func(d *pluginTorrent) { d.SeedingSeconds = 92 * 3600 }), true},
		{"unknown private tracker defaults to ratio one or 240 hours", managedTorrent{Private: true},
			with(complete, func(d *pluginTorrent) { d.SeedingSeconds = 200 * 3600 }), false},
		{"public ratio target", managedTorrent{},
			with(complete, func(d *pluginTorrent) { d.IsSeeding = false; d.AllTimeUpload = 10 * gibibyte }), true},
		{"public seed age", managedTorrent{},
			with(complete, func(d *pluginTorrent) { d.FinishedSeconds = 168 * 3600 }), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := engine.seedState(tc.record, tc.detail).met; got != tc.met {
				t.Fatalf("met = %v, want %v", got, tc.met)
			}
		})
	}
}

func TestSourceUnavailableIsTemporaryAndPerPlayback(t *testing.T) {
	plugin := torrentstreamtest.NewPlugin(t)
	film := movie("Film.mkv", 1<<20, 64<<10)
	hash := plugin.Register(film)
	engine := newTestEngine(t, plugin, Config{})
	failed, err := engine.Create(t.Context(), Source{TorrentPath: writeTorrent(t, film)})
	if err != nil {
		t.Fatal(err)
	}
	other, err := engine.Create(t.Context(), Source{TorrentPath: writeTorrent(t, film)})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.MarkSourceUnavailable(failed.ID, errors.New("producer stopped advancing")); !errors.Is(err, ErrSourceUnavailable) {
		t.Fatalf("mark = %v", err)
	}
	if _, err := fetch(t.Context(), engine, failed.ID, "bytes=0-9"); !errors.Is(err, ErrSourceUnavailable) {
		t.Fatalf("blocked read on an unavailable source = %v", err)
	}
	if engine.SourceUnavailable(other.ID) != nil {
		t.Fatal("one playback's verdict poisoned another playback of the same torrent")
	}
	plugin.CompleteAll(hash)
	response, err := fetch(t.Context(), engine, failed.ID, "bytes=0-9")
	if err != nil || !bytes.Equal(response.Body.Bytes(), film.Files[0].Data[:10]) {
		t.Fatalf("read after data arrived: err = %v", err)
	}
	if err := engine.SourceUnavailable(failed.ID); err != nil {
		t.Fatalf("delivered data did not clear the verdict: %v", err)
	}
}

func TestPeerlessStartupFailsWithinBound(t *testing.T) {
	plugin := torrentstreamtest.NewPlugin(t)
	film := movie("Film.mkv", 1<<20, 64<<10)
	plugin.Register(film)
	engine := newTestEngine(t, plugin, Config{PeerlessStartupWait: 200 * time.Millisecond})
	session, err := engine.Create(t.Context(), Source{TorrentPath: writeTorrent(t, film)})
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if _, err := fetch(t.Context(), engine, session.ID, ""); !errors.Is(err, ErrSourceUnavailable) {
		t.Fatalf("peerless read = %v", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("peerless read failed after %s", elapsed)
	}
	if status, _ := engine.Status(session.ID); !status.SourceUnavailable {
		t.Fatalf("status = %+v", status)
	}
}

func TestLocalFilePathOnlyForCompleteFile(t *testing.T) {
	plugin := torrentstreamtest.NewPlugin(t)
	film := movie("Film.mkv", 1<<20, 64<<10)
	hash := plugin.Register(film)
	engine := newTestEngine(t, plugin, Config{})
	session, err := engine.Create(t.Context(), Source{TorrentPath: writeTorrent(t, film)})
	if err != nil {
		t.Fatal(err)
	}
	plugin.Complete(hash, 0, 14)
	if path, ok := engine.LocalFilePath(session.ID); ok {
		t.Fatalf("partial file exposed at %s", path)
	}
	plugin.Complete(hash, 15, 15)
	path, ok := engine.LocalFilePath(session.ID)
	if !ok {
		t.Fatal("complete file not exposed")
	}
	if contents, err := os.ReadFile(path); err != nil || !bytes.Equal(contents, film.Files[0].Data) {
		t.Fatalf("local file contents mismatch: %v", err)
	}
}

func TestRestoreKeepsUnreadableEntriesAndReaddsLostTorrents(t *testing.T) {
	plugin := torrentstreamtest.NewPlugin(t)
	private := movie("Private.mkv", 1<<20, 64<<10)
	private.Private = true
	hash := plugin.Register(private)
	dataDir := t.TempDir()
	record, err := json.Marshal(managedTorrent{InfoHash: hash, Private: true, Started: true, WantAll: true, Files: []int{0},
		LastActivity: time.Now().Add(-time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	state := `{"version":2,"torrents":[` + string(record) + `,{"info_hash":"not-a-hash"}]}`
	if err := os.WriteFile(filepath.Join(dataDir, "managed-torrents.json"), []byte(state), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeManagedFile(filepath.Join(dataDir, "managed-torrents", hash+".torrent"), private.Metainfo()); err != nil {
		t.Fatal(err)
	}

	engine := newTestEngine(t, plugin, Config{DataDir: dataDir})
	eventually(t, "the lost obligation to be re-added to Deluge", func() bool { return plugin.Has(hash) })
	if got := plugin.Priorities(hash); !slices.Equal(got, []int{4}) {
		t.Fatalf("re-added priorities = %v", got)
	}
	engine.maintain(t.Context(), time.Now().Add(time.Hour))
	if !plugin.Has(hash) {
		t.Fatal("restored private obligation was retired before its rule was met")
	}
	engine.Close()
	contents, err := os.ReadFile(filepath.Join(dataDir, "managed-torrents.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(contents), hash) || !strings.Contains(string(contents), "not-a-hash") {
		t.Fatalf("saved state lost entries:\n%s", contents)
	}
}

func TestListenPortFileOverridesFixedPort(t *testing.T) {
	plugin := torrentstreamtest.NewPlugin(t)
	portFile := filepath.Join(t.TempDir(), "forwarded-port")
	if err := os.WriteFile(portFile, []byte("51413\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	engine := newTestEngine(t, plugin, Config{ListenPort: 6881, ListenPortFile: portFile})
	eventually(t, "the forwarded port to reach Deluge", func() bool { return plugin.ListenPort() == 51413 })
	if got := engine.ListenPort(); got != 51413 {
		t.Fatalf("ListenPort() = %d", got)
	}
}

func TestEngineLocksItsDataDirectory(t *testing.T) {
	plugin := torrentstreamtest.NewPlugin(t)
	dataDir := t.TempDir()
	newTestEngine(t, plugin, Config{DataDir: dataDir})
	if _, err := New(Config{DataDir: dataDir, PluginURL: plugin.URL}); err == nil {
		t.Fatal("second engine shared a data directory")
	}
}
