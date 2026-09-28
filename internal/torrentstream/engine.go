// Package torrentstream streams torrent payloads through a Deluge daemon.
//
// Deluge (libtorrent) does all BitTorrent work; the TeaStream Deluge plugin
// (deluge/teastream) exposes a loopback API for adding torrents, selecting
// files and steering piece deadlines. Payload bytes are read straight from the
// shared downloads directory once their pieces are verified.
package torrentstream

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/pgeske/filmstream/internal/config"
)

const (
	maxMetainfoBytes = 16 << 20

	defaultPluginURL           = "http://127.0.0.1:8113"
	defaultDownloadsDir        = "/downloads"
	defaultMetadataTimeout     = 2 * time.Minute
	defaultCacheLimit          = int64(20 << 30)
	defaultMaxSeedSessions     = 20
	defaultIdleGrace           = 2 * time.Minute
	defaultSeedMaxAge          = 168 * time.Hour
	defaultCleanupInterval     = 30 * time.Second
	defaultStallTimeout        = 3 * time.Minute
	defaultPeerlessStartupWait = 45 * time.Second

	// sourceUnavailableHold is how long an unavailable verdict fails blocked
	// reads before the source may be tried again.
	sourceUnavailableHold = time.Minute
	// detailMaxAge bounds how stale cached Deluge status may be.
	detailMaxAge        = time.Second
	metadataPollMax     = 500 * time.Millisecond
	torrentFetchTimeout = 30 * time.Second
	listenPortPoll      = 10 * time.Second

	// A new playback immediately asks for its file's head (container headers,
	// first frames) and tail (MP4 moov, MKV cues) so probing starts at once.
	mountHeadBytes = 16 << 20
	mountTailBytes = 4 << 20
	mountWindowTTL = 2 * time.Minute
)

type Config struct {
	// DataDir holds Filmstream's torrent records, not payload.
	DataDir string
	// PluginURL is the TeaStream Deluge plugin API.
	PluginURL string
	// PluginTokenFile holds the plugin's bearer token.
	PluginTokenFile string
	// DownloadsDir is where Deluge saves torrents. Deluge and Filmstream must
	// see it at the same path.
	DownloadsDir string
	// ListenPort is a fixed peer port pushed to Deluge; 0 keeps Deluge's.
	ListenPort int
	// ListenPortFile holds a peer port that can change at runtime, such as a
	// VPN's forwarded port. While readable it takes precedence over ListenPort.
	ListenPortFile  string
	MaxTorrentBytes int64
	MetadataTimeout time.Duration
	// Public torrents are retired once idle and past SeedRatioTarget or
	// SeedMaxAge. Private torrents follow their indexer's seed rule instead.
	SeedRatioTarget float64
	SeedMaxAge      time.Duration
	CacheLimitBytes int64
	MaxSeedSessions int
	IdleGrace       time.Duration
	CleanupInterval time.Duration
	// StallTimeout fails a blocked read once the torrent downloaded nothing
	// for this long.
	StallTimeout time.Duration
	// PeerlessStartupWait fails a playback that has not served any data yet
	// after this long without a connected peer.
	PeerlessStartupWait time.Duration
	// Indexers supply private flags and seed rules by indexer name.
	Indexers []config.Indexer
	Logger   *slog.Logger
}

type Source struct {
	MagnetURI   string
	TorrentURL  string
	TorrentPath string
	FileHint    string
	// Indexer is the configured indexer name the release came from; it selects
	// the private-tracker seed rule. Empty for direct magnet/.torrent input.
	Indexer string
}

type Engine struct {
	plugin           *pluginClient
	fetchClient      *http.Client
	dataDir          string
	managedDir       string
	managedStatePath string
	downloadsDir     string
	listenPortFixed  int
	listenPortFile   string
	maxTorrentBytes  int64
	metadataTimeout  time.Duration
	seedRatioTarget  float64
	seedMaxAge       time.Duration
	cacheLimitBytes  int64
	maxSeedSessions  int
	idleGrace        time.Duration
	cleanupInterval  time.Duration
	stallTimeout     time.Duration
	peerlessWait     time.Duration
	// indexers maps indexerKey names to their settings; see SetIndexers.
	indexers  atomic.Pointer[map[string]config.Indexer]
	logger    *slog.Logger
	lockFile  *os.File
	streamSeq atomic.Uint64

	// lifecycleMu serializes adding torrents to and removing them from
	// Deluge. It is never held while waiting for metadata or a download.
	lifecycleMu sync.Mutex

	mu                sync.Mutex
	sessions          map[string]*Session
	torrents          map[string]*torrentState
	unreadableRecords []json.RawMessage
	onCleanup         func(string, string)
	listenPort        int
	missingWarned     map[string]bool

	cancel    context.CancelFunc
	wg        sync.WaitGroup
	closeOnce sync.Once
}

// torrentState is one Deluge torrent shared by every playback of it.
type torrentState struct {
	hash string
	// record is nil until a playback registers; afterwards it is persisted.
	record   *managedTorrent
	restored bool
	// added records that Filmstream added the torrent to Deluge.
	added bool
	// pending counts Create calls that have not registered or abandoned yet.
	pending       int
	sessions      map[string]*Session
	wantAllPushed bool
	// wantMu serializes readers switching a snatched torrent to every file.
	wantMu sync.Mutex

	// Metadata, written once under Engine.mu before any session exists.
	name        string
	pieceLength int64
	numPieces   int
	files       []pluginFile
	savePath    string

	detailMu       sync.Mutex
	detail         pluginTorrent
	detailAt       time.Time
	lastDone       int64
	lastProgressAt time.Time
}

