package usenetstream

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pgeske/filmstream/internal/config"
)

const (
	maxNZBBytes            = 32 << 20
	defaultStartupTimeout  = 2 * time.Minute
	defaultIdleGrace       = 2 * time.Minute
	defaultCleanupInterval = 30 * time.Second
	videoReadyTimeout      = 5 * time.Second
	maxWebDAVDepth         = 4
)

var errNoSupportedVideo = errors.New("Usenet release contains no supported video files")

type Config struct {
	BaseURL         string
	APIKey          string
	WebDAVUser      string
	WebDAVPassword  string
	Category        string
	StartupTimeout  time.Duration
	IdleGrace       time.Duration
	CleanupInterval time.Duration
	CleanOnClose    bool
	Logger          *slog.Logger
	ControlClient   *http.Client
	StreamClient    *http.Client
}

type Source struct {
	NZBURL   string
	NZBPath  string
	Name     string
	FileHint string
}

type Engine struct {
	baseURL         *url.URL
	apiKey          string
	webDAVUser      string
	webDAVPassword  string
	category        string
	startupTimeout  time.Duration
	idleGrace       time.Duration
	cleanupInterval time.Duration
	cleanOnClose    bool
	logger          *slog.Logger
	controlClient   *http.Client
	streamClient    *http.Client

	mu        sync.RWMutex
	sessions  map[string]*Session
	onCleanup func(string, string)

	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	closeOnce sync.Once
}

type Session struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	FileName  string    `json:"file_name"`
	FileSize  int64     `json:"file_size"`
	CreatedAt time.Time `json:"created_at"`

	nzoID         string
	upstreamURL   string
	nzb           []byte
	activeStreams int
	lastActivity  time.Time
	bytesRead     int64
}

type Status struct {
	ID              string    `json:"id"`
	Name            string    `json:"name"`
	FileName        string    `json:"file_name"`
	FileSize        int64     `json:"file_size"`
	Source          string    `json:"source"`
	State           string    `json:"state"`
	ActiveStreams   int       `json:"active_streams"`
	LastActivity    time.Time `json:"last_activity"`
	DownloadedBytes int64     `json:"downloaded_bytes"`
}

func FromConfig(cfg config.Usenet, idleGrace time.Duration, logger *slog.Logger) (*Engine, error) {
	if cfg.Provider == "" {
		return nil, nil
	}
	if cfg.Provider != "infinidysk" {
		return nil, fmt.Errorf("unsupported Usenet provider %q", cfg.Provider)
	}
	apiKey, err := configuredSecret(cfg.APIKeyEnv, cfg.APIKeyFile, "Usenet API key")
	if err != nil {
		return nil, err
	}
	password, err := configuredSecret(cfg.WebDAVPasswordEnv, cfg.WebDAVPasswordFile, "Usenet WebDAV password")
	if err != nil {
		return nil, err
	}
	startupTimeout := time.Duration(cfg.StartupTimeoutSeconds) * time.Second
	return New(Config{
		BaseURL: cfg.BaseURL, APIKey: apiKey,
		WebDAVUser: cfg.WebDAVUser, WebDAVPassword: password,
		Category: cfg.Category, StartupTimeout: startupTimeout,
		IdleGrace: idleGrace, CleanOnClose: true, Logger: logger,
	})
}

func configuredSecret(envName, fileName, label string) (string, error) {
	value := ""
	if envName != "" {
		value = strings.TrimSpace(os.Getenv(envName))
	}
	if value == "" && fileName != "" {
		contents, err := os.ReadFile(fileName)
		if err != nil {
			return "", fmt.Errorf("read %s: %w", strings.ToLower(label), err)
		}
		value = strings.TrimSpace(string(contents))
	}
	if value == "" {
		return "", fmt.Errorf("%s is not configured", label)
	}
	return value, nil
}

