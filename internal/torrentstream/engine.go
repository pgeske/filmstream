package torrentstream

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
)

const (
	maxMetainfoBytes       = 16 << 20
	defaultCacheLimit      = int64(20 << 30)
	defaultMaxSeedSessions = 20
	defaultIdleGrace       = 2 * time.Minute
	defaultSeedMaxAge      = 168 * time.Hour
	defaultCleanupInterval = 30 * time.Second

	// A source read only makes progress once the pieces it needs are complete
	// locally or a connected peer can deliver them. Bound how long a read waits
	// for that so a playback mounted on a dead swarm fails quickly and
	// diagnosably instead of stalling until the client times out.
	defaultServeReadinessWait = 15 * time.Second
	serviceReadinessPoll      = 250 * time.Millisecond
	serviceReadinessLogEvery  = 3 * time.Second
	localVerifyBudget         = 8 * time.Second
)

var videoExtensions = map[string]bool{
	".avi": true, ".m4v": true, ".mkv": true, ".mov": true,
	".mp4": true, ".mpeg": true, ".mpg": true, ".ts": true, ".webm": true,
}

type Config struct {
	DataDir            string
	ListenPort         int
	MaxTorrentBytes    int64
	ReadaheadBytes     int64
	MetadataTimeout    time.Duration
	SeedRatioTarget    float64
	CacheLimitBytes    int64
	MaxSeedSessions    int
	IdleGrace          time.Duration
	SeedMaxAge         time.Duration
	CleanupInterval    time.Duration
	ServeReadinessWait time.Duration
	CleanOnStart       bool
	CleanOnClose       bool
	Logger             *slog.Logger
}

type Source struct {
	MagnetURI   string
	TorrentURL  string
	TorrentPath string
	FileHint    string
}

type Engine struct {
	client           *torrent.Client
	httpClient       *http.Client
	dataDir          string
	managedDir       string
	managedStatePath string
	maxTorrentBytes  int64
	readaheadBytes   int64
	metadataTimeout  time.Duration
	seedRatioTarget  float64
	cacheLimitBytes  int64
	maxSeedSessions  int
	idleGrace        time.Duration
	seedMaxAge       time.Duration
	cleanupInterval  time.Duration
	serveWait        time.Duration
	cleanOnClose     bool
	logger           *slog.Logger
	lockFile         *os.File

	lifecycleMu sync.Mutex
	mu          sync.RWMutex
	serveMu     sync.Mutex
	sessions    map[string]*Session
	managed     map[*torrent.Torrent]*managedTorrent
	onCleanup   func(string, string)

	cleanupCancel context.CancelFunc
	cleanupWG     sync.WaitGroup
	restoreWG     sync.WaitGroup
	closeOnce     sync.Once
}

type Session struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	FileName  string    `json:"file_name"`
	FileSize  int64     `json:"file_size"`
	CreatedAt time.Time `json:"created_at"`

	torrent       *torrent.Torrent
	file          *torrent.File
	activeStreams int
	started       bool
	lastActivity  time.Time

	serveDeadline       time.Time
	serveUnavailable    error
	serveInitialPending int
	serveDialObserved   bool
}

// ErrSourceUnavailable identifies a torrent that exhausted its one startup
// readiness budget without local coverage or a peer that could serve it.
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
}