type Session struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	FileName  string    `json:"file_name"`
	FileSize  int64     `json:"file_size"`
	CreatedAt time.Time `json:"created_at"`

	torrent   *torrentState
	file      pluginFile
	localPath string

	// Guarded by Engine.mu.
	activeStreams    int
	served           bool
	lastActivity     time.Time
	serveDeadline    time.Time
	unavailable      error
	unavailableUntil time.Time
}

// ErrSourceUnavailable identifies a playback whose torrent cannot currently
// deliver data. The verdict is temporary: it expires, and any delivered piece
// clears it.
var ErrSourceUnavailable = errors.New("playback source unavailable")

type Status struct {
	ID                   string     `json:"id"`
	Name                 string     `json:"name"`
	FileName             string     `json:"file_name"`
	FileSize             int64      `json:"file_size"`
	State                string     `json:"state"`
	ActiveStreams        int        `json:"active_streams"`
	LastActivity         time.Time  `json:"last_activity"`
	SeedDeadline         *time.Time `json:"seed_deadline,omitempty"`
	BytesComplete        int64      `json:"bytes_complete"`
	TorrentBytes         int64      `json:"torrent_bytes"`
	TorrentComplete      int64      `json:"torrent_complete"`
	DownloadedBytes      int64      `json:"downloaded_bytes"`
	UploadedBytes        int64      `json:"uploaded_bytes"`
	Ratio                float64    `json:"ratio"`
	RatioTarget          float64    `json:"ratio_target"`
	RatioTargetMet       bool       `json:"ratio_target_met"`
	TotalPeers           int        `json:"total_peers"`
	PendingPeers         int        `json:"pending_peers"`
	ActivePeers          int        `json:"active_peers"`
	ConnectedSeeders     int        `json:"connected_seeders"`
	HalfOpenPeers        int        `json:"half_open_peers"`
	InfoHash             string     `json:"info_hash,omitempty"`
	CachedPercent        int        `json:"cached_percent"`
	SourceUnavailable    bool       `json:"source_unavailable,omitempty"`
	OutboundDialObserved bool       `json:"outbound_dial_observed,omitempty"`
	ServeDeadline        *time.Time `json:"serve_deadline,omitempty"`
	DownloadRate         int64      `json:"download_rate"`
	UploadRate           int64      `json:"upload_rate"`
	Progress             float64    `json:"progress"`
	Private              bool       `json:"private"`
	// Snatched reports that a private torrent was played (or downloaded far
	// enough) and must now be completed and seeded. An unsnatched private
	// torrent downloads only its file head and tail.
	Snatched           bool   `json:"snatched"`
	SeedingSeconds     int64  `json:"seeding_seconds"`
	SeedRequirementMet bool   `json:"seed_requirement_met"`
	TrackerMessage     string `json:"tracker_message"`
}

func New(cfg Config) (*Engine, error) {
	if cfg.ListenPort < 0 || cfg.ListenPort > 65535 {
		return nil, fmt.Errorf("listen port must be between 0 and 65535: %d", cfg.ListenPort)
	}
	if cfg.PluginURL == "" {
		cfg.PluginURL = defaultPluginURL
	}
	if cfg.DownloadsDir == "" {
		cfg.DownloadsDir = defaultDownloadsDir
	}
	if !filepath.IsAbs(cfg.DownloadsDir) {
		return nil, fmt.Errorf("downloads directory must be absolute: %s", cfg.DownloadsDir)
	}
	if cfg.MetadataTimeout <= 0 {
		cfg.MetadataTimeout = defaultMetadataTimeout
	}
	if cfg.CacheLimitBytes <= 0 {
		cfg.CacheLimitBytes = defaultCacheLimit
	}
	if cfg.MaxSeedSessions <= 0 {
		cfg.MaxSeedSessions = defaultMaxSeedSessions
	}
	if cfg.IdleGrace <= 0 {
		cfg.IdleGrace = defaultIdleGrace
	}
	if cfg.SeedMaxAge <= 0 {
		cfg.SeedMaxAge = defaultSeedMaxAge
	}
	if cfg.CleanupInterval <= 0 {
		cfg.CleanupInterval = defaultCleanupInterval
	}
	if cfg.StallTimeout <= 0 {
		cfg.StallTimeout = defaultStallTimeout
	}
	if cfg.PeerlessStartupWait <= 0 {
		cfg.PeerlessStartupWait = defaultPeerlessStartupWait
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}
	lockFile, err := acquireDataDirLock(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	fetchTransport := http.DefaultTransport.(*http.Transport).Clone()
	fetchTransport.TLSHandshakeTimeout = 10 * time.Second
	fetchTransport.ResponseHeaderTimeout = 20 * time.Second
	engine := &Engine{
		plugin: newPluginClient(cfg.PluginURL, cfg.PluginTokenFile),
		fetchClient: &http.Client{
			Transport: fetchTransport,
			// Indexers such as Prowlarr answer a download link with a redirect
			// to a magnet URI for magnet-only releases.
			CheckRedirect: func(request *http.Request, via []*http.Request) error {
				if request.URL.Scheme == "magnet" {
					return http.ErrUseLastResponse
				}
				if len(via) >= 10 {
					return errors.New("stopped after 10 redirects")
				}
				return nil
			},
		},
		dataDir:          cfg.DataDir,
		managedDir:       filepath.Join(cfg.DataDir, "managed-torrents"),
		managedStatePath: filepath.Join(cfg.DataDir, "managed-torrents.json"),
		downloadsDir:     filepath.Clean(cfg.DownloadsDir),
		listenPortFixed:  cfg.ListenPort,
		listenPortFile:   cfg.ListenPortFile,
		maxTorrentBytes:  cfg.MaxTorrentBytes,
		metadataTimeout:  cfg.MetadataTimeout,
		seedRatioTarget:  cfg.SeedRatioTarget,
		seedMaxAge:       cfg.SeedMaxAge,
		cacheLimitBytes:  cfg.CacheLimitBytes,
		maxSeedSessions:  cfg.MaxSeedSessions,
		idleGrace:        cfg.IdleGrace,
		cleanupInterval:  cfg.CleanupInterval,
		stallTimeout:     cfg.StallTimeout,
		peerlessWait:     cfg.PeerlessStartupWait,
		logger:           cfg.Logger,
		lockFile:         lockFile,
		sessions:         make(map[string]*Session),
		torrents:         make(map[string]*torrentState),
		missingWarned:    make(map[string]bool),
	}
	engine.SetIndexers(cfg.Indexers)
	if err := engine.loadManagedTorrents(); err != nil {
		releaseDataDirLock(lockFile)
		return nil, err
	}
	if info, err := os.Stat(filepath.Join(cfg.DataDir, "torrents")); err == nil && info.IsDir() {
		engine.logger.Warn("legacy built-in torrent data is no longer used; Deluge re-downloads retained torrents into the downloads directory",
			"path", filepath.Join(cfg.DataDir, "torrents"))
	}
	ctx, cancel := context.WithCancel(context.Background())
	engine.cancel = cancel
	engine.wg.Add(2)
	go engine.runJanitor(ctx)
	go engine.runListenPort(ctx)
	return engine, nil
}

func acquireDataDirLock(dataDir string) (*os.File, error) {
	path := filepath.Join(dataDir, ".engine.lock")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open data directory lock: %w", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, fmt.Errorf("filmstream data directory is already in use: %s", dataDir)
	}
	return file, nil
}

