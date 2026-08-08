package torrentstream

import (
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
	defaultSeedMaxAge      = 24 * time.Hour
	defaultCleanupInterval = 30 * time.Second
)

type Config struct {
	DataDir         string
	MaxTorrentBytes int64
	ReadaheadBytes  int64
	MetadataTimeout time.Duration
	SeedRatioTarget float64
	CacheLimitBytes int64
	MaxSeedSessions int
	IdleGrace       time.Duration
	SeedMaxAge      time.Duration
	CleanupInterval time.Duration
	CleanOnStart    bool
	CleanOnClose    bool
	Logger          *slog.Logger
}

type Source struct {
	MagnetURI   string
	TorrentURL  string
	TorrentPath string
}

type Engine struct {
	client          *torrent.Client
	httpClient      *http.Client
	dataDir         string
	maxTorrentBytes int64
	readaheadBytes  int64
	metadataTimeout time.Duration
	seedRatioTarget float64
	cacheLimitBytes int64
	maxSeedSessions int
	idleGrace       time.Duration
	seedMaxAge      time.Duration
	cleanupInterval time.Duration
	cleanOnClose    bool
	logger          *slog.Logger
	lockFile        *os.File

	lifecycleMu sync.Mutex
	mu          sync.RWMutex
	sessions    map[string]*Session
	onCleanup   func(string, string)

	backgroundCtx context.Context
	cleanupCancel context.CancelFunc
	cleanupWG     sync.WaitGroup
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
}

type Status struct {
	ID               string     `json:"id"`
	Name             string     `json:"name"`
	FileName         string     `json:"file_name"`
	FileSize         int64      `json:"file_size"`
	State            string     `json:"state"`
	ActiveStreams    int        `json:"active_streams"`
	LastActivity     time.Time  `json:"last_activity"`
	SeedDeadline     *time.Time `json:"seed_deadline,omitempty"`
	BytesComplete    int64      `json:"bytes_complete"`
	TorrentBytes     int64      `json:"torrent_bytes"`
	TorrentComplete  int64      `json:"torrent_complete"`
	DownloadedBytes  int64      `json:"downloaded_bytes"`
	UploadedBytes    int64      `json:"uploaded_bytes"`
	Ratio            float64    `json:"ratio"`
	RatioTarget      float64    `json:"ratio_target"`
	RatioTargetMet   bool       `json:"ratio_target_met"`
	ActivePeers      int        `json:"active_peers"`
	ConnectedSeeders int        `json:"connected_seeders"`
}

func New(cfg Config) (*Engine, error) {
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
	if cfg.CleanOnStart {
		if err := os.RemoveAll(torrentDataDir); err != nil {
			unlock()
			return nil, fmt.Errorf("clear stale torrent cache: %w", err)
		}
	}
	if err := os.MkdirAll(torrentDataDir, 0o755); err != nil {
		unlock()
		return nil, fmt.Errorf("create torrent data directory: %w", err)
	}
	clientConfig := torrent.NewDefaultClientConfig()
	clientConfig.DataDir = torrentDataDir
	clientConfig.ListenPort = 0
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
		client:          client,
		httpClient:      &http.Client{Timeout: cfg.MetadataTimeout, Transport: httpTransport},
		dataDir:         torrentDataDir,
		maxTorrentBytes: cfg.MaxTorrentBytes,
		readaheadBytes:  cfg.ReadaheadBytes,
		metadataTimeout: cfg.MetadataTimeout,
		seedRatioTarget: cfg.SeedRatioTarget,
		cacheLimitBytes: cfg.CacheLimitBytes,
		maxSeedSessions: cfg.MaxSeedSessions,
		idleGrace:       cfg.IdleGrace,
		seedMaxAge:      cfg.SeedMaxAge,
		cleanupInterval: cfg.CleanupInterval,
		cleanOnClose:    cfg.CleanOnClose,
		logger:          cfg.Logger,
		lockFile:        lockFile,
		sessions:        make(map[string]*Session),
	}
	ctx, cancel := context.WithCancel(context.Background())
	engine.backgroundCtx = ctx
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

func (e *Engine) Close() error {
	e.closeOnce.Do(func() {
		e.cleanupCancel()
		e.cleanupWG.Wait()
		e.lifecycleMu.Lock()
		e.client.Close()
		if e.cleanOnClose {
			if err := os.RemoveAll(e.dataDir); err != nil {
				e.logger.Warn("could not clear torrent cache during shutdown", "error", err)
			}
		}
		e.lifecycleMu.Unlock()
		_ = syscall.Flock(int(e.lockFile.Fd()), syscall.LOCK_UN)
		_ = e.lockFile.Close()
	})
	return nil
}

func (e *Engine) SetCleanupHandler(handler func(string, string)) {
	e.mu.Lock()
	e.onCleanup = handler
	e.mu.Unlock()
}