func New(cfg Config) (*Engine, error) {
	parsed, err := url.Parse(cfg.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse Usenet base URL: %w", err)
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, errors.New("Usenet base URL must be an absolute HTTP or HTTPS URL")
	}
	if cfg.APIKey == "" || cfg.WebDAVUser == "" || cfg.WebDAVPassword == "" {
		return nil, errors.New("Usenet API and WebDAV credentials are required")
	}
	if cfg.Category == "" {
		cfg.Category = "movies"
	}
	if cfg.StartupTimeout <= 0 {
		cfg.StartupTimeout = defaultStartupTimeout
	}
	if cfg.IdleGrace <= 0 {
		cfg.IdleGrace = defaultIdleGrace
	}
	if cfg.CleanupInterval <= 0 {
		cfg.CleanupInterval = defaultCleanupInterval
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if cfg.ControlClient == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.ResponseHeaderTimeout = 30 * time.Second
		cfg.ControlClient = &http.Client{Transport: transport}
	}
	if cfg.StreamClient == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.MaxIdleConnsPerHost = 16
		transport.ResponseHeaderTimeout = 30 * time.Second
		cfg.StreamClient = &http.Client{Transport: transport}
	}

	ctx, cancel := context.WithCancel(context.Background())
	engine := &Engine{
		baseURL: parsed, apiKey: cfg.APIKey,
		webDAVUser: cfg.WebDAVUser, webDAVPassword: cfg.WebDAVPassword,
		category: cfg.Category, startupTimeout: cfg.StartupTimeout,
		idleGrace: cfg.IdleGrace, cleanupInterval: cfg.CleanupInterval,
		cleanOnClose: cfg.CleanOnClose, logger: cfg.Logger,
		controlClient: cfg.ControlClient, streamClient: cfg.StreamClient,
		sessions: make(map[string]*Session), ctx: ctx, cancel: cancel,
	}
	engine.wg.Add(1)
	go engine.runJanitor()
	return engine, nil
}

func (e *Engine) SetCleanupHandler(handler func(string, string)) {
	e.mu.Lock()
	e.onCleanup = handler
	e.mu.Unlock()
}

func (e *Engine) Create(parent context.Context, source Source) (*Session, error) {
	startedAt := time.Now()
	hasURL := strings.TrimSpace(source.NZBURL) != ""
	hasPath := strings.TrimSpace(source.NZBPath) != ""
	if hasURL == hasPath {
		return nil, errors.New("provide exactly one NZB URL or cached NZB path")
	}
	ctx, cancel := context.WithTimeout(parent, e.startupTimeout)
	defer cancel()

	var nzb []byte
	var err error
	if hasPath {
		nzb, err = readNZBFile(source.NZBPath)
	} else {
		nzb, err = e.downloadNZB(ctx, source.NZBURL)
	}
	if err != nil {
		return nil, err
	}
	name := sanitizeNZBName(source.Name)
	nzoID, err := e.addNZB(ctx, name, nzb)
	if err != nil {
		return nil, err
	}
	cleanupJob := true
	defer func() {
		if cleanupJob {
			e.removeJob(context.Background(), nzoID)
		}
	}()

	mountStartedAt := time.Now()
	mount, err := e.waitForMount(ctx, nzoID)
	if err != nil {
		return nil, err
	}
	mountDuration := time.Since(mountStartedAt)
	video, err := e.waitForVideo(ctx, mount, source.FileHint)
	if err != nil {
		return nil, err
	}
	preflightStartedAt := time.Now()
	if err := e.preflightVideo(ctx, video); err != nil {
		return nil, err
	}
	preflightDuration := time.Since(preflightStartedAt)
	id, err := randomID()
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	session := &Session{
		ID: id, Name: strings.TrimSuffix(name, filepath.Ext(name)),
		FileName: video.name, FileSize: video.size, CreatedAt: now,
		nzoID: nzoID, upstreamURL: video.url, nzb: append([]byte(nil), nzb...), lastActivity: now,
	}
	e.mu.Lock()
	e.sessions[id] = session
	e.mu.Unlock()
	cleanupJob = false
	e.logger.Info("created Usenet playback", "id", id, "name", session.Name, "file", session.FileName,
		"prepare_duration", time.Since(startedAt).Round(time.Millisecond),
		"mount_duration", mountDuration.Round(time.Millisecond),
		"preflight_duration", preflightDuration.Round(time.Millisecond))
	return session, nil
}