func releaseDataDirLock(file *os.File) {
	_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	_ = file.Close()
}

// Close stops background work and saves state. Torrents stay in Deluge, which
// keeps seeding them while Filmstream is down.
func (e *Engine) Close() error {
	e.closeOnce.Do(func() {
		e.cancel()
		e.wg.Wait()
		e.mu.Lock()
		e.persistLocked()
		e.mu.Unlock()
		releaseDataDirLock(e.lockFile)
	})
	return nil
}

// ListenPort returns the peer port Deluge was told to use, or the one it
// reported, or 0 before Deluge answered.
func (e *Engine) ListenPort() int {
	e.mu.Lock()
	port := e.listenPort
	e.mu.Unlock()
	if port == 0 {
		port = e.desiredListenPort()
	}
	return port
}

func (e *Engine) SetCleanupHandler(handler func(string, string)) {
	e.mu.Lock()
	e.onCleanup = handler
	e.mu.Unlock()
}

// Create adds a torrent to Deluge, waits for its metadata and mounts one video
// file. It holds no engine-wide lock while downloading the .torrent file or
// waiting for magnet metadata, so a slow candidate cannot delay other
// playbacks, and it returns as soon as ctx ends.
func (e *Engine) Create(ctx context.Context, source Source) (*Session, error) {
	started := time.Now()
	configured := 0
	for _, value := range []string{source.MagnetURI, source.TorrentURL, source.TorrentPath} {
		if value != "" {
			configured++
		}
	}
	if configured != 1 {
		return nil, errors.New("exactly one torrent source must be provided")
	}

	request := pluginAddRequest{SaveRoot: e.downloadsDir, WantedFiles: "all"}
	var metainfo []byte
	sourceKind := "magnet"
	switch {
	case source.MagnetURI != "":
		request.Magnet = source.MagnetURI
	case source.TorrentURL != "":
		sourceKind = "torrent_url"
		contents, magnet, err := e.fetchTorrent(ctx, source.TorrentURL)
		if err != nil {
			return nil, err
		}
		if magnet != "" {
			request.Magnet = magnet
		} else {
			metainfo = contents
		}
	default:
		sourceKind = "torrent_path"
		contents, err := readTorrentFile(source.TorrentPath)
		if err != nil {
			return nil, err
		}
		metainfo = contents
	}
	if metainfo != nil {
		request.Torrent = encodeTorrent(metainfo)
	}
	fetchDuration := time.Since(started)

	addStarted := time.Now()
	t, err := e.addTorrent(ctx, request)
	if err != nil {
		return nil, fmt.Errorf("add %s: %w", sourceKind, err)
	}
	addDuration := time.Since(addStarted)
	registered := false
	defer func() {
		if !registered {
			e.abandon(t)
		}
	}()

	metadataStarted := time.Now()
	detail, err := e.awaitMetadata(ctx, t)
	if err != nil {
		return nil, err
	}
	metadataDuration := time.Since(metadataStarted)
	if detail.TotalSize <= 0 {
		return nil, errors.New("torrent contains no data")
	}
	if e.maxTorrentBytes > 0 && detail.TotalSize > e.maxTorrentBytes {
		return nil, fmt.Errorf("torrent is %.1f GiB; configured maximum is %.1f GiB",
			gib(detail.TotalSize), gib(e.maxTorrentBytes))
	}
	file, err := selectVideoFile(detail.Files, source.FileHint)
	if err != nil {
		return nil, err
	}
	private, rule := e.seedPolicy(source.Indexer, detail.Private)
	id, err := randomID()
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	session := &Session{
		ID: id, Name: detail.Name, FileName: displayPath(detail.Name, detail.Files, file), FileSize: file.Size,
		CreatedAt: now, torrent: t, file: file,
		localPath:    filepath.Join(detail.SavePath, filepath.FromSlash(file.Path)),
		lastActivity: now,
	}
	wanted := e.register(t, session, detail, source.Indexer, private, rule)
	registered = true

	if metainfo == nil {
		metaContext, cancel := context.WithTimeout(context.Background(), pluginCallTimeout)
		metainfo, err = e.plugin.metainfo(metaContext, t.hash)
		cancel()
		if err != nil {
			e.logger.Warn("could not save torrent metainfo", "info_hash", t.hash, "error", err)
		}
	}
	e.saveMetainfo(t.hash, metainfo)
	// Windows first: an unplayed private torrent wants no files, and with
	// nothing wanted libtorrent would count it finished and drop its seeds.
	e.setMountWindows(ctx, session)
	if err := e.plugin.setFiles(ctx, t.hash, wanted); err != nil {
		_ = e.Drop(id)
		return nil, fmt.Errorf("select torrent files: %w", err)
	}

	e.logger.Info("torrent mounted",
		"id", session.ID, "name", session.Name, "file", session.FileName, "info_hash", t.hash,
		"source_kind", sourceKind, "indexer", source.Indexer, "private", private,
		"fetch_duration", fetchDuration, "add_duration", addDuration,
		"metadata_wait_duration", metadataDuration, "total_duration", time.Since(started))
	return session, nil
}

