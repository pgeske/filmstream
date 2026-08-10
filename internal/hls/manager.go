package hls

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultStartupTimeout = 90 * time.Second
	defaultSegmentSeconds = 4
)

type Config struct {
	DataDir        string
	FFmpegPath     string
	FFprobePath    string
	SourceBaseURL  string
	StartupTimeout time.Duration
	SegmentSeconds int
	Logger         *slog.Logger
}

type Stream struct {
	PlaybackID      string  `json:"playback_id"`
	StartSeconds    float64 `json:"start_seconds"`
	DurationSeconds float64 `json:"duration_seconds,omitempty"`
	VideoCodec      string  `json:"video_codec"`
}

type Manager struct {
	dataDir        string
	ffmpegPath     string
	ffprobePath    string
	sourceBaseURL  string
	startupTimeout time.Duration
	segmentSeconds int
	logger         *slog.Logger

	ctx       context.Context
	cancel    context.CancelFunc
	mu        sync.RWMutex
	streams   map[string]*runningStream
	closeOnce sync.Once
}

type runningStream struct {
	info   Stream
	dir    string
	cancel context.CancelFunc
	done   chan struct{}
	errMu  sync.RWMutex
	err    error
}

type mediaProbe struct {
	Streams []struct {
		CodecName    string `json:"codec_name"`
		SideDataList []struct {
			SideDataType string `json:"side_data_type"`
		} `json:"side_data_list"`
	} `json:"streams"`
	Format struct {
		Duration string `json:"duration"`
	} `json:"format"`
}