func (e *Engine) Get(id string) (*Session, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	session, ok := e.sessions[id]
	return session, ok
}

func (e *Engine) NZB(id string) ([]byte, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	session, ok := e.sessions[id]
	if !ok {
		return nil, errors.New("playback not found")
	}
	return append([]byte(nil), session.nzb...), nil
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
	}
	return Status{
		ID: session.ID, Name: session.Name, FileName: session.FileName,
		FileSize: session.FileSize, Source: "usenet", State: state,
		ActiveStreams: session.activeStreams, LastActivity: session.lastActivity,
		DownloadedBytes: session.bytesRead,
	}, true
}

func (e *Engine) ServeHTTP(w http.ResponseWriter, r *http.Request, id string) error {
	session, ok := e.beginStream(id)
	if !ok {
		return errors.New("playback not found")
	}
	defer e.endStream(id)

	request, err := http.NewRequestWithContext(r.Context(), r.Method, session.upstreamURL, nil)
	if err != nil {
		return err
	}
	request.SetBasicAuth(e.webDAVUser, e.webDAVPassword)
	request.Header.Set("User-Agent", "filmstream/0.1")
	for _, header := range []string{"Range", "If-Range", "If-None-Match", "If-Modified-Since"} {
		if value := r.Header.Get(header); value != "" {
			request.Header.Set(header, value)
		}
	}
	response, err := e.streamClient.Do(request)
	if err != nil {
		return fmt.Errorf("stream Usenet media: %w", requestError(err))
	}
	defer response.Body.Close()
	for _, header := range []string{
		"Accept-Ranges", "Content-Length", "Content-Range", "Content-Type",
		"ETag", "Last-Modified",
	} {
		if values := response.Header.Values(header); len(values) > 0 {
			w.Header()[header] = append([]string(nil), values...)
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(response.StatusCode)
	if r.Method == http.MethodHead {
		return nil
	}
	written, copyErr := io.Copy(w, response.Body)
	e.mu.Lock()
	if current, exists := e.sessions[id]; exists {
		current.bytesRead += written
		current.lastActivity = time.Now().UTC()
	}
	e.mu.Unlock()
	if copyErr != nil {
		return fmt.Errorf("copy Usenet media: %w", copyErr)
	}
	return nil
}

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
	delete(e.sessions, id)
	handler := e.onCleanup
	e.mu.Unlock()

	e.removeJob(context.Background(), session.nzoID)
	if handler != nil {
		handler(id, "dropped")
	}
	return nil
}

func (e *Engine) Close() error {
	e.closeOnce.Do(func() {
		e.cancel()
		e.wg.Wait()
		e.mu.Lock()
		sessions := make([]*Session, 0, len(e.sessions))
		for _, session := range e.sessions {
			sessions = append(sessions, session)
		}
		e.sessions = make(map[string]*Session)
		e.mu.Unlock()
		if e.cleanOnClose {
			for _, session := range sessions {
				e.removeJob(context.Background(), session.nzoID)
			}
		}
	})
	return nil
}

func (e *Engine) beginStream(id string) (*Session, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	session, ok := e.sessions[id]
	if !ok {
		return nil, false
	}
	session.activeStreams++
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

func (e *Engine) runJanitor() {
	defer e.wg.Done()
	ticker := time.NewTicker(e.cleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-e.ctx.Done():
			return
		case now := <-ticker.C:
			e.cleanup(now.UTC())
		}
	}
}

func (e *Engine) cleanup(now time.Time) {
	e.mu.Lock()
	var expired []*Session
	for id, session := range e.sessions {
		if session.activeStreams == 0 && now.Sub(session.lastActivity) >= e.idleGrace {
			expired = append(expired, session)
			delete(e.sessions, id)
		}
	}
	handler := e.onCleanup
	e.mu.Unlock()
	for _, session := range expired {
		e.removeJob(context.Background(), session.nzoID)
		e.logger.Info("retired Usenet session", "id", session.ID, "reason", "idle")
		if handler != nil {
			handler(session.ID, "idle")
		}
	}
}

func readNZBFile(fileName string) ([]byte, error) {
	file, err := os.Open(fileName)
	if err != nil {
		return nil, fmt.Errorf("open cached NZB: %w", err)
	}
	defer file.Close()
	contents, err := io.ReadAll(io.LimitReader(file, maxNZBBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read cached NZB: %w", err)
	}
	if len(contents) == 0 {
		return nil, errors.New("cached NZB is empty")
	}
	if len(contents) > maxNZBBytes {
		return nil, errors.New("cached NZB exceeds 32 MiB")
	}
	return contents, nil
}

func (e *Engine) downloadNZB(ctx context.Context, rawURL string) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build NZB request: %w", err)
	}
	request.Header.Set("User-Agent", "filmstream/0.1")
	response, err := e.controlClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("download NZB: %w", requestError(err))
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("download NZB returned %s", response.Status)
	}
	contents, err := io.ReadAll(io.LimitReader(response.Body, maxNZBBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read NZB: %w", err)
	}
	if len(contents) == 0 {
		return nil, errors.New("downloaded NZB is empty")
	}
	if len(contents) > maxNZBBytes {
		return nil, errors.New("downloaded NZB exceeds 32 MiB")
	}
	return contents, nil
}