// addTorrent adds the torrent to Deluge (idempotently) and registers a
// pending Create so concurrent cleanup cannot remove it.
func (e *Engine) addTorrent(ctx context.Context, request pluginAddRequest) (*torrentState, error) {
	e.lifecycleMu.Lock()
	defer e.lifecycleMu.Unlock()
	response, err := e.plugin.add(ctx, request)
	if err != nil {
		return nil, err
	}
	hash := strings.ToLower(response.InfoHash)
	if !validInfoHash(hash) {
		return nil, fmt.Errorf("deluge plugin returned invalid info hash %q", response.InfoHash)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	t := e.torrents[hash]
	if t == nil {
		t = &torrentState{hash: hash}
		e.torrents[hash] = t
	}
	t.pending++
	t.added = t.added || response.Added
	return t, nil
}

// abandon releases a Create that failed before registering a playback and
// removes a torrent nothing else uses.
func (e *Engine) abandon(t *torrentState) {
	e.mu.Lock()
	t.pending--
	e.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), pluginCallTimeout)
	defer cancel()
	e.removeTorrent(ctx, t, "abandoned", func(t *torrentState) bool {
		return t.record == nil && t.added
	})
}

func (e *Engine) awaitMetadata(ctx context.Context, t *torrentState) (pluginTorrent, error) {
	ctx, cancel := context.WithTimeout(ctx, e.metadataTimeout)
	defer cancel()
	delay := 50 * time.Millisecond
	for {
		detail, err := e.plugin.torrent(ctx, t.hash)
		if err == nil && detail.HasMetadata && detail.PieceLength > 0 && detail.NumPieces > 0 && len(detail.Files) > 0 {
			t.storeDetail(detail)
			return detail, nil
		}
		if errors.Is(err, errTorrentNotFound) {
			return pluginTorrent{}, fmt.Errorf("wait for torrent metadata: %w", err)
		}
		if err != nil && ctx.Err() == nil {
			e.logger.Debug("torrent metadata poll failed", "info_hash", t.hash, "error", err)
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return pluginTorrent{}, fmt.Errorf("wait for torrent metadata: %w", ctx.Err())
		case <-timer.C:
		}
		delay = min(delay*2, metadataPollMax)
	}
}

// register attaches a new playback to its torrent and returns the file
// selection Deluge should download.
func (e *Engine) register(t *torrentState, session *Session, detail pluginTorrent, indexer string, private bool, rule *config.SeedRule) any {
	e.mu.Lock()
	defer e.mu.Unlock()
	if t.numPieces == 0 {
		t.name = detail.Name
		t.pieceLength = detail.PieceLength
		t.numPieces = detail.NumPieces
		t.files = detail.Files
		t.savePath = detail.SavePath
	}
	t.pending--
	if t.sessions == nil {
		t.sessions = make(map[string]*Session)
	}
	t.sessions[session.ID] = session
	e.sessions[session.ID] = session
	record := t.record
	if record == nil {
		record = &managedTorrent{InfoHash: t.hash, CreatedAt: session.CreatedAt}
		t.record = record
	}
	record.Name = detail.Name
	if record.Indexer == "" {
		record.Indexer = indexer
	}
	if private {
		record.Private = true
		if record.Seed == nil {
			record.Seed = rule
		}
	}
	record.addFile(session.file.Index)
	record.LastActivity = session.CreatedAt
	e.persistLocked()
	if record.WantAll {
		t.wantAllPushed = true
	}
	return record.wanted()
}