func New(cfg Config) (*Engine, error) {
	if cfg.ListenPort < 0 || cfg.ListenPort > 65535 {
		return nil, fmt.Errorf("listen port must be between 0 and 65535: %d", cfg.ListenPort)
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
	if cfg.ServeReadinessWait <= 0 {
		cfg.ServeReadinessWait = defaultServeReadinessWait
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
	unlock := func() {
		_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
		_ = lockFile.Close()
	}

	torrentDataDir := filepath.Join(cfg.DataDir, "torrents")
	managedDir := filepath.Join(cfg.DataDir, "managed-torrents")
	managedStatePath := filepath.Join(cfg.DataDir, "managed-torrents.json")
	if cfg.CleanOnStart {
		for _, path := range []string{torrentDataDir, managedDir, managedStatePath} {
			if err := os.RemoveAll(path); err != nil {
				unlock()
				return nil, fmt.Errorf("clear stale torrent cache: %w", err)
			}
		}
	}
	if err := os.MkdirAll(torrentDataDir, 0o755); err != nil {
		unlock()
		return nil, fmt.Errorf("create torrent data directory: %w", err)
	}
	if err := repairSparseCompletedMedia(torrentDataDir, cfg.Logger); err != nil {
		unlock()
		return nil, err
	}
	clientConfig := torrent.NewDefaultClientConfig()
	clientConfig.DataDir = torrentDataDir
	clientConfig.ListenPort = cfg.ListenPort
	clientConfig.Seed = true
	clientConfig.NoUpload = false
	clientConfig.Slogger = slog.New(slog.NewTextHandler(io.Discard, nil))

	client, err := torrent.NewClient(clientConfig)
	if err != nil {
		unlock()
		return nil, fmt.Errorf("create torrent client: %w", err)
	}
	httpTransport := http.DefaultTransport.(*http.Transport).Clone()
	httpTransport.TLSHandshakeTimeout = 30 * time.Second
	engine := &Engine{
		client:           client,
		httpClient:       &http.Client{Timeout: cfg.MetadataTimeout, Transport: httpTransport},
		dataDir:          torrentDataDir,
		managedDir:       managedDir,
		managedStatePath: managedStatePath,
		maxTorrentBytes:  cfg.MaxTorrentBytes,
		readaheadBytes:   cfg.ReadaheadBytes,
		metadataTimeout:  cfg.MetadataTimeout,
		seedRatioTarget:  cfg.SeedRatioTarget,
		cacheLimitBytes:  cfg.CacheLimitBytes,
		maxSeedSessions:  cfg.MaxSeedSessions,
		idleGrace:        cfg.IdleGrace,
		seedMaxAge:       cfg.SeedMaxAge,
		cleanupInterval:  cfg.CleanupInterval,
		serveWait:        cfg.ServeReadinessWait,
		cleanOnClose:     cfg.CleanOnClose,
		logger:           cfg.Logger,
		lockFile:         lockFile,
		sessions:         make(map[string]*Session),
		managed:          make(map[*torrent.Torrent]*managedTorrent),
	}
	engine.restoreWG.Add(1)
	go func() {
		defer engine.restoreWG.Done()
		if err := engine.restoreManagedTorrents(); err != nil {
			engine.logger.Error("could not restore managed torrents", "error", err)
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	engine.cleanupCancel = cancel
	engine.cleanupWG.Add(1)
	go engine.runJanitor(ctx)
	if cfg.CleanOnStart {
		engine.logger.Info("cleared stale torrent cache from previous server run")
	}
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

// Part-file storage treats a final media path as complete after a restart. Demote
// sparse final files so missing pieces are downloaded instead of served as zeroes,
// and drop part files that shadow complete media: chunk writes for pieces shared
// with adjacent files can recreate a part file after its media was promoted, and
// reads prefer the part file, which would serve holes over verified data.
func repairSparseCompletedMedia(dataDir string, logger *slog.Logger) error {
	return filepath.Walk(dataDir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			// Entries can vanish mid-walk because this pass removes files itself.
			if errors.Is(walkErr, os.ErrNotExist) {
				return nil
			}
			return walkErr
		}
		if !info.Mode().IsRegular() || strings.HasSuffix(path, ".part") ||
			!videoExtensions[strings.ToLower(filepath.Ext(path))] || info.Size() == 0 {
			return nil
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return nil
		}
		if stat.Blocks*512*2 >= info.Size() {
			// Not sparse: the final file holds verified complete data. A sibling
			// part file can only be a stale shadow from later chunk writes.
			partPath := path + ".part"
			if err := os.Remove(partPath); err == nil {
				logger.Warn("removed part file shadowing complete media", "part", partPath)
			} else if !errors.Is(err, os.ErrNotExist) {
				logger.Warn("remove part file shadowing complete media", "part", partPath, "error", err)
			}
			return nil
		}

		partPath := path + ".part"
		if _, err := os.Stat(partPath); err == nil {
			if err := os.Remove(path); err != nil {
				return fmt.Errorf("remove sparse completed torrent file %s: %w", path, err)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect partial torrent file %s: %w", partPath, err)
		} else if err := os.Rename(path, partPath); err != nil {
			return fmt.Errorf("demote sparse completed torrent file %s: %w", path, err)
		}
		logger.Warn("demoted sparse completed torrent file for recovery", "file", path,
			"size_bytes", info.Size(), "allocated_bytes", stat.Blocks*512)
		return nil
	})
}

func (e *Engine) Close() error {
	e.closeOnce.Do(func() {
		e.cleanupCancel()
		e.cleanupWG.Wait()
		e.restoreWG.Wait()
		e.lifecycleMu.Lock()
		e.mu.Lock()
		now := time.Now().UTC()
		for torrent := range e.managed {
			e.accountManagedTorrentLocked(torrent, now)
		}
		if err := e.persistManagedTorrentsLocked(); err != nil {
			e.logger.Warn("could not save managed torrent state during shutdown", "error", err)
		}
		e.mu.Unlock()
		e.client.Close()
		if e.cleanOnClose {
			for _, path := range []string{e.dataDir, e.managedDir, e.managedStatePath} {
				if err := os.RemoveAll(path); err != nil {
					e.logger.Warn("could not clear torrent cache during shutdown", "error", err)
				}
			}
		}
		e.lifecycleMu.Unlock()
		_ = syscall.Flock(int(e.lockFile.Fd()), syscall.LOCK_UN)
		_ = e.lockFile.Close()
	})
	return nil
}

// ListenPort returns the TCP and UDP port used for incoming peers.
func (e *Engine) ListenPort() int {
	return e.client.LocalPort()
}

func (e *Engine) SetCleanupHandler(handler func(string, string)) {
	e.mu.Lock()
	e.onCleanup = handler
	e.mu.Unlock()
}

func (e *Engine) Create(ctx context.Context, source Source) (*Session, error) {
	started := time.Now()
	configured := 0
	if source.MagnetURI != "" {
		configured++
	}
	if source.TorrentURL != "" {
		configured++
	}
	if source.TorrentPath != "" {
		configured++
	}
	if configured != 1 {
		return nil, errors.New("exactly one torrent source must be provided")
	}

	sourceKind := "torrent_url"
	if source.MagnetURI != "" {
		sourceKind = "magnet"
	} else if source.TorrentPath != "" {
		sourceKind = "torrent_path"
	}
	e.lifecycleMu.Lock()
	defer e.lifecycleMu.Unlock()
	addStarted := time.Now()
	t, err := e.addTorrent(ctx, source)
	addSourceDuration := time.Since(addStarted)
	if err != nil {
		return nil, err
	}
	metadataContext, cancel := context.WithTimeout(ctx, e.metadataTimeout)
	defer cancel()
	metadataStarted := time.Now()
	select {
	case <-t.GotInfo():
	case <-metadataContext.Done():
		t.Drop()
		return nil, fmt.Errorf("wait for torrent metadata: %w", metadataContext.Err())
	}
	metadataWaitDuration := time.Since(metadataStarted)

	if length := t.Length(); length <= 0 {
		t.Drop()
		return nil, errors.New("torrent contains no data")
	} else if e.maxTorrentBytes > 0 && length > e.maxTorrentBytes {
		t.Drop()
		return nil, fmt.Errorf("torrent is %.1f GiB; configured maximum is %.1f GiB", gib(length), gib(e.maxTorrentBytes))
	}

	fileSelectionStarted := time.Now()
	file, err := selectVideoFile(t.Files(), source.FileHint)
	if err != nil {
		t.Drop()
		return nil, err
	}
	fileSelectionDuration := time.Since(fileSelectionStarted)
	registrationStarted := time.Now()
	id, err := randomID()
	if err != nil {
		t.Drop()
		return nil, err
	}
	now := time.Now().UTC()
	session := &Session{
		ID: id, Name: t.Name(), FileName: file.DisplayPath(), FileSize: file.Length(), CreatedAt: now,
		torrent: t, file: file, lastActivity: now,
	}
	e.mu.Lock()
	if err := e.ensureManagedTorrentLocked(t, source.FileHint, now); err != nil {
		e.mu.Unlock()
		t.Drop()
		return nil, err
	}
	e.sessions[id] = session
	e.mu.Unlock()
	registrationDuration := time.Since(registrationStarted)

	// Metadata selection does not request payload data. The first source HTTP
	// read establishes demand for only its requested range, so rejected ranked
	// candidates never become private-tracker HnR obligations.
	e.logger.Info("torrent mount stages",
		"id", session.ID, "name", session.Name, "source_kind", sourceKind,
		"add_source_duration", addSourceDuration,
		"metadata_wait_duration", metadataWaitDuration,
		"file_selection_duration", fileSelectionDuration,
		"session_registration_duration", registrationDuration,
		"announce_async", true, "payload_demand", false,
		"total_duration", time.Since(started))
	return session, nil
}

func (e *Engine) Get(id string) (*Session, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	session, ok := e.sessions[id]
	return session, ok
}

// LocalFilePath returns the selected media path only when the torrent client
// has promoted a fully downloaded file into its final location. Callers can
// then inspect the file directly without bypassing torrent reads for partial
// data, whose sparse holes must continue to be filled through NewReader.
func (e *Engine) LocalFilePath(id string) (string, bool) {
	e.mu.RLock()
	session, ok := e.sessions[id]
	e.mu.RUnlock()
	if !ok || session.file.BytesCompleted() < session.file.Length() {
		return "", false
	}
	path := filepath.Join(e.dataDir, filepath.FromSlash(session.file.Path()))
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() != session.file.Length() {
		return "", false
	}
	return path, true
}

func (e *Engine) TorrentMetainfo(id string) ([]byte, error) {
	e.mu.RLock()
	session, ok := e.sessions[id]
	e.mu.RUnlock()
	if !ok {
		return nil, errors.New("playback not found")
	}
	meta := session.torrent.Metainfo()
	var contents bytes.Buffer
	if err := meta.Write(&contents); err != nil {
		return nil, fmt.Errorf("encode torrent metainfo: %w", err)
	}
	return contents.Bytes(), nil
}

func (e *Engine) Drop(id string) error {
	e.lifecycleMu.Lock()
	defer e.lifecycleMu.Unlock()

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
	shared := false
	for candidateID, candidate := range e.sessions {
		if candidateID != id && candidate.torrent == session.torrent {
			shared = true
			break
		}
	}
	retainForSeeding := false
	if state := e.managed[session.torrent]; !shared && state != nil && state.Started {
		retainedID, err := randomID()
		if err != nil {
			e.mu.Unlock()
			return err
		}
		e.serveMu.Lock()
		retained := *session
		e.serveMu.Unlock()
		retained.ID = retainedID
		retained.activeStreams = 0
		retained.started = true
		e.sessions[retainedID] = &retained
		retainForSeeding = true
	}
	delete(e.sessions, id)
	if !shared && !retainForSeeding {
		e.removeManagedTorrentLocked(session.torrent)
		if err := e.persistManagedTorrentsLocked(); err != nil {
			e.logger.Warn("could not save managed torrent state", "error", err)
		}
	}
	handler := e.onCleanup
	e.mu.Unlock()

	if !shared && !retainForSeeding {
		paths := e.torrentDataPaths(session.torrent)
		session.torrent.Drop()
		if err := e.removeTorrentData(paths); err != nil {
			e.logger.Warn("could not fully remove rejected torrent cache", "error", err)
		}
	} else if retainForSeeding {
		e.logger.Info("retained rejected torrent to satisfy seeding requirement", "name", session.Name)
	}
	if handler != nil {
		handler(id, "rejected")
	}
	return nil
}

func (e *Engine) Status(id string) (Status, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	session, ok := e.sessions[id]
	if !ok {
		return Status{}, false
	}
	state := "ready"
	if session.activeStreams > 0 {
		state = "streaming"
	} else if session.started {
		state = "seeding"
	}
	activeStreams := session.activeStreams
	lastActivity := session.lastActivity
	started := session.started
	t := session.torrent
	file := session.file
	status := Status{
		ID: session.ID, Name: session.Name, FileName: session.FileName, FileSize: session.FileSize,
		State: state, ActiveStreams: activeStreams, LastActivity: lastActivity,
	}
	downloaded, uploaded, seededFor := e.managedTransferTotalsLocked(t, time.Now().UTC())
	if started && activeStreams == 0 {
		remaining := max(0, e.seedMaxAge-seededFor)
		deadline := time.Now().UTC().Add(remaining)
		status.SeedDeadline = &deadline
	}
	stats := t.Stats()
	ratio := transferRatio(downloaded, uploaded)
	status.BytesComplete = file.BytesCompleted()
	status.TorrentBytes = t.Length()
	status.TorrentComplete = t.BytesCompleted()
	status.DownloadedBytes = downloaded
	status.UploadedBytes = uploaded
	status.Ratio = ratio
	status.RatioTarget = e.seedRatioTarget
	status.RatioTargetMet = ratioTargetMet(downloaded, ratio, e.seedRatioTarget)
	status.TotalPeers = stats.TotalPeers
	status.PendingPeers = stats.PendingPeers
	status.ActivePeers = stats.ActivePeers
	status.ConnectedSeeders = stats.ConnectedSeeders
	status.HalfOpenPeers = stats.HalfOpenPeers
	status.InfoHash = t.InfoHash().HexString()
	status.CachedPercent = localCoveragePercent(session)
	e.serveMu.Lock()
	status.SourceUnavailable = session.serveUnavailable != nil
	status.OutboundDialObserved = session.serveDialObserved
	if !session.serveDeadline.IsZero() {
		deadline := session.serveDeadline
		status.ServeDeadline = &deadline
	}
	e.serveMu.Unlock()
	return status, true
}

// SourceUnavailable returns the bounded readiness failure for a playback. HLS
// uses this to stop probes and packagers as soon as the source HTTP request has
// established that a peerless partial torrent cannot serve startup.
func (e *Engine) SourceUnavailable(id string) error {
	e.mu.RLock()
	session, ok := e.sessions[id]
	e.mu.RUnlock()
	if !ok {
		return nil
	}
	e.serveMu.Lock()
	defer e.serveMu.Unlock()
	return session.serveUnavailable
}

func (e *Engine) ServeHTTP(w http.ResponseWriter, r *http.Request, id string) error {
	session, ok := e.beginStream(id, r.Method != http.MethodHead)
	if !ok {
		return errors.New("playback not found")
	}
	defer e.endStream(id)

	// HEAD never reads data, and a parked HLS packager resumes over its existing
	// source connection, so only body-producing requests need serve readiness.
	if r.Method != http.MethodHead {
		if err := e.ensureServeReadiness(r.Context(), session, requestReadStart(r, session.file.Length())); err != nil {
			return err
		}
	}

	reader := session.file.NewReader()
	defer reader.Close()
	reader.SetContext(r.Context())
	reader.SetResponsive()
	reader.SetReadahead(e.readaheadBytes)

	if mediaType := mime.TypeByExtension(strings.ToLower(filepath.Ext(session.FileName))); mediaType != "" {
		w.Header().Set("Content-Type", mediaType)
	}
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, filepath.Base(session.FileName), session.CreatedAt, reader)
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

// ensureServeReadiness makes sure a source read can actually make progress.
// Pieces of a locally present file can still be marked incomplete (for example
// boundary pieces shared with adjacent part files), so the read first re-hashes
// local data, then waits a bounded window for the swarm. Without this, a
// playback mounted on a dead swarm stalls silently inside the first read until
// the client's request times out with no log or error anywhere.
func (e *Engine) ensureServeReadiness(ctx context.Context, session *Session, readStart int64) error {
	deadline, unavailable := e.serveReadinessState(session)
	if unavailable != nil {
		return unavailable
	}
	if !e.readBlocked(session, readStart) {
		return nil
	}
	readinessContext, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	e.verifyFileLocalPieces(readinessContext, session)
	if !e.readBlocked(session, readStart) {
		return nil
	}

	// anacrolix only permits outgoing peer connections when needData is true.
	// Creating a File reader is not enough: its first Read marks the current and
	// readahead pieces wanted, which moves tracker peers out of the pending pool
	// and into outbound dials. Keep this demand reader alive until readiness is
	// established; the HTTP reader below then takes over the same range.
	demandContext, cancelDemand := context.WithCancel(readinessContext)
	demandReader := session.file.NewReader()
	demandReader.SetContext(demandContext)
	demandReader.SetResponsive()
	demandReader.SetReadahead(e.readaheadBytes)
	if _, err := demandReader.Seek(readStart, io.SeekStart); err != nil {
		cancelDemand()
		_ = demandReader.Close()
		return fmt.Errorf("seek playback source demand: %w", err)
	}
	initialStats := session.torrent.Stats()
	e.serveMu.Lock()
	session.serveInitialPending = max(session.serveInitialPending, initialStats.PendingPeers)
	e.serveMu.Unlock()
	demandDone := make(chan error, 1)
	go func() {
		_, err := demandReader.Read(make([]byte, 1))
		demandDone <- err
	}()
	defer func() {
		cancelDemand()
		<-demandDone
		_ = demandReader.Close()
	}()

	e.logger.Info("started playback source demand",
		"name", session.Name, "file", session.FileName, "read_start", readStart,
		"readahead_bytes", e.readaheadBytes, "pending_peers", initialStats.PendingPeers,
		"half_open_peers", initialStats.HalfOpenPeers, "serve_deadline", deadline)
	nextLog := time.Now().Add(serviceReadinessLogEvery)
	for {
		stats := session.torrent.Stats()
		e.observeServeDial(session, stats)
		if !e.readBlocked(session, readStart) {
			return nil
		}
		if err := readinessContext.Err(); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return e.markSourceUnavailable(session)
		}
		if time.Now().After(nextLog) {
			e.logger.Info("waiting for playback swarm to serve source",
				"name", session.Name, "file", session.FileName,
				"connected_peers", stats.ActivePeers, "connected_seeders", stats.ConnectedSeeders,
				"pending_peers", stats.PendingPeers, "half_open_peers", stats.HalfOpenPeers,
				"cached_percent", localCoveragePercent(session), "serve_deadline", deadline)
			nextLog = time.Now().Add(serviceReadinessLogEvery)
		}
		select {
		case <-readinessContext.Done():
		case <-time.After(serviceReadinessPoll):
		}
	}
}

func (e *Engine) observeServeDial(session *Session, stats torrent.TorrentStats) {
	e.serveMu.Lock()
	if stats.HalfOpenPeers > 0 ||
		session.serveInitialPending > 0 && stats.PendingPeers < session.serveInitialPending {
		session.serveDialObserved = true
	}
	e.serveMu.Unlock()
}

// serveReadinessState starts one deadline for the session. FFprobe and FFmpeg
// make several range requests during startup; giving every request a new wait
// allowed those requests to chain 15-second stalls for minutes.
func (e *Engine) serveReadinessState(session *Session) (time.Time, error) {
	e.serveMu.Lock()
	defer e.serveMu.Unlock()
	if session.serveDeadline.IsZero() {
		session.serveDeadline = time.Now().Add(e.serveWait)
	}
	return session.serveDeadline, session.serveUnavailable
}

func (e *Engine) markSourceUnavailable(session *Session) error {
	e.serveMu.Lock()
	if session.serveUnavailable != nil {
		err := session.serveUnavailable
		e.serveMu.Unlock()
		return err
	}
	stats := session.torrent.Stats()
	coverage := localCoveragePercent(session)
	err := fmt.Errorf("%w: no peers connected before the shared %s startup deadline and only %d%% of %s is available locally",
		ErrSourceUnavailable, e.serveWait, coverage, session.FileName)
	session.serveUnavailable = err
	deadline := session.serveDeadline
	dialObserved := session.serveDialObserved
	e.serveMu.Unlock()

	discoveryOutcome := "peers_discovered_but_not_connected"
	if stats.TotalPeers == 0 && stats.PendingPeers == 0 && stats.HalfOpenPeers == 0 {
		if dialObserved {
			discoveryOutcome = "peers_dialed_but_not_connected"
		} else {
			discoveryOutcome = "no_peers_returned_or_retained"
		}
	}
	e.logger.Warn("playback swarm cannot serve source",
		"name", session.Name, "file", session.FileName,
		"info_hash", session.torrent.InfoHash().HexString(),
		"connected_peers", stats.ActivePeers, "connected_seeders", stats.ConnectedSeeders,
		"total_peers", stats.TotalPeers, "pending_peers", stats.PendingPeers,
		"half_open_peers", stats.HalfOpenPeers, "cached_percent", coverage,
		"serve_deadline", deadline, "outbound_dial_observed", dialObserved,
		"peer_discovery_outcome", discoveryOutcome,
		"tracker_outcome", "inferred_from_peer_counters")
	return err
}

// readBlocked reports whether a read starting at readStart would have to wait
// for the network: nothing is connected or dialing, and the readahead window is
// not fully covered by complete local pieces.
func (e *Engine) readBlocked(session *Session, readStart int64) bool {
	t := session.torrent
	stats := t.Stats()
	if stats.ActivePeers > 0 {
		return false
	}
	file := session.file
	windowEnd := min(file.Length()-1, readStart+e.readaheadBytes)
	return !fileRangeComplete(t, file, readStart, windowEnd)
}

// verifyFileLocalPieces re-hashes the playback file's incomplete pieces so
// locally present data becomes servable without peers. Data can be on disk
// while pieces are marked incomplete, and the in-memory completion state does
// not survive a restart, so this is the only way a disconnected replay can
// serve. Verification is bounded; pieces beyond the budget stay incomplete.
func (e *Engine) verifyFileLocalPieces(ctx context.Context, session *Session) {
	t := session.torrent
	file := session.file
	if !fileDataPresent(e.dataDir, file) {
		return
	}
	pieceLength := t.Info().PieceLength
	begin := int(file.Offset() / pieceLength)
	end := int((file.Offset() + file.Length() + pieceLength - 1) / pieceLength)
	if maxPieces := int(t.NumPieces()); end > maxPieces {
		end = maxPieces
	}
	missingPieces := 0
	for piece := begin; piece < end; piece++ {
		if t.PieceBytesMissing(piece) > 0 {
			missingPieces++
		}
	}
	if missingPieces == 0 {
		return
	}
	verifyContext, cancel := context.WithTimeout(ctx, localVerifyBudget)
	defer cancel()
	verifiedPieces := 0
	var verifiedBytes int64
	for piece := begin; piece < end; piece++ {
		if verifyContext.Err() != nil {
			break
		}
		if t.PieceBytesMissing(piece) <= 0 {
			continue
		}
		pieceLength := t.Piece(piece).Info().Length()
		if err := t.Piece(piece).VerifyDataContext(verifyContext); err != nil {
			e.logger.Warn("verify local torrent data", "name", t.Name(), "piece", piece, "error", err)
			break
		}
		if t.PieceBytesMissing(piece) <= 0 {
			verifiedPieces++
			verifiedBytes += pieceLength
		}
	}
	remainingPieces := 0
	for piece := begin; piece < end; piece++ {
		if t.PieceBytesMissing(piece) > 0 {
			remainingPieces++
		}
	}
	if verifiedPieces == 0 && remainingPieces == missingPieces {
		return
	}
	logArgs := []any{"name", t.Name(), "file", file.DisplayPath(),
		"incomplete_pieces", missingPieces, "verified_pieces", verifiedPieces,
		"verified_bytes", verifiedBytes, "remaining_incomplete_pieces", remainingPieces}
	if remainingPieces > 0 {
		e.logger.Warn("checked local torrent data for playback", logArgs...)
	} else {
		e.logger.Info("recovered playback data from local pieces", logArgs...)
	}
}

// fileDataPresent reports whether any local bytes exist for the file, so that
// re-hashing its incomplete pieces has a chance to recover them.
func fileDataPresent(dataDir string, file *torrent.File) bool {
	for _, suffix := range []string{"", ".part"} {
		path := filepath.Join(dataDir, filepath.FromSlash(file.Path())) + suffix
		if info, err := os.Stat(path); err == nil && info.Size() > 0 {
			return true
		}
	}
	return false
}

// fileRangeComplete reports whether every piece overlapping the file-relative
// byte range [start, end] is complete, so reads there are served from local data.
func fileRangeComplete(t *torrent.Torrent, file *torrent.File, start, end int64) bool {
	pieceLength := t.Info().PieceLength
	firstPiece := int((file.Offset() + start) / pieceLength)
	lastPiece := int((file.Offset() + end) / pieceLength)
	if maxPiece := int(t.NumPieces()) - 1; lastPiece > maxPiece {
		lastPiece = maxPiece
	}
	for piece := firstPiece; piece <= lastPiece; piece++ {
		if t.PieceBytesMissing(piece) > 0 {
			return false
		}
	}
	return true
}

func localCoveragePercent(session *Session) int {
	if session.FileSize <= 0 {
		return 0
	}
	return int(100 * session.file.BytesCompleted() / session.FileSize)
}

func (e *Engine) beginStream(id string, markStarted bool) (*Session, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	session, ok := e.sessions[id]
	if !ok {
		return nil, false
	}
	now := time.Now().UTC()
	session.activeStreams++
	if markStarted {
		session.started = true
		e.markManagedTorrentStartedLocked(session.torrent, now)
	}
	session.lastActivity = now
	e.updateManagedTorrentActivityLocked(session.torrent, now)
	if markStarted {
		if err := e.persistManagedTorrentsLocked(); err != nil {
			e.logger.Warn("could not save managed torrent state", "error", err)
		}
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
		e.updateManagedTorrentActivityLocked(session.torrent, now)
	}
}

func (e *Engine) runJanitor(ctx context.Context) {
	defer e.cleanupWG.Done()
	ticker := time.NewTicker(e.cleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			e.cleanup(now.UTC())
		}
	}
}

type torrentGroup struct {
	torrent      *torrent.Torrent
	sessions     []*Session
	lastActivity time.Time
	active       int
	started      bool
	complete     int64
	downloaded   int64
	uploaded     int64
	ratio        float64
	ratioMet     bool
	seededFor    time.Duration
}

func (e *Engine) cleanup(now time.Time) {
	e.lifecycleMu.Lock()
	defer e.lifecycleMu.Unlock()

	e.mu.Lock()
	groupsByTorrent := make(map[*torrent.Torrent]*torrentGroup)
	for _, session := range e.sessions {
		group := groupsByTorrent[session.torrent]
		if group == nil {
			state := e.accountManagedTorrentLocked(session.torrent, now)
			downloaded, uploaded, seededFor := e.managedTransferTotalsLocked(session.torrent, now)
			ratio := transferRatio(downloaded, uploaded)
			group = &torrentGroup{
				torrent: session.torrent, complete: session.torrent.BytesCompleted(),
				downloaded: downloaded, uploaded: uploaded, ratio: ratio,
				ratioMet: ratioTargetMet(downloaded, ratio, e.seedRatioTarget), seededFor: seededFor,
			}
			if state != nil {
				group.started = state.Started
			}
			groupsByTorrent[session.torrent] = group
		}
		group.sessions = append(group.sessions, session)
		group.active += session.activeStreams
		group.started = group.started || session.started
		if session.lastActivity.After(group.lastActivity) {
			group.lastActivity = session.lastActivity
		}
	}

	groups := make([]*torrentGroup, 0, len(groupsByTorrent))
	var totalComplete int64
	for _, group := range groupsByTorrent {
		groups = append(groups, group)
		totalComplete += group.complete
	}
	selected := make(map[*torrentGroup]string)
	for _, group := range groups {
		if group.active > 0 {
			continue
		}
		idle := now.Sub(group.lastActivity)
		switch {
		case !group.started && idle >= e.idleGrace:
			selected[group] = "unused"
		case group.started && idle >= e.idleGrace && group.ratioMet:
			selected[group] = "ratio-target"
		case group.started && idle >= e.idleGrace && group.seededFor >= e.seedMaxAge:
			selected[group] = "seed-time-target"
		}
	}
	for group := range selected {
		totalComplete -= group.complete
	}
	remainingGroups := len(groups) - len(selected)
	if remainingGroups > e.maxSeedSessions {
		candidates := cleanupCandidates(groups, selected, now, e.idleGrace, e.seedMaxAge)
		for _, group := range candidates {
			if remainingGroups <= e.maxSeedSessions {
				break
			}
			selected[group] = "session-limit"
			totalComplete -= group.complete
			remainingGroups--
		}
	}
	if totalComplete > e.cacheLimitBytes {
		for _, group := range cleanupCandidates(groups, selected, now, e.idleGrace, e.seedMaxAge) {
			if totalComplete <= e.cacheLimitBytes {
				break
			}
			selected[group] = "cache-limit"
			totalComplete -= group.complete
		}
	}

	type cleanupItem struct {
		group  *torrentGroup
		reason string
		ids    []string
	}
	items := make([]cleanupItem, 0, len(selected))
	for group, reason := range selected {
		item := cleanupItem{group: group, reason: reason}
		for _, session := range group.sessions {
			delete(e.sessions, session.ID)
			item.ids = append(item.ids, session.ID)
		}
		e.removeManagedTorrentLocked(group.torrent)
		items = append(items, item)
	}
	if err := e.persistManagedTorrentsLocked(); err != nil {
		e.logger.Warn("could not save managed torrent state", "error", err)
	}
	handler := e.onCleanup
	e.mu.Unlock()

	for _, item := range items {
		paths := e.torrentDataPaths(item.group.torrent)
		item.group.torrent.Drop()
		if err := e.removeTorrentData(paths); err != nil {
			e.logger.Warn("could not fully remove torrent cache", "reason", item.reason, "error", err)
		}
		e.logger.Info("retired torrent session",
			"reason", item.reason,
			"stored_bytes", item.group.complete,
			"downloaded_bytes", item.group.downloaded,
			"uploaded_bytes", item.group.uploaded,
			"ratio", item.group.ratio,
		)
		if handler != nil {
			for _, id := range item.ids {
				handler(id, item.reason)
			}
		}
	}
}

func cleanupCandidates(
	groups []*torrentGroup,
	selected map[*torrentGroup]string,
	now time.Time,
	idleGrace time.Duration,
	seedMaxAge time.Duration,
) []*torrentGroup {
	var candidates []*torrentGroup
	for _, group := range groups {
		if _, alreadySelected := selected[group]; alreadySelected || group.active > 0 {
			continue
		}
		if group.started && !group.ratioMet && group.seededFor < seedMaxAge {
			continue
		}
		if now.Sub(group.lastActivity) < idleGrace {
			continue
		}
		candidates = append(candidates, group)
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].lastActivity.Before(candidates[j].lastActivity)
	})
	return candidates
}