func (e *Engine) addNZB(ctx context.Context, name string, contents []byte) (string, error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for key, value := range map[string]string{
		"mode": "addfile", "output": "json", "cat": e.category,
		"priority": "2", "pp": "0", "nzbname": name,
	} {
		if err := writer.WriteField(key, value); err != nil {
			return "", err
		}
	}
	part, err := writer.CreateFormFile("nzbFile", name)
	if err != nil {
		return "", err
	}
	if _, err := part.Write(contents); err != nil {
		return "", err
	}
	if err := writer.Close(); err != nil {
		return "", err
	}

	endpoint := e.apiURL(nil)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, &body)
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("X-Api-Key", e.apiKey)
	response, err := e.controlClient.Do(request)
	if err != nil {
		return "", fmt.Errorf("submit NZB: %w", requestError(err))
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", fmt.Errorf("submit NZB returned %s", response.Status)
	}
	var payload struct {
		Status bool     `json:"status"`
		Error  string   `json:"error"`
		IDs    []string `json:"nzo_ids"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&payload); err != nil {
		return "", fmt.Errorf("decode NZB submission: %w", err)
	}
	if !payload.Status || len(payload.IDs) == 0 {
		if payload.Error == "" {
			payload.Error = "Usenet engine rejected the NZB"
		}
		return "", errors.New(payload.Error)
	}
	return payload.IDs[0], nil
}

type mountInfo struct {
	category string
	folder   string
}

func (e *Engine) waitForMount(ctx context.Context, nzoID string) (mountInfo, error) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		history, err := e.history(ctx, nzoID)
		if err != nil {
			return mountInfo{}, err
		}
		if len(history) > 0 {
			slot := history[0]
			switch strings.ToLower(slot.Status) {
			case "completed":
				folder := path.Base(filepath.ToSlash(slot.Storage))
				if folder == "." || folder == "/" || folder == "" {
					return mountInfo{}, errors.New("Usenet engine completed without a mounted path")
				}
				category := slot.Category
				if category == "" {
					category = e.category
				}
				return mountInfo{category: category, folder: folder}, nil
			case "failed":
				message := strings.TrimSpace(slot.FailMessage)
				if message == "" {
					message = "Usenet release could not be prepared"
				}
				return mountInfo{}, errors.New(message)
			}
		}
		select {
		case <-ctx.Done():
			return mountInfo{}, fmt.Errorf("prepare Usenet release: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

type historySlot struct {
	ID          string `json:"nzo_id"`
	Status      string `json:"status"`
	Category    string `json:"category"`
	Storage     string `json:"storage"`
	FailMessage string `json:"fail_message"`
}

func (e *Engine) history(ctx context.Context, nzoID string) ([]historySlot, error) {
	endpoint := e.apiURL(url.Values{
		"mode": {"history"}, "output": {"json"}, "nzo_ids": {nzoID},
	})
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("X-Api-Key", e.apiKey)
	response, err := e.controlClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("query Usenet preparation: %w", requestError(err))
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("query Usenet preparation returned %s", response.Status)
	}
	var payload struct {
		History struct {
			Slots []historySlot `json:"slots"`
		} `json:"history"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode Usenet preparation: %w", err)
	}
	return payload.History.Slots, nil
}