func (e *Engine) setMountWindows(ctx context.Context, session *Session) {
	windows := map[string]pluginWindow{
		"mount-head-" + session.ID: {
			File: session.file.Index, Offset: 0, Length: min(mountHeadBytes, session.FileSize),
			DeadlineBytes: mountHeadBytes, TTLMillis: mountWindowTTL.Milliseconds(),
		},
	}
	if session.FileSize > 2*mountHeadBytes {
		windows["mount-tail-"+session.ID] = pluginWindow{
			File: session.file.Index, Offset: session.FileSize - mountTailBytes, Length: mountTailBytes,
			DeadlineBytes: mountTailBytes, TTLMillis: mountWindowTTL.Milliseconds(),
		}
	}
	for stream, window := range windows {
		if err := e.plugin.setWindow(ctx, session.torrent.hash, stream, window); err != nil {
			e.logger.Warn("could not prioritize playback start", "id", session.ID, "error", err)
			return
		}
	}
}

func (e *Engine) removeMountWindows(session *Session) {
	ctx, cancel := context.WithTimeout(context.Background(), pluginCallTimeout)
	defer cancel()
	for _, stream := range []string{"mount-head-" + session.ID, "mount-tail-" + session.ID} {
		if err := e.plugin.removeWindow(ctx, session.torrent.hash, stream); err != nil && !errors.Is(err, errTorrentNotFound) {
			e.logger.Debug("could not remove playback start window", "id", session.ID, "error", err)
		}
	}
}

func (e *Engine) Get(id string) (*Session, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	session, ok := e.sessions[id]
	return session, ok
}

// LocalFilePath returns the playback file's path once Deluge has verified
// every piece of it, so callers may read it directly.
func (e *Engine) LocalFilePath(id string) (string, bool) {
	session, ok := e.Get(id)
	if !ok {
		return "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), pluginCallTimeout)
	defer cancel()
	detail, err := e.fetchDetail(ctx, session.torrent)
	if err != nil || !fileComplete(detail, session.file) {
		return "", false
	}
	info, err := os.Stat(session.localPath)
	if err != nil || !info.Mode().IsRegular() || info.Size() != session.FileSize {
		return "", false
	}
	return session.localPath, true
}

func fileComplete(detail pluginTorrent, file pluginFile) bool {
	return file.Index < len(detail.FileProgress) && detail.FileProgress[file.Index] >= file.Size
}

func (e *Engine) TorrentMetainfo(id string) ([]byte, error) {
	session, ok := e.Get(id)
	if !ok {
		return nil, errors.New("playback not found")
	}
	if contents, err := os.ReadFile(e.managedMetainfoPath(session.torrent.hash)); err == nil && len(contents) > 0 {
		return contents, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), pluginCallTimeout)
	defer cancel()
	contents, err := e.plugin.metainfo(ctx, session.torrent.hash)
	if err != nil {
		return nil, fmt.Errorf("read torrent metainfo: %w", err)
	}
	return contents, nil
}

// Drop discards a playback that will not be used. Its torrent is removed with
// its data unless another playback uses it or it carries a seeding obligation.
func (e *Engine) Drop(id string) error {
	e.mu.Lock()
	session, ok := e.sessions[id]
	if !ok {
		e.mu.Unlock()
		return errors.New("playback not found")
	}
	if session.activeStreams > 0 {
		e.mu.Unlock()
		return errors.New("playback is active")
	}
	t := session.torrent
	delete(e.sessions, id)
	delete(t.sessions, id)
	unused := len(t.sessions) == 0 && t.pending == 0
	var record managedTorrent
	if t.record != nil {
		record = *t.record
	}
	handler := e.onCleanup
	e.mu.Unlock()

	e.removeMountWindows(session)
	if unused {
		ctx, cancel := context.WithTimeout(context.Background(), pluginCallTimeout)
		switch {
		case record.Started || record.Obligated:
			e.logger.Info("retained rejected torrent to satisfy seeding requirement", "name", session.Name, "info_hash", t.hash)
		case record.Private && e.privateDownloadObligates(ctx, t):
			// Enough was downloaded to count as a snatch on the tracker.
		default:
			e.removeTorrent(ctx, t, "rejected", func(t *torrentState) bool {
				return len(t.sessions) == 0 && (t.record == nil || !t.record.Started && !t.record.Obligated)
			})
		}
		cancel()
	}
	if handler != nil {
		handler(id, "rejected")
	}
	return nil
}

// privateDownloadObligates marks a never-played private torrent obligated
// once it downloaded enough to count as a snatch. It also reports true when
// Deluge cannot be asked, so nothing is removed on a guess.
func (e *Engine) privateDownloadObligates(ctx context.Context, t *torrentState) bool {
	detail, err := e.fetchDetail(ctx, t)
	if err != nil {
		e.logger.Warn("kept private torrent because its download volume is unknown", "info_hash", t.hash, "error", err)
		return true
	}
	if !obligationReached(detail) {
		return false
	}
	e.obligate(ctx, t, "downloaded_bytes", detail.AllTimeDownload)
	return true
}

