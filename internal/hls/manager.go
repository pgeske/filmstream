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
	defaultStartupTimeout       = 90 * time.Second
	defaultStartupBufferSeconds = 16
	defaultSegmentSeconds       = 4
)

type Config struct {
	DataDir        string
	FFmpegPath     string
	FFprobePath    string
	SourceBaseURL  string
	StartupTimeout time.Duration
	BufferSeconds  int
	SegmentSeconds int
	Logger         *slog.Logger
}

type Stream struct {
	PlaybackID      string          `json:"playback_id"`
	StartSeconds    float64         `json:"start_seconds"`
	DurationSeconds float64         `json:"duration_seconds,omitempty"`
	VideoCodec      string          `json:"video_codec"`
	Subtitles       []SubtitleTrack `json:"subtitles"`
}

type SubtitleTrack struct {
	Index    int    `json:"index"`
	Language string `json:"language,omitempty"`
	Title    string `json:"title,omitempty"`
	Default  bool   `json:"default,omitempty"`
	Forced   bool   `json:"forced,omitempty"`
}

type Manager struct {
	dataDir        string
	ffmpegPath     string
	ffprobePath    string
	sourceBaseURL  string
	startupTimeout time.Duration
	bufferSeconds  int
	segmentSeconds int
	logger         *slog.Logger

	ctx       context.Context
	cancel    context.CancelFunc
	mu        sync.RWMutex
	streams   map[string]*runningStream
	closeOnce sync.Once
}

type runningStream struct {
	info           Stream
	dir            string
	sourceURL      string
	requestedStart float64
	ctx            context.Context
	cancel         context.CancelFunc
	done           chan struct{}
	errMu          sync.RWMutex
	err            error

	subtitleMu     sync.Mutex
	subtitleIndex  int
	subtitleCancel context.CancelFunc
	subtitleDone   chan struct{}
}

type mediaProbe struct {
	Streams []mediaStream `json:"streams"`
	Format  struct {
		Duration string `json:"duration"`
	} `json:"format"`
}

type mediaStream struct {
	Index        int    `json:"index"`
	CodecName    string `json:"codec_name"`
	CodecType    string `json:"codec_type"`
	SideDataList []struct {
		SideDataType string `json:"side_data_type"`
	} `json:"side_data_list"`
	Tags struct {
		Language string `json:"language"`
		Title    string `json:"title"`
	} `json:"tags"`
	Disposition struct {
		Default int `json:"default"`
		Forced  int `json:"forced"`
	} `json:"disposition"`
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
	if cfg.BufferSeconds <= 0 {
		cfg.BufferSeconds = defaultStartupBufferSeconds
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
		bufferSeconds:  cfg.BufferSeconds,
		segmentSeconds: cfg.SegmentSeconds,
		logger:         cfg.Logger,
		ctx:            ctx,
		cancel:         cancel,
		streams:        make(map[string]*runningStream),
	}, nil
}

func (m *Manager) ProbeSubtitles(ctx context.Context, playbackID string) ([]SubtitleTrack, error) {
	if !validPlaybackID(playbackID) {
		return nil, errors.New("invalid playback ID")
	}
	sourceURL := m.sourceBaseURL + "/v1/playbacks/" + playbackID + "/stream"
	probe, err := m.probe(ctx, sourceURL)
	if err != nil {
		return nil, err
	}
	return supportedSubtitles(probe), nil
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
	subtitles := supportedSubtitles(probe)
	timelineStart := startSeconds
	if startSeconds > 0 {
		if keyframeStart, err := m.probeTimelineStart(ctx, sourceURL, startSeconds); err != nil {
			m.logger.Warn("probe HLS keyframe start", "playback_id", playbackID, "error", err)
		} else {
			timelineStart = keyframeStart
		}
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
		info: Stream{
			PlaybackID: playbackID, StartSeconds: timelineStart, DurationSeconds: duration,
			VideoCodec: codec, Subtitles: subtitles,
		},
		dir:            dir,
		sourceURL:      sourceURL,
		requestedStart: startSeconds,
		ctx:            streamContext,
		cancel:         cancel,
		done:           make(chan struct{}),
		subtitleIndex:  -1,
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
	m.logger.Info("HLS stream ready", "playback_id", playbackID, "codec", codec,
		"text_subtitle_tracks", len(subtitles),
		"requested_start_seconds", startSeconds, "timeline_start_seconds", timelineStart)
	return stream.info, nil
}

func (m *Manager) StartSubtitle(_ context.Context, playbackID string, index int) error {
	if !validPlaybackID(playbackID) || index < 0 {
		return os.ErrNotExist
	}
	m.mu.RLock()
	stream := m.streams[playbackID]
	m.mu.RUnlock()
	if stream == nil || !hasSubtitle(stream.info.Subtitles, index) {
		return os.ErrNotExist
	}

	stream.subtitleMu.Lock()
	defer stream.subtitleMu.Unlock()
	if stream.subtitleIndex == index && stream.subtitleCancel != nil {
		return nil
	}
	m.stopSubtitleLocked(stream)

	for _, match := range []string{"subtitle-*.vtt", "subtitle-*.log"} {
		paths, _ := filepath.Glob(filepath.Join(stream.dir, match))
		for _, path := range paths {
			_ = os.Remove(path)
		}
	}

	logPath := filepath.Join(stream.dir, fmt.Sprintf("subtitle-%d.log", index))
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create subtitle log: %w", err)
	}
	subtitleContext, cancel := context.WithCancel(stream.ctx)
	args := m.subtitleArgs(stream, index)
	command := exec.CommandContext(subtitleContext, m.ffmpegPath, args...)
	command.Stdout = logFile
	command.Stderr = logFile
	if err := command.Start(); err != nil {
		cancel()
		_ = logFile.Close()
		return fmt.Errorf("start subtitle conversion: %w", err)
	}
	stream.subtitleIndex = index
	stream.subtitleCancel = cancel
	stream.subtitleDone = make(chan struct{})
	done := stream.subtitleDone
	go func() {
		err := command.Wait()
		_ = logFile.Close()
		close(done)
		if err != nil && subtitleContext.Err() == nil {
			m.logger.Warn("subtitle conversion stopped", "playback_id", playbackID, "index", index, "error", err)
		}
	}()
	m.logger.Info("subtitle conversion started", "playback_id", playbackID, "index", index)
	return nil
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
	stream.subtitleMu.Lock()
	m.stopSubtitleLocked(stream)
	stream.subtitleMu.Unlock()
	stream.cancel()
	select {
	case <-stream.done:
	case <-time.After(5 * time.Second):
	}
	if err := os.RemoveAll(stream.dir); err != nil {
		m.logger.Warn("remove HLS stream", "playback_id", playbackID, "error", err)
	}
}