type webDAVItem struct {
	href       string
	name       string
	size       int64
	collection bool
}

type videoInfo struct {
	url  string
	name string
	size int64
}

func (e *Engine) waitForVideo(ctx context.Context, mount mountInfo, fileHint string) (videoInfo, error) {
	readyContext, cancel := context.WithTimeout(ctx, videoReadyTimeout)
	defer cancel()
	for {
		video, err := e.findLargestVideo(readyContext, mount, fileHint)
		if err == nil || !errors.Is(err, errNoSupportedVideo) {
			return video, err
		}
		select {
		case <-ctx.Done():
			return videoInfo{}, ctx.Err()
		case <-readyContext.Done():
			return videoInfo{}, err
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func (e *Engine) findLargestVideo(ctx context.Context, mount mountInfo, fileHint string) (videoInfo, error) {
	root := "/content/" + url.PathEscape(mount.category) + "/" + url.PathEscape(mount.folder)
	rootPath := cleanDAVPath(root)
	if rootPath == "" {
		return videoInfo{}, errors.New("Usenet engine returned an invalid mounted path")
	}
	pending := []struct {
		path  string
		depth int
	}{{path: root}}
	visited := make(map[string]bool)
	var videos []webDAVItem
	for len(pending) > 0 {
		current := pending[0]
		pending = pending[1:]
		if visited[current.path] || current.depth > maxWebDAVDepth {
			continue
		}
		visited[current.path] = true
		items, err := e.propfind(ctx, current.path)
		if err != nil {
			return videoInfo{}, err
		}
		for _, item := range items {
			itemPath := cleanDAVPath(item.href)
			if itemPath == "" || itemPath == cleanDAVPath(current.path) {
				continue
			}
			if itemPath != rootPath && !strings.HasPrefix(itemPath, rootPath+"/") {
				continue
			}
			if item.collection {
				pending = append(pending, struct {
					path  string
					depth int
				}{path: itemPath, depth: current.depth + 1})
				continue
			}
			if isVideoFile(item.name) {
				item.href = itemPath
				videos = append(videos, item)
			}
		}
	}
	if len(videos) == 0 {
		return videoInfo{}, errNoSupportedVideo
	}
	if strings.TrimSpace(fileHint) != "" && len(videos) > 1 {
		matching := matchingWebDAVVideos(videos, fileHint)
		if len(matching) == 0 {
			return videoInfo{}, fmt.Errorf("Usenet release contains no video matching %s", fileHint)
		}
		videos = matching
	}
	best := videos[0]
	for _, candidate := range videos[1:] {
		if candidate.size > best.size {
			best = candidate
		}
	}
	upstream := e.resolveURL(best.href)
	if best.size <= 0 {
		size, err := e.headSize(ctx, upstream)
		if err != nil {
			return videoInfo{}, err
		}
		best.size = size
	}
	return videoInfo{url: upstream, name: best.name, size: best.size}, nil
}

func matchingWebDAVVideos(videos []webDAVItem, hint string) []webDAVItem {
	hint = normalizeFileHint(hint)
	if hint == "" {
		return nil
	}
	var matching []webDAVItem
	for _, video := range videos {
		name := normalizeFileHint(video.name)
		if strings.Contains(name, hint) || matchesAlternateEpisodeCode(name, hint) {
			matching = append(matching, video)
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

func (e *Engine) propfind(ctx context.Context, davPath string) ([]webDAVItem, error) {
	body := strings.NewReader(`<?xml version="1.0"?><d:propfind xmlns:d="DAV:"><d:prop><d:resourcetype/><d:getcontentlength/><d:getcontenttype/></d:prop></d:propfind>`)
	request, err := http.NewRequestWithContext(ctx, "PROPFIND", e.resolveURL(davPath), body)
	if err != nil {
		return nil, err
	}
	request.SetBasicAuth(e.webDAVUser, e.webDAVPassword)
	request.Header.Set("Depth", "1")
	request.Header.Set("Content-Type", "application/xml")
	response, err := e.controlClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("browse Usenet release: %w", requestError(err))
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusMultiStatus {
		return nil, fmt.Errorf("browse Usenet release returned %s", response.Status)
	}
	var payload struct {
		Responses []struct {
			Href      string `xml:"href"`
			PropStats []struct {
				Status string `xml:"status"`
				Prop   struct {
					Length       string `xml:"getcontentlength"`
					ContentType  string `xml:"getcontenttype"`
					ResourceType struct {
						Collection *struct{} `xml:"collection"`
					} `xml:"resourcetype"`
				} `xml:"prop"`
			} `xml:"propstat"`
		} `xml:"response"`
	}
	if err := xml.NewDecoder(io.LimitReader(response.Body, 8<<20)).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode Usenet directory: %w", err)
	}
	items := make([]webDAVItem, 0, len(payload.Responses))
	for _, candidate := range payload.Responses {
		item := webDAVItem{href: candidate.Href}
		for _, propstat := range candidate.PropStats {
			if !strings.Contains(propstat.Status, " 200 ") {
				continue
			}
			item.size, _ = strconv.ParseInt(propstat.Prop.Length, 10, 64)
			item.collection = propstat.Prop.ResourceType.Collection != nil
			break
		}
		cleaned := cleanDAVPath(item.href)
		decoded, _ := url.PathUnescape(path.Base(cleaned))
		item.name = decoded
		items = append(items, item)
	}
	return items, nil
}

func (e *Engine) preflightVideo(ctx context.Context, video videoInfo) error {
	const (
		chunkSize   = int64(1 << 20)
		sampleCount = 3
	)
	maxStart := max(int64(0), video.size-chunkSize)
	ranges := make([][2]int64, 0, sampleCount)
	seen := make(map[int64]bool, sampleCount)
	for index := range sampleCount {
		start := maxStart * int64(index) / (sampleCount - 1)
		if seen[start] {
			continue
		}
		seen[start] = true
		ranges = append(ranges, [2]int64{start, min(video.size-1, start+chunkSize-1)})
	}

	preflightContext, cancel := context.WithCancel(ctx)
	defer cancel()
	var wait sync.WaitGroup
	var firstError error
	var errorOnce sync.Once
	for _, byteRange := range ranges {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if err := e.preflightRange(preflightContext, video.url, byteRange, chunkSize); err != nil {
				errorOnce.Do(func() {
					firstError = err
					cancel()
				})
			}
		}()
	}
	wait.Wait()
	return firstError
}

func (e *Engine) preflightRange(ctx context.Context, videoURL string, byteRange [2]int64, limit int64) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, videoURL, nil)
	if err != nil {
		return err
	}
	request.SetBasicAuth(e.webDAVUser, e.webDAVPassword)
	request.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", byteRange[0], byteRange[1]))
	response, err := e.streamClient.Do(request)
	if err != nil {
		return fmt.Errorf("verify Usenet video: %w", requestError(err))
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("verify Usenet video returned %s", response.Status)
	}
	read, err := io.Copy(io.Discard, io.LimitReader(response.Body, limit))
	if err != nil {
		return fmt.Errorf("verify Usenet video: %w", err)
	}
	if read == 0 {
		return errors.New("verify Usenet video returned no data")
	}
	return nil
}

func (e *Engine) headSize(ctx context.Context, upstream string) (int64, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodHead, upstream, nil)
	if err != nil {
		return 0, err
	}
	request.SetBasicAuth(e.webDAVUser, e.webDAVPassword)
	response, err := e.controlClient.Do(request)
	if err != nil {
		return 0, fmt.Errorf("inspect Usenet video: %w", requestError(err))
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return 0, fmt.Errorf("inspect Usenet video returned %s", response.Status)
	}
	if response.ContentLength <= 0 {
		return 0, errors.New("Usenet video did not report its size")
	}
	return response.ContentLength, nil
}

func (e *Engine) removeJob(parent context.Context, nzoID string) {
	if nzoID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(parent, 15*time.Second)
	defer cancel()
	for _, mode := range []string{"queue", "history"} {
		endpoint := e.apiURL(url.Values{
			"mode": {mode}, "name": {"delete"}, "value": {nzoID},
			"del_files": {"1"}, "output": {"json"},
		})
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			continue
		}
		request.Header.Set("X-Api-Key", e.apiKey)
		response, err := e.controlClient.Do(request)
		if err == nil {
			response.Body.Close()
		}
	}
}