func (e *Engine) torrentDataPaths(t *torrent.Torrent) []string {
	paths := make([]string, 0, len(t.Files()))
	for _, file := range t.Files() {
		paths = append(paths, file.Path())
	}
	return paths
}

func (e *Engine) removeTorrentData(paths []string) error {
	var firstErr error
	for _, torrentPath := range paths {
		relative := filepath.Clean(filepath.FromSlash(torrentPath))
		if relative == "." || filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			if firstErr == nil {
				firstErr = fmt.Errorf("unsafe torrent path %q", torrentPath)
			}
			continue
		}
		path := filepath.Join(e.dataDir, relative)
		for _, candidate := range []string{path, path + ".part"} {
			if err := os.Remove(candidate); err != nil && !errors.Is(err, os.ErrNotExist) && firstErr == nil {
				firstErr = err
			}
		}
		removeEmptyParents(filepath.Dir(path), e.dataDir)
	}
	return firstErr
}

func removeEmptyParents(path, stop string) {
	for path != stop && path != "." && path != string(filepath.Separator) {
		if err := os.Remove(path); err != nil {
			return
		}
		path = filepath.Dir(path)
	}
}

func transferRatio(downloaded, uploaded int64) float64 {
	if downloaded <= 0 {
		return 0
	}
	return float64(uploaded) / float64(downloaded)
}