func (m *Manager) stopSubtitleLocked(stream *runningStream) {
	if stream.subtitleCancel == nil {
		return
	}
	stream.subtitleCancel()
	if stream.subtitleDone != nil {
		select {
		case <-stream.subtitleDone:
		case <-time.After(5 * time.Second):
		}
	}
	stream.subtitleIndex = -1
	stream.subtitleCancel = nil
	stream.subtitleDone = nil
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
		"-show_entries", "stream=index,codec_name,codec_type:stream_tags=language,title:stream_disposition=default,forced:stream_side_data=side_data_type:format=duration",
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

func (m *Manager) probeTimelineStart(parent context.Context, sourceURL string, requested float64) (float64, error) {
	ctx, cancel := context.WithTimeout(parent, m.startupTimeout)
	defer cancel()
	interval := strconv.FormatFloat(requested, 'f', 3, 64) + "%+#1"
	command := exec.CommandContext(ctx, m.ffprobePath,
		"-v", "error",
		"-read_intervals", interval,
		"-select_streams", "v:0",
		"-show_entries", "packet=pts_time",
		"-of", "default=noprint_wrappers=1:nokey=1",
		sourceURL,
	)
	output, err := command.Output()
	if err != nil {
		return requested, fmt.Errorf("probe video keyframe: %w", err)
	}
	fields := strings.Fields(string(output))
	if len(fields) == 0 {
		return requested, errors.New("probe video keyframe returned no timestamp")
	}
	start, err := strconv.ParseFloat(fields[0], 64)
	if err != nil || start < 0 || start > requested+0.5 {
		return requested, fmt.Errorf("invalid video keyframe timestamp %q", fields[0])
	}
	return start, nil
}

func compatibleVideo(probe mediaProbe) (string, float64, error) {
	for _, stream := range probe.Streams {
		if stream.CodecType != "video" {
			continue
		}
		codec := strings.ToLower(stream.CodecName)
		if codec != "h264" && codec != "hevc" {
			return "", 0, fmt.Errorf("video codec %q is not supported by native Apple playback", codec)
		}
		for _, sideData := range stream.SideDataList {
			name := strings.ToLower(sideData.SideDataType)
			if strings.Contains(name, "dovi") || strings.Contains(name, "dolby vision") {
				return "", 0, errors.New("this Dolby Vision profile is not supported by native Apple playback")
			}
		}
		duration, _ := strconv.ParseFloat(probe.Format.Duration, 64)
		return codec, duration, nil
	}
	return "", 0, errors.New("playback has no video stream")
}

func supportedSubtitles(probe mediaProbe) []SubtitleTrack {
	var tracks []SubtitleTrack
	for _, stream := range probe.Streams {
		if stream.CodecType != "subtitle" || !isTextSubtitleCodec(stream.CodecName) {
			continue
		}
		tracks = append(tracks, SubtitleTrack{
			Index:    stream.Index,
			Language: canonicalLanguage(stream.Tags.Language),
			Title:    strings.TrimSpace(stream.Tags.Title),
			Default:  stream.Disposition.Default != 0,
			Forced:   stream.Disposition.Forced != 0,
		})
	}
	return tracks
}

func hasSubtitle(tracks []SubtitleTrack, index int) bool {
	for _, track := range tracks {
		if track.Index == index {
			return true
		}
	}
	return false
}

func isTextSubtitleCodec(codec string) bool {
	switch strings.ToLower(codec) {
	case "ass", "mov_text", "ssa", "subrip", "text", "webvtt":
		return true
	default:
		return false
	}
}

func canonicalLanguage(language string) string {
	language = strings.ToLower(strings.TrimSpace(language))
	aliases := map[string]string{
		"ara": "ar", "bul": "bg", "cat": "ca", "chi": "zh", "zho": "zh",
		"hrv": "hr", "cze": "cs", "ces": "cs", "dan": "da", "dut": "nl", "nld": "nl",
		"eng": "en", "est": "et", "fin": "fi", "fre": "fr", "fra": "fr",
		"ger": "de", "deu": "de", "gre": "el", "ell": "el", "heb": "he",
		"hun": "hu", "ice": "is", "isl": "is", "ind": "id", "ita": "it",
		"jpn": "ja", "kor": "ko", "lav": "lv", "lit": "lt", "mac": "mk", "mkd": "mk",
		"may": "ms", "msa": "ms", "nob": "no", "per": "fa", "fas": "fa",
		"pol": "pl", "por": "pt", "rum": "ro", "ron": "ro", "rus": "ru",
		"srp": "sr", "slo": "sk", "slk": "sk", "slv": "sl", "spa": "es",
		"swe": "sv", "tha": "th", "tur": "tr", "ukr": "uk",
	}
	if canonical := aliases[language]; canonical != "" {
		return canonical
	}
	return language
}

func (m *Manager) ffmpegArgs(sourceURL, dir, codec string, startSeconds float64) []string {
	// Allow one segment beyond the target buffer to be packaged without rate limiting.
	// Stream-copied video can only cut on keyframes, so segment lengths may exceed the target.
	initialBurstSeconds := m.bufferSeconds + m.segmentSeconds
	args := []string{
		"-hide_banner", "-loglevel", "warning", "-nostdin", "-y",
		"-readrate", "1.05", "-readrate_initial_burst", strconv.Itoa(initialBurstSeconds),
	}
	if startSeconds > 0 {
		// Video is stream-copied while audio is transcoded. Keeping pre-roll for both
		// prevents accurate input seeking from discarding audio before the first video keyframe.
		args = append(args, "-noaccurate_seek", "-ss", strconv.FormatFloat(startSeconds, 'f', 3, 64))
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

func (m *Manager) subtitleArgs(stream *runningStream, index int) []string {
	initialBurstSeconds := m.bufferSeconds + m.segmentSeconds
	args := []string{
		"-hide_banner", "-loglevel", "warning", "-nostdin", "-y",
		"-readrate", "1.05", "-readrate_initial_burst", strconv.Itoa(initialBurstSeconds),
	}
	if stream.requestedStart > 0 {
		args = append(args,
			"-noaccurate_seek", "-ss", strconv.FormatFloat(stream.requestedStart, 'f', 3, 64),
		)
	}
	args = append(args,
		"-i", stream.sourceURL,
		"-map", fmt.Sprintf("0:%d", index),
		"-c:s", "webvtt",
	)
	if offset := stream.requestedStart - stream.info.StartSeconds; offset > 0 {
		args = append(args, "-output_ts_offset", strconv.FormatFloat(offset, 'f', 3, 64))
	}
	return append(args,
		"-flush_packets", "1", "-f", "webvtt",
		filepath.Join(stream.dir, fmt.Sprintf("subtitle-%d.vtt", index)),
	)
}

func (m *Manager) waitUntilReady(ctx context.Context, stream *runningStream) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if playlistReady(stream.dir, m.bufferSeconds) {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for HLS startup buffer: %w", ctx.Err())
		case <-stream.done:
			if playlistReady(stream.dir, m.bufferSeconds) {
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

func playlistReady(dir string, minimumSeconds int) bool {
	playlist, err := os.ReadFile(filepath.Join(dir, "index.m3u8"))
	if err != nil {
		return false
	}

	var duration float64
	segmentCount := 0
	for _, line := range strings.Split(string(playlist), "\n") {
		if !strings.HasPrefix(line, "#EXTINF:") {
			continue
		}
		value := strings.TrimSuffix(strings.TrimPrefix(line, "#EXTINF:"), ",")
		seconds, err := strconv.ParseFloat(value, 64)
		if err != nil || seconds <= 0 {
			continue
		}
		duration += seconds
		segmentCount++
	}
	if segmentCount == 0 || duration < float64(minimumSeconds) {
		return false
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "segment-*.m4s"))
	return len(matches) >= segmentCount
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
	if strings.HasPrefix(name, "segment-") && strings.HasSuffix(name, ".m4s") {
		return filepath.Base(name) == name
	}
	if !strings.HasPrefix(name, "subtitle-") || !strings.HasSuffix(name, ".vtt") || filepath.Base(name) != name {
		return false
	}
	index := strings.TrimSuffix(strings.TrimPrefix(name, "subtitle-"), ".vtt")
	value, err := strconv.Atoi(index)
	return err == nil && value >= 0
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