// removeTorrent removes a torrent and its data from Deluge when still
// eligible, and forgets it. It returns the playback IDs that were removed.
func (e *Engine) removeTorrent(ctx context.Context, t *torrentState, reason string, eligible func(*torrentState) bool) ([]string, bool) {
	e.lifecycleMu.Lock()
	defer e.lifecycleMu.Unlock()
	e.mu.Lock()
	if e.torrents[t.hash] != t || t.pending > 0 || !eligible(t) {
		e.mu.Unlock()
		return nil, false
	}
	for _, session := range t.sessions {
		if session.activeStreams > 0 {
			e.mu.Unlock()
			return nil, false
		}
	}
	e.mu.Unlock()

	// Only a torrent Deluge no longer has may be forgotten; otherwise it would
	// keep downloading unmanaged.
	if err := e.plugin.remove(ctx, t.hash, true); err != nil && !errors.Is(err, errTorrentNotFound) {
		e.logger.Warn("could not remove torrent from Deluge", "reason", reason, "info_hash", t.hash, "error", err)
		return nil, false
	}
	e.mu.Lock()
	ids := make([]string, 0, len(t.sessions))
	for id := range t.sessions {
		delete(e.sessions, id)
		ids = append(ids, id)
	}
	t.sessions = nil
	delete(e.torrents, t.hash)
	delete(e.missingWarned, t.hash)
	if t.record != nil {
		e.persistLocked()
	}
	e.mu.Unlock()
	e.removeMetainfo(t.hash)
	e.logger.Info("removed torrent", "reason", reason, "info_hash", t.hash, "name", t.name)
	return ids, true
}

// obligate commits a private torrent to be completed and seeded.
func (e *Engine) obligate(ctx context.Context, t *torrentState, args ...any) {
	e.mu.Lock()
	if t.record == nil {
		e.mu.Unlock()
		return
	}
	changed := !t.record.Obligated || !t.record.WantAll
	t.record.Obligated = true
	t.record.WantAll = true
	if changed {
		e.persistLocked()
	}
	e.mu.Unlock()
	if changed {
		e.logger.Info("private torrent must now be completed and seeded", append([]any{"info_hash", t.hash, "name", t.name}, args...)...)
	}
	e.pushWantAll(ctx, t)
}

func (e *Engine) pushWantAll(ctx context.Context, t *torrentState) {
	if err := e.ensureWantAll(ctx, t); err != nil {
		e.logger.Warn("could not request every file of a torrent", "info_hash", t.hash, "error", err)
	}
}

func (e *Engine) Status(id string) (Status, bool) {
	e.mu.Lock()
	session, ok := e.sessions[id]
	if !ok {
		e.mu.Unlock()
		return Status{}, false
	}
	t := session.torrent
	state := "ready"
	if session.activeStreams > 0 {
		state = "streaming"
	} else if session.served {
		state = "seeding"
	}
	status := Status{
		ID: session.ID, Name: session.Name, FileName: session.FileName, FileSize: session.FileSize,
		State: state, ActiveStreams: session.activeStreams, LastActivity: session.lastActivity,
		InfoHash: t.hash,
	}
	now := time.Now()
	status.SourceUnavailable = session.unavailable != nil && now.Before(session.unavailableUntil)
	if !session.serveDeadline.IsZero() {
		deadline := session.serveDeadline
		status.ServeDeadline = &deadline
	}
	var record managedTorrent
	if t.record != nil {
		record = *t.record
	}
	served := session.served
	e.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), pluginCallTimeout)
	defer cancel()
	detail, err := e.detail(ctx, t)
	if err != nil {
		e.logger.Debug("torrent status unavailable", "id", id, "error", err)
	}
	seed := e.seedState(record, detail)
	if session.file.Index < len(detail.FileProgress) {
		status.BytesComplete = detail.FileProgress[session.file.Index]
	}
	status.TorrentBytes = detail.TotalSize
	status.TorrentComplete = detail.TotalDone
	status.DownloadedBytes = detail.AllTimeDownload
	status.UploadedBytes = detail.AllTimeUpload
	status.Ratio = seed.ratio
	status.RatioTarget = seed.ratioTarget
	status.RatioTargetMet = seed.ratioMet
	status.TotalPeers = detail.ListPeers
	status.PendingPeers = detail.ConnectCandidates
	status.ActivePeers = detail.NumPeers
	status.ConnectedSeeders = detail.NumSeeds
	status.OutboundDialObserved = detail.ListPeers > 0
	if session.FileSize > 0 {
		status.CachedPercent = int(100 * status.BytesComplete / session.FileSize)
	}
	status.DownloadRate = detail.DownloadRate
	status.UploadRate = detail.UploadRate
	status.Progress = detail.Progress
	status.Private = record.Private || detail.Private
	status.Snatched = status.Private && (record.Started || record.Obligated)
	status.SeedingSeconds = detail.SeedingSeconds
	status.SeedRequirementMet = seed.met
	status.TrackerMessage = detail.TrackerMessage
	if served && status.ActiveStreams == 0 {
		if remaining, known := seed.remaining(); known {
			deadline := now.UTC().Add(remaining)
			status.SeedDeadline = &deadline
		}
	}
	return status, true
}

// SourceUnavailable returns the current unavailable verdict for a playback,
// if any. HLS uses it to stop probes and packagers early.
func (e *Engine) SourceUnavailable(id string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	session, ok := e.sessions[id]
	if !ok || session.unavailable == nil || !time.Now().Before(session.unavailableUntil) {
		return nil
	}
	return session.unavailable
}

// MarkSourceUnavailable records a playback whose consumer saw the source stop
// advancing. Blocked reads fail until the verdict expires or data arrives.
func (e *Engine) MarkSourceUnavailable(id string, cause error) error {
	session, ok := e.Get(id)
	if !ok {
		return nil
	}
	if cause == nil {
		cause = errors.New("source stopped advancing")
	}
	return e.markUnavailable(session, cause)
}