func ratioTargetMet(downloaded int64, ratio, target float64) bool {
	return target <= 0 || downloaded > 0 && ratio >= target
}

func (e *Engine) addTorrent(ctx context.Context, source Source) (*torrent.Torrent, error) {
	switch {
	case source.MagnetURI != "":
		t, err := e.client.AddMagnet(source.MagnetURI)
		if err != nil {
			return nil, fmt.Errorf("add magnet: %w", err)
		}
		return t, nil
	case source.TorrentPath != "":
		meta, err := metainfo.LoadFromFile(source.TorrentPath)
		if err != nil {
			return nil, fmt.Errorf("load torrent file: %w", err)
		}
		t, err := e.client.AddTorrent(meta)
		if err != nil {
			return nil, fmt.Errorf("add torrent file: %w", err)
		}
		return t, nil
	default:
		meta, err := e.downloadMetainfo(ctx, source.TorrentURL)
		if err != nil {
			return nil, err
		}
		t, err := e.client.AddTorrent(meta)
		if err != nil {
			return nil, fmt.Errorf("add torrent URL: %w", err)
		}
		return t, nil
	}
}

func (e *Engine) downloadMetainfo(ctx context.Context, torrentURL string) (*metainfo.MetaInfo, error) {
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, torrentURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", "filmstream/0.1")
		response, err := e.httpClient.Do(req)
		if err == nil {
			if response.StatusCode >= 200 && response.StatusCode < 300 {
				meta, parseErr := metainfo.Load(io.LimitReader(response.Body, maxMetainfoBytes))
				response.Body.Close()
				if parseErr != nil {
					return nil, fmt.Errorf("parse torrent file: %w", parseErr)
				}
				return meta, nil
			}
			lastErr = fmt.Errorf("download torrent file returned %s", response.Status)
			response.Body.Close()
			if response.StatusCode < 500 {
				break
			}
		} else {
			var urlError *url.Error
			if errors.As(err, &urlError) {
				lastErr = urlError.Err
			} else {
				lastErr = err
			}
		}
		if attempt < 3 {
			select {
			case <-ctx.Done():
				return nil, fmt.Errorf("download torrent file: %w", ctx.Err())
			case <-time.After(time.Duration(attempt) * time.Second):
			}
		}
	}
	return nil, fmt.Errorf("download torrent file: %w", lastErr)
}