func (e *Engine) Create(ctx context.Context, source Source) (*Session, error) {
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

	e.lifecycleMu.Lock()
	defer e.lifecycleMu.Unlock()
	t, err := e.addTorrent(ctx, source)
	if err != nil {
		return nil, err
	}
	metadataContext, cancel := context.WithTimeout(ctx, e.metadataTimeout)
	defer cancel()
	select {
	case <-t.GotInfo():
	case <-metadataContext.Done():
		t.Drop()
		return nil, fmt.Errorf("wait for torrent metadata: %w", metadataContext.Err())
	}

	if length := t.Length(); length <= 0 {
		t.Drop()
		return nil, errors.New("torrent contains no data")
	} else if e.maxTorrentBytes > 0 && length > e.maxTorrentBytes {
		t.Drop()
		return nil, fmt.Errorf("torrent is %.1f GiB; configured maximum is %.1f GiB", gib(length), gib(e.maxTorrentBytes))
	}

	file, err := selectVideoFile(t.Files())
	if err != nil {
		t.Drop()
		return nil, err
	}
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
	e.sessions[id] = session
	e.mu.Unlock()

	// Readers request only the playback window. There is deliberately no File.Download call:
	// closing the player must not turn a quick sample into a full background download.
	e.cleanupWG.Add(1)
	go func() {
		defer e.cleanupWG.Done()
		e.prefetchTail(e.backgroundCtx, session)
	}()
	return session, nil
}

func (e *Engine) Get(id string) (*Session, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	session, ok := e.sessions[id]
	return session, ok
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
	if started && activeStreams == 0 {
		deadline := lastActivity.Add(e.seedMaxAge)
		status.SeedDeadline = &deadline
	}
	stats := t.Stats()
	downloaded := stats.BytesReadData.Int64()
	uploaded := stats.BytesWrittenData.Int64()
	ratio := transferRatio(downloaded, uploaded)
	status.BytesComplete = file.BytesCompleted()
	status.TorrentBytes = t.Length()
	status.TorrentComplete = t.BytesCompleted()
	status.DownloadedBytes = downloaded
	status.UploadedBytes = uploaded
	status.Ratio = ratio
	status.RatioTarget = e.seedRatioTarget
	status.RatioTargetMet = ratioTargetMet(downloaded, ratio, e.seedRatioTarget)
	status.ActivePeers = stats.ActivePeers
	status.ConnectedSeeders = stats.ConnectedSeeders
	return status, true
}

func (e *Engine) ServeHTTP(w http.ResponseWriter, r *http.Request, id string) error {
	session, ok := e.beginStream(id, r.Method != http.MethodHead)
	if !ok {
		return errors.New("playback not found")
	}
	defer e.endStream(id)

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

func (e *Engine) beginStream(id string, markStarted bool) (*Session, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	session, ok := e.sessions[id]
	if !ok {
		return nil, false
	}
	session.activeStreams++
	if markStarted {
		session.started = true
	}
	session.lastActivity = time.Now().UTC()
	return session, true
}

func (e *Engine) endStream(id string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if session, ok := e.sessions[id]; ok {
		if session.activeStreams > 0 {
			session.activeStreams--
		}
		session.lastActivity = time.Now().UTC()
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
}

func (e *Engine) cleanup(now time.Time) {
	e.lifecycleMu.Lock()
	defer e.lifecycleMu.Unlock()

	e.mu.Lock()
	groupsByTorrent := make(map[*torrent.Torrent]*torrentGroup)
	for _, session := range e.sessions {
		group := groupsByTorrent[session.torrent]
		if group == nil {
			stats := session.torrent.Stats()
			downloaded := stats.BytesReadData.Int64()
			uploaded := stats.BytesWrittenData.Int64()
			ratio := transferRatio(downloaded, uploaded)
			group = &torrentGroup{
				torrent: session.torrent, complete: session.torrent.BytesCompleted(),
				downloaded: downloaded, uploaded: uploaded, ratio: ratio,
				ratioMet: ratioTargetMet(downloaded, ratio, e.seedRatioTarget),
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
		case group.started && idle >= e.seedMaxAge:
			selected[group] = "seed-time-limit"
		}
	}
	for group := range selected {
		totalComplete -= group.complete
	}
	remainingGroups := len(groups) - len(selected)
	if remainingGroups > e.maxSeedSessions {
		candidates := cleanupCandidates(groups, selected, now, e.idleGrace)
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
		for _, group := range cleanupCandidates(groups, selected, now, e.idleGrace) {
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
		items = append(items, item)
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

func cleanupCandidates(groups []*torrentGroup, selected map[*torrentGroup]string, now time.Time, idleGrace time.Duration) []*torrentGroup {
	var candidates []*torrentGroup
	for _, group := range groups {
		if _, alreadySelected := selected[group]; alreadySelected || group.active > 0 {
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

func (e *Engine) prefetchTail(parent context.Context, session *Session) {
	const tailBytes = int64(4 << 20)
	start := session.FileSize - tailBytes
	if start <= 0 {
		return
	}
	ctx, cancel := context.WithTimeout(parent, 45*time.Second)
	defer cancel()
	reader := session.file.NewReader()
	defer reader.Close()
	reader.SetContext(ctx)
	reader.SetResponsive()
	reader.SetReadahead(tailBytes)
	if _, err := reader.Seek(start, io.SeekStart); err != nil {
		return
	}
	_, _ = io.CopyN(io.Discard, reader, tailBytes)
}

func selectVideoFile(files []*torrent.File) (*torrent.File, error) {
	videoExtensions := map[string]bool{
		".avi": true, ".m4v": true, ".mkv": true, ".mov": true,
		".mp4": true, ".mpeg": true, ".mpg": true, ".ts": true, ".webm": true,
	}
	var videos []*torrent.File
	for _, file := range files {
		if videoExtensions[strings.ToLower(filepath.Ext(file.Path()))] {
			videos = append(videos, file)
		}
	}
	if len(videos) == 0 {
		return nil, errors.New("torrent contains no supported video files")
	}
	sort.SliceStable(videos, func(i, j int) bool {
		return videos[i].Length() > videos[j].Length()
	})
	return videos[0], nil
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