func (e *Engine) markUnavailable(session *Session, cause error) error {
	err := fmt.Errorf("%w: %w", ErrSourceUnavailable, cause)
	e.mu.Lock()
	if session.unavailable != nil && time.Now().Before(session.unavailableUntil) {
		err = session.unavailable
	} else {
		session.unavailable = err
		session.unavailableUntil = time.Now().Add(sourceUnavailableHold)
	}
	e.mu.Unlock()
	t := session.torrent
	t.detailMu.Lock()
	detail := t.detail
	t.detailMu.Unlock()
	e.logger.Warn("playback source unavailable",
		"id", session.ID, "name", session.Name, "file", session.FileName, "info_hash", t.hash,
		"connected_peers", detail.NumPeers, "connected_seeders", detail.NumSeeds,
		"known_peers", detail.ListPeers, "download_rate", detail.DownloadRate,
		"tracker_status", detail.TrackerStatus, "hold", sourceUnavailableHold, "error", cause)
	return err
}

func (e *Engine) ServeHTTP(w http.ResponseWriter, r *http.Request, id string) error {
	session, ok := e.beginStream(id)
	if !ok {
		return errors.New("playback not found")
	}
	defer e.endStream(id)

	reader := e.newReader(r.Context(), session)
	defer reader.Close()
	// HEAD never reads data. Other requests first wait for their first byte so
	// an unavailable source is reported as an error instead of a stalled body.
	if r.Method != http.MethodHead {
		if err := reader.prepare(requestReadStart(r, session.FileSize)); err != nil {
			return err
		}
	}
	if mediaType := mime.TypeByExtension(strings.ToLower(path.Ext(session.FileName))); mediaType != "" {
		w.Header().Set("Content-Type", mediaType)
	}
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, path.Base(session.FileName), session.CreatedAt, reader)
	return nil
}

// requestReadStart returns the position a read request begins at, falling back
// to the start of the file for absent or unparseable range headers.
func requestReadStart(r *http.Request, size int64) int64 {
	rangeHeader := r.Header.Get("Range")
	if rangeHeader == "" || !strings.HasPrefix(rangeHeader, "bytes=") {
		return 0
	}
	spec := strings.SplitN(strings.TrimPrefix(rangeHeader, "bytes="), ",", 2)[0]
	dash := strings.IndexByte(spec, '-')
	if dash < 0 {
		return 0
	}
	first := strings.TrimSpace(spec[:dash])
	if first == "" {
		// Suffix range: the last N bytes of the file, for example an MKV cue
		// seek near the end.
		if suffix, err := strconv.ParseInt(strings.TrimSpace(spec[dash+1:]), 10, 64); err == nil && suffix > 0 {
			return max(0, size-suffix)
		}
		return 0
	}
	start, err := strconv.ParseInt(first, 10, 64)
	if err != nil || start < 0 || start >= size {
		return 0
	}
	return start
}

// beginStream counts an HTTP reader.
func (e *Engine) beginStream(id string) (*Session, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	session, ok := e.sessions[id]
	if !ok {
		return nil, false
	}
	now := time.Now().UTC()
	session.activeStreams++
	session.lastActivity = now
	if record := session.torrent.record; record != nil {
		record.LastActivity = now
	}
	return session, true
}

func (e *Engine) endStream(id string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if session, ok := e.sessions[id]; ok {
		if session.activeStreams > 0 {
			session.activeStreams--
		}
		now := time.Now().UTC()
		session.lastActivity = now
		if record := session.torrent.record; record != nil {
			record.LastActivity = now
		}
	}
}

// detail returns the torrent's Deluge status, at most detailMaxAge old.
func (e *Engine) detail(ctx context.Context, t *torrentState) (pluginTorrent, error) {
	t.detailMu.Lock()
	defer t.detailMu.Unlock()
	if !t.detailAt.IsZero() && time.Since(t.detailAt) < detailMaxAge {
		return t.detail, nil
	}
	detail, err := e.plugin.torrent(ctx, t.hash)
	if err != nil {
		return t.detail, err
	}
	t.storeDetailLocked(detail)
	return detail, nil
}

// fetchDetail returns fresh Deluge status.
func (e *Engine) fetchDetail(ctx context.Context, t *torrentState) (pluginTorrent, error) {
	detail, err := e.plugin.torrent(ctx, t.hash)
	if err != nil {
		return pluginTorrent{}, err
	}
	t.storeDetail(detail)
	return detail, nil
}

func (t *torrentState) storeDetail(detail pluginTorrent) {
	t.detailMu.Lock()
	t.storeDetailLocked(detail)
	t.detailMu.Unlock()
}

// storeDetailLocked caches status and tracks when the torrent last gained
// verified data, which read stall detection relies on.
func (t *torrentState) storeDetailLocked(detail pluginTorrent) {
	now := time.Now()
	t.detail = detail
	t.detailAt = now
	if t.lastProgressAt.IsZero() || detail.TotalDone > t.lastDone {
		t.lastDone = detail.TotalDone
		t.lastProgressAt = now
	}
}

func (e *Engine) lastProgress(t *torrentState) time.Time {
	t.detailMu.Lock()
	defer t.detailMu.Unlock()
	return t.lastProgressAt
}