func selectVideoFile(files []*torrent.File, fileHint string) (*torrent.File, error) {
	var videos []*torrent.File
	for _, file := range files {
		if videoExtensions[strings.ToLower(filepath.Ext(file.Path()))] {
			videos = append(videos, file)
		}
	}
	if len(videos) == 0 {
		return nil, errors.New("torrent contains no supported video files")
	}
	if strings.TrimSpace(fileHint) != "" && len(videos) > 1 {
		matching := matchingVideoFiles(videos, fileHint)
		if len(matching) == 0 {
			return nil, fmt.Errorf("torrent contains no video matching %s", fileHint)
		}
		videos = matching
	}
	sort.SliceStable(videos, func(i, j int) bool {
		return videos[i].Length() > videos[j].Length()
	})
	return videos[0], nil
}

func matchingVideoFiles(files []*torrent.File, hint string) []*torrent.File {
	hint = normalizeFileHint(hint)
	if hint == "" {
		return nil
	}
	var matching []*torrent.File
	for _, file := range files {
		name := normalizeFileHint(file.Path())
		if strings.Contains(name, hint) || matchesAlternateEpisodeCode(name, hint) {
			matching = append(matching, file)
		}
	}
	return matching
}

func normalizeFileHint(value string) string {
	var builder strings.Builder
	for _, character := range strings.ToLower(value) {
		if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' {
			builder.WriteRune(character)
		}
	}
	return builder.String()
}

func matchesAlternateEpisodeCode(name, hint string) bool {
	if len(hint) < 6 || hint[0] != 's' {
		return false
	}
	episodeIndex := strings.IndexByte(hint, 'e')
	if episodeIndex < 2 || episodeIndex == len(hint)-1 {
		return false
	}
	seasonRaw := hint[1:episodeIndex]
	episodeRaw := hint[episodeIndex+1:]
	season := strings.TrimLeft(seasonRaw, "0")
	episode := strings.TrimLeft(episodeRaw, "0")
	if season == "" {
		season = "0"
	}
	if episode == "" {
		episode = "0"
	}
	return strings.Contains(name, season+"x"+episode) ||
		strings.Contains(name, season+"x"+episodeRaw) ||
		strings.Contains(name, seasonRaw+"x"+episode) ||
		strings.Contains(name, seasonRaw+"x"+episodeRaw)
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