func New(cfg Config) (*Manager, error) {
	if strings.TrimSpace(cfg.DataDir) == "" {
		return nil, errors.New("HLS data directory cannot be empty")
	}
	ffmpegPath, err := exec.LookPath(defaultString(cfg.FFmpegPath, "ffmpeg"))
	if err != nil {
		return nil, fmt.Errorf("find ffmpeg: %w", err)
	}
	ffprobePath, err := exec.LookPath(defaultString(cfg.FFprobePath, "ffprobe"))
	if err != nil {
		return nil, fmt.Errorf("find ffprobe: %w", err)
	}
	if strings.TrimSpace(cfg.SourceBaseURL) == "" {
		return nil, errors.New("HLS source base URL cannot be empty")
	}
	if cfg.StartupTimeout <= 0 {
		cfg.StartupTimeout = defaultStartupTimeout
	}
	if cfg.SegmentSeconds <= 0 {
		cfg.SegmentSeconds = defaultSegmentSeconds
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return nil, fmt.Errorf("create HLS data directory: %w", err)
	}
	if err := clearDirectory(cfg.DataDir); err != nil {
		return nil, fmt.Errorf("clear HLS data directory: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Manager{
		dataDir:        cfg.DataDir,
		ffmpegPath:     ffmpegPath,
		ffprobePath:    ffprobePath,
		sourceBaseURL:  strings.TrimRight(cfg.SourceBaseURL, "/"),
		startupTimeout: cfg.StartupTimeout,
		segmentSeconds: cfg.SegmentSeconds,
		logger:         cfg.Logger,
		ctx:            ctx,
		cancel:         cancel,
		streams:        make(map[string]*runningStream),
	}, nil
}

func (m *Manager) Start(ctx context.Context, playbackID string, startSeconds float64) (Stream, error) {
	if !validPlaybackID(playbackID) {
		return Stream{}, errors.New("invalid playback ID")
	}
	if startSeconds < 0 {
		return Stream{}, errors.New("start_seconds cannot be negative")
	}
	m.Stop(playbackID)

	sourceURL := m.sourceBaseURL + "/v1/playbacks/" + playbackID + "/stream"
	probe, err := m.probe(ctx, sourceURL)
	if err != nil {
		return Stream{}, err
	}
	codec, duration, err := compatibleVideo(probe)
	if err != nil {
		return Stream{}, err
	}

	dir := filepath.Join(m.dataDir, playbackID)
	if err := os.RemoveAll(dir); err != nil {
		return Stream{}, fmt.Errorf("clear playback HLS directory: %w", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Stream{}, fmt.Errorf("create playback HLS directory: %w", err)
	}
	logFile, err := os.OpenFile(filepath.Join(dir, "ffmpeg.log"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return Stream{}, fmt.Errorf("create FFmpeg log: %w", err)
	}

	streamContext, cancel := context.WithCancel(m.ctx)
	args := m.ffmpegArgs(sourceURL, dir, codec, startSeconds)
	command := exec.CommandContext(streamContext, m.ffmpegPath, args...)
	command.Stdout = logFile
	command.Stderr = logFile
	if err := command.Start(); err != nil {
		cancel()
		logFile.Close()
		return Stream{}, fmt.Errorf("start FFmpeg: %w", err)
	}
	stream := &runningStream{
		info:   Stream{PlaybackID: playbackID, StartSeconds: startSeconds, DurationSeconds: duration, VideoCodec: codec},
		dir:    dir,
		cancel: cancel,
		done:   make(chan struct{}),
	}
	m.mu.Lock()
	m.streams[playbackID] = stream
	m.mu.Unlock()
	go func() {
		err := command.Wait()
		_ = logFile.Close()
		stream.errMu.Lock()
		stream.err = err
		stream.errMu.Unlock()
		close(stream.done)
		if err != nil && streamContext.Err() == nil {
			m.logger.Warn("HLS packager stopped", "playback_id", playbackID, "error", err)
		}
	}()

	startupContext, startupCancel := context.WithTimeout(ctx, m.startupTimeout)
	defer startupCancel()
	if err := m.waitUntilReady(startupContext, stream); err != nil {
		m.Stop(playbackID)
		return Stream{}, err
	}
	m.logger.Info("HLS stream ready", "playback_id", playbackID, "codec", codec, "start_seconds", startSeconds)
	return stream.info, nil
}

func (m *Manager) AssetPath(playbackID, name string) (string, error) {
	if !validPlaybackID(playbackID) || !validAssetName(name) {
		return "", os.ErrNotExist
	}
	m.mu.RLock()
	stream := m.streams[playbackID]
	m.mu.RUnlock()
	if stream == nil {
		return "", os.ErrNotExist
	}
	path := filepath.Join(stream.dir, name)
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return "", os.ErrNotExist
	}
	return path, nil
}

func (m *Manager) Stop(playbackID string) {
	m.mu.Lock()
	stream := m.streams[playbackID]
	delete(m.streams, playbackID)
	m.mu.Unlock()
	if stream == nil {
		return
	}
	stream.cancel()
	select {
	case <-stream.done:
	case <-time.After(5 * time.Second):
	}
	if err := os.RemoveAll(stream.dir); err != nil {
		m.logger.Warn("remove HLS stream", "playback_id", playbackID, "error", err)
	}
}

func (m *Manager) Close() error {
	m.closeOnce.Do(func() {
		m.cancel()
		m.mu.RLock()
		ids := make([]string, 0, len(m.streams))
		for id := range m.streams {
			ids = append(ids, id)
		}
		m.mu.RUnlock()
		for _, id := range ids {
			m.Stop(id)
		}
		if err := clearDirectory(m.dataDir); err != nil {
			m.logger.Warn("clear HLS data directory", "error", err)
		}
	})
	return nil
}

func (m *Manager) probe(parent context.Context, sourceURL string) (mediaProbe, error) {
	ctx, cancel := context.WithTimeout(parent, m.startupTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, m.ffprobePath,
		"-v", "error",
		"-select_streams", "v:0",
		"-show_entries", "stream=codec_name:stream_side_data=side_data_type:format=duration",
		"-of", "json",
		sourceURL,
	)
	output, err := command.Output()
	if err != nil {
		return mediaProbe{}, fmt.Errorf("probe playback media: %w", err)
	}
	var probe mediaProbe
	if err := json.Unmarshal(output, &probe); err != nil {
		return mediaProbe{}, fmt.Errorf("decode media probe: %w", err)
	}
	return probe, nil
}

func compatibleVideo(probe mediaProbe) (string, float64, error) {
	if len(probe.Streams) == 0 {
		return "", 0, errors.New("playback has no video stream")
	}
	codec := strings.ToLower(probe.Streams[0].CodecName)
	if codec != "h264" && codec != "hevc" {
		return "", 0, fmt.Errorf("video codec %q is not supported by native Apple playback", codec)
	}
	for _, sideData := range probe.Streams[0].SideDataList {
		name := strings.ToLower(sideData.SideDataType)
		if strings.Contains(name, "dovi") || strings.Contains(name, "dolby vision") {
			return "", 0, errors.New("this Dolby Vision profile is not supported by native Apple playback")
		}
	}
	duration, _ := strconv.ParseFloat(probe.Format.Duration, 64)
	return codec, duration, nil
}

func (m *Manager) ffmpegArgs(sourceURL, dir, codec string, startSeconds float64) []string {
	args := []string{
		"-hide_banner", "-loglevel", "warning", "-nostdin", "-y",
		"-readrate", "1.05", "-readrate_initial_burst", strconv.Itoa(m.segmentSeconds),
	}
	if startSeconds > 0 {
		args = append(args, "-ss", strconv.FormatFloat(startSeconds, 'f', 3, 64))
	}
	args = append(args,
		"-i", sourceURL,
		"-map", "0:v:0", "-map", "0:a:0?", "-sn", "-dn",
		"-c:v", "copy",
	)
	if codec == "hevc" {
		args = append(args, "-tag:v", "hvc1")
	}
	args = append(args,
		"-c:a", "aac", "-b:a", "256k", "-ac", "2",
		"-avoid_negative_ts", "make_zero", "-max_muxing_queue_size", "2048",
		"-f", "hls",
		"-hls_time", strconv.Itoa(m.segmentSeconds),
		"-hls_list_size", "0",
		"-hls_playlist_type", "event",
		"-hls_segment_type", "fmp4",
		"-hls_flags", "independent_segments+temp_file",
		"-hls_fmp4_init_filename", "init.mp4",
		"-hls_segment_filename", filepath.Join(dir, "segment-%06d.m4s"),
		filepath.Join(dir, "index.m3u8"),
	)
	return args
}

func (m *Manager) waitUntilReady(ctx context.Context, stream *runningStream) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if playlistReady(stream.dir) {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for initial HLS segment: %w", ctx.Err())
		case <-stream.done:
			if playlistReady(stream.dir) {
				return nil
			}
			stream.errMu.RLock()
			err := stream.err
			stream.errMu.RUnlock()
			if details := tailFile(filepath.Join(stream.dir, "ffmpeg.log"), 4096); details != "" {
				return fmt.Errorf("FFmpeg exited before HLS was ready: %v: %s", err, details)
			}
			return fmt.Errorf("FFmpeg exited before HLS was ready: %v", err)
		case <-ticker.C:
		}
	}
}

func playlistReady(dir string) bool {
	playlist, err := os.ReadFile(filepath.Join(dir, "index.m3u8"))
	if err != nil || !strings.Contains(string(playlist), "segment-") {
		return false
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "segment-*.m4s"))
	return len(matches) > 0
}

func validPlaybackID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, r := range id {
		letter := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z'
		digit := r >= '0' && r <= '9'
		if !letter && !digit && r != '-' && r != '_' {
			return false
		}
	}
	return true
}

func validAssetName(name string) bool {
	if name == "index.m3u8" || name == "init.mp4" {
		return true
	}
	return strings.HasPrefix(name, "segment-") && strings.HasSuffix(name, ".m4s") && filepath.Base(name) == name
}

func tailFile(path string, limit int64) string {
	contents, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	if int64(len(contents)) > limit {
		contents = contents[int64(len(contents))-limit:]
	}
	return strings.TrimSpace(string(contents))
}

func clearDirectory(path string) error {
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(path, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

func defaultString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