// fetchTorrent downloads a .torrent file, or returns the magnet URI an indexer
// redirected to.
func (e *Engine) fetchTorrent(ctx context.Context, torrentURL string) ([]byte, string, error) {
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		attemptContext, cancel := context.WithTimeout(ctx, torrentFetchTimeout)
		contents, magnet, retry, err := e.fetchTorrentOnce(attemptContext, torrentURL)
		cancel()
		if err == nil {
			return contents, magnet, nil
		}
		if ctx.Err() != nil {
			return nil, "", fmt.Errorf("download torrent file: %w", ctx.Err())
		}
		lastErr = err
		if !retry || attempt == 3 {
			break
		}
		timer := time.NewTimer(time.Duration(attempt) * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, "", fmt.Errorf("download torrent file: %w", ctx.Err())
		case <-timer.C:
		}
	}
	return nil, "", fmt.Errorf("download torrent file: %w", lastErr)
}

func (e *Engine) fetchTorrentOnce(ctx context.Context, torrentURL string) (contents []byte, magnet string, retry bool, err error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, torrentURL, nil)
	if err != nil {
		return nil, "", false, err
	}
	request.Header.Set("User-Agent", "filmstream/0.1")
	response, err := e.fetchClient.Do(request)
	if err != nil {
		return nil, "", true, err
	}
	defer response.Body.Close()
	if location := response.Header.Get("Location"); response.StatusCode >= 300 && response.StatusCode < 400 &&
		strings.HasPrefix(strings.ToLower(location), "magnet:") {
		return nil, location, false, nil
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return nil, "", response.StatusCode >= 500 || response.StatusCode == http.StatusTooManyRequests,
			fmt.Errorf("download torrent file returned %s", response.Status)
	}
	contents, err = io.ReadAll(io.LimitReader(response.Body, maxMetainfoBytes+1))
	if err != nil {
		return nil, "", true, err
	}
	return parseTorrentPayload(contents)
}

func parseTorrentPayload(contents []byte) ([]byte, string, bool, error) {
	if len(contents) > maxMetainfoBytes {
		return nil, "", false, fmt.Errorf("torrent file exceeds %d bytes", maxMetainfoBytes)
	}
	if trimmed := strings.TrimSpace(string(contents[:min(len(contents), 8192)])); strings.HasPrefix(strings.ToLower(trimmed), "magnet:") {
		return nil, trimmed, false, nil
	}
	if len(contents) == 0 || contents[0] != 'd' {
		return nil, "", false, errors.New("response is not a torrent file")
	}
	return contents, "", false, nil
}

func readTorrentFile(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("load torrent file: %w", err)
	}
	defer file.Close()
	contents, err := io.ReadAll(io.LimitReader(file, maxMetainfoBytes+1))
	if err != nil {
		return nil, fmt.Errorf("load torrent file: %w", err)
	}
	contents, magnet, _, err := parseTorrentPayload(contents)
	if err != nil {
		return nil, fmt.Errorf("load torrent file: %w", err)
	}
	if magnet != "" {
		return nil, errors.New("load torrent file: file contains a magnet URI, not a torrent")
	}
	return contents, nil
}

// runListenPort pushes the configured peer port to Deluge and follows the
// port file, whose value can change when the VPN reconnects.
func (e *Engine) runListenPort(ctx context.Context) {
	defer e.wg.Done()
	pushed := 0
	healthChecked := false
	var lastErr string
	ticker := time.NewTicker(listenPortPoll)
	defer ticker.Stop()
	for {
		if !healthChecked {
			if health, err := e.plugin.health(ctx); err == nil {
				healthChecked = true
				e.logDelugeHealth(health)
				e.mu.Lock()
				if e.listenPort == 0 {
					e.listenPort = health.ListenPort
				}
				e.mu.Unlock()
			}
		}
		if port := e.desiredListenPort(); port > 0 && port != pushed {
			if err := e.plugin.setListenPort(ctx, port); err != nil {
				if ctx.Err() == nil && err.Error() != lastErr {
					e.logger.Warn("could not set Deluge listen port", "port", port, "error", err)
					lastErr = err.Error()
				}
			} else {
				e.logger.Info("set Deluge listen port", "port", port, "previous", pushed)
				pushed = port
				lastErr = ""
				e.mu.Lock()
				e.listenPort = port
				e.mu.Unlock()
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (e *Engine) desiredListenPort() int {
	if e.listenPortFile != "" {
		if contents, err := os.ReadFile(e.listenPortFile); err == nil {
			if port, err := strconv.Atoi(strings.TrimSpace(string(contents))); err == nil && port > 0 && port <= 65535 {
				return port
			}
		}
	}
	return e.listenPortFixed
}

func (e *Engine) logDelugeHealth(health pluginHealth) {
	e.logger.Info("connected to Deluge", "deluge", health.Deluge, "libtorrent", health.Libtorrent,
		"listen_port", health.ListenPort)
	if health.DHT || health.LSD || health.UTPEX || health.UPnP || health.NATPMP || health.PEXLoaded {
		e.logger.Warn("Deluge peer discovery that private trackers forbid is enabled",
			"dht", health.DHT, "lsd", health.LSD, "pex", health.UTPEX, "pex_loaded", health.PEXLoaded,
			"upnp", health.UPnP, "natpmp", health.NATPMP)
	}
}

func randomID() (string, error) {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("create playback ID: %w", err)
	}
	return hex.EncodeToString(bytes), nil
}

func gib(bytes int64) float64 {
	return float64(bytes) / float64(int64(1)<<30)
}
