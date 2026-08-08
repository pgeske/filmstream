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
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
)

const maxMetainfoBytes = 16 << 20

type Config struct {
	DataDir         string
	MaxTorrentBytes int64
	ReadaheadBytes  int64
	MetadataTimeout time.Duration
	SeedRatioTarget float64
}

type Source struct {
	MagnetURI   string
	TorrentURL  string
	TorrentPath string
}

type Engine struct {
	client          *torrent.Client
	httpClient      *http.Client
	maxTorrentBytes int64
	readaheadBytes  int64
	metadataTimeout time.Duration
	seedRatioTarget float64

	mu       sync.RWMutex
	sessions map[string]*Session
}

type Session struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	FileName  string    `json:"file_name"`
	FileSize  int64     `json:"file_size"`
	CreatedAt time.Time `json:"created_at"`

	torrent *torrent.Torrent
	file    *torrent.File
}

type Status struct {
	ID               string  `json:"id"`
	Name             string  `json:"name"`
	FileName         string  `json:"file_name"`
	FileSize         int64   `json:"file_size"`
	BytesComplete    int64   `json:"bytes_complete"`
	TorrentBytes     int64   `json:"torrent_bytes"`
	TorrentComplete  int64   `json:"torrent_complete"`
	DownloadedBytes  int64   `json:"downloaded_bytes"`
	UploadedBytes    int64   `json:"uploaded_bytes"`
	Ratio            float64 `json:"ratio"`
	RatioTarget      float64 `json:"ratio_target"`
	RatioTargetMet   bool    `json:"ratio_target_met"`
	ActivePeers      int     `json:"active_peers"`
	ConnectedSeeders int     `json:"connected_seeders"`
}

func New(cfg Config) (*Engine, error) {
	if err := os.MkdirAll(filepath.Join(cfg.DataDir, "torrents"), 0o755); err != nil {
		return nil, fmt.Errorf("create torrent data directory: %w", err)
	}
	clientConfig := torrent.NewDefaultClientConfig()
	clientConfig.DataDir = filepath.Join(cfg.DataDir, "torrents")
	clientConfig.ListenPort = 0
	clientConfig.Seed = true
	clientConfig.NoUpload = false
	clientConfig.Slogger = slog.New(slog.NewTextHandler(io.Discard, nil))

	client, err := torrent.NewClient(clientConfig)
	if err != nil {
		return nil, fmt.Errorf("create torrent client: %w", err)
	}
	httpTransport := http.DefaultTransport.(*http.Transport).Clone()
	httpTransport.TLSHandshakeTimeout = 30 * time.Second
	return &Engine{
		client:          client,
		httpClient:      &http.Client{Timeout: cfg.MetadataTimeout, Transport: httpTransport},
		maxTorrentBytes: cfg.MaxTorrentBytes,
		readaheadBytes:  cfg.ReadaheadBytes,
		metadataTimeout: cfg.MetadataTimeout,
		seedRatioTarget: cfg.SeedRatioTarget,
		sessions:        make(map[string]*Session),
	}, nil
}

func (e *Engine) Close() error {
	e.client.Close()
	return nil
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
	file.Download()

	id, err := randomID()
	if err != nil {
		t.Drop()
		return nil, err
	}
	session := &Session{
		ID:        id,
		Name:      t.Name(),
		FileName:  file.DisplayPath(),
		FileSize:  file.Length(),
		CreatedAt: time.Now().UTC(),
		torrent:   t,
		file:      file,
	}
	e.mu.Lock()
	e.sessions[id] = session
	e.mu.Unlock()

	go e.prefetchTail(session)
	return session, nil
}

func (e *Engine) Get(id string) (*Session, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	session, ok := e.sessions[id]
	return session, ok
}

func (e *Engine) Status(id string) (Status, bool) {
	session, ok := e.Get(id)
	if !ok {
		return Status{}, false
	}
	stats := session.torrent.Stats()
	downloaded := stats.BytesReadData.Int64()
	uploaded := stats.BytesWrittenData.Int64()
	ratio := 0.0
	if downloaded > 0 {
		ratio = float64(uploaded) / float64(downloaded)
	}
	return Status{
		ID:               session.ID,
		Name:             session.Name,
		FileName:         session.FileName,
		FileSize:         session.FileSize,
		BytesComplete:    session.file.BytesCompleted(),
		TorrentBytes:     session.torrent.Length(),
		TorrentComplete:  session.torrent.BytesCompleted(),
		DownloadedBytes:  downloaded,
		UploadedBytes:    uploaded,
		Ratio:            ratio,
		RatioTarget:      e.seedRatioTarget,
		RatioTargetMet:   downloaded > 0 && ratio >= e.seedRatioTarget,
		ActivePeers:      stats.ActivePeers,
		ConnectedSeeders: stats.ConnectedSeeders,
	}, true
}

func (e *Engine) ServeHTTP(w http.ResponseWriter, r *http.Request, id string) error {
	session, ok := e.Get(id)
	if !ok {
		return errors.New("playback not found")
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

func (e *Engine) prefetchTail(session *Session) {
	const tailBytes = int64(4 << 20)
	start := session.FileSize - tailBytes
	if start <= 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
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