func (e *Engine) apiURL(values url.Values) string {
	copy := *e.baseURL
	copy.Path = strings.TrimRight(copy.Path, "/") + "/api"
	query := copy.Query()
	for key, entries := range values {
		for _, value := range entries {
			query.Add(key, value)
		}
	}
	copy.RawQuery = query.Encode()
	return copy.String()
}

func (e *Engine) resolveURL(reference string) string {
	parsed, err := url.Parse(reference)
	if err != nil {
		return ""
	}
	return e.baseURL.ResolveReference(parsed).String()
}

func cleanDAVPath(value string) string {
	parsed, err := url.Parse(value)
	if err != nil {
		return ""
	}
	cleaned := path.Clean(parsed.Path)
	if cleaned == "." || !strings.HasPrefix(cleaned, "/content/") && cleaned != "/content" {
		return ""
	}
	return (&url.URL{Path: cleaned}).EscapedPath()
}

func isVideoFile(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".avi", ".m4v", ".mkv", ".mov", ".mp4", ".mpeg", ".mpg", ".ts", ".webm":
		return true
	default:
		return false
	}
}

func sanitizeNZBName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "filmstream"
	}
	var cleaned strings.Builder
	for _, r := range name {
		if r == '/' || r == '\\' || r < 0x20 {
			cleaned.WriteByte('_')
		} else {
			cleaned.WriteRune(r)
		}
	}
	name = strings.TrimSpace(cleaned.String())
	if !strings.HasSuffix(strings.ToLower(name), ".nzb") {
		name += ".nzb"
	}
	return name
}

func randomID() (string, error) {
	contents := make([]byte, 16)
	if _, err := rand.Read(contents); err != nil {
		return "", fmt.Errorf("create playback ID: %w", err)
	}
	return hex.EncodeToString(contents), nil
}

func requestError(err error) error {
	var urlError *url.Error
	if errors.As(err, &urlError) {
		return urlError.Err
	}
	return err
}
