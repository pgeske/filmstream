package hls

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	defaultStartupTimeout       = 90 * time.Second
	defaultStartupBufferSeconds = 12
	defaultReadRate             = 1.25
	defaultSegmentSeconds       = 4
	defaultParkedTTL            = 2 * time.Hour
	defaultParkedResumeTimeout  = 6 * time.Second
	startupProgressInterval     = 5 * time.Second
)

type Config struct {
	DataDir               string
	FFmpegPath            string
	FFprobePath           string
	SourceBaseURL         string
	StartupTimeout        time.Duration
	BufferSeconds         int
	ReadRate              float64
	SegmentSeconds        int
	ParkedTTL             time.Duration
	ParkedResumeTimeout   time.Duration
	Logger                *slog.Logger
	BitmapSubtitleEncoder string
	LocalSourcePath       func(string) (string, bool)
}

type Stream struct {
	PlaybackID            string          `json:"playback_id"`
	RequestedStartSeconds float64         `json:"requested_start_seconds"`
	StartSeconds          float64         `json:"start_seconds"`
	DurationSeconds       float64         `json:"duration_seconds,omitempty"`
	VideoCodec            string          `json:"video_codec"`
	Subtitles             []SubtitleTrack `json:"subtitles"`
	BurnedSubtitleIndex   *int            `json:"burned_subtitle_index,omitempty"`
}

type SubtitleTrack struct {
	Index    int    `json:"index"`
	Language string `json:"language,omitempty"`
	Title    string `json:"title,omitempty"`
	Default  bool   `json:"default,omitempty"`
	Forced   bool   `json:"forced,omitempty"`
	Codec    string `json:"codec,omitempty"`
	Kind     string `json:"kind,omitempty"`
}

type Manager struct {
	dataDir               string
	ffmpegPath            string
	ffprobePath           string
	sourceBaseURL         string
	startupTimeout        time.Duration
	bufferSeconds         int
	readRate              float64
	segmentSeconds        int
	parkedTTL             time.Duration
	parkedResumeTimeout   time.Duration
	logger                *slog.Logger
	bitmapSubtitleEncoder string
	localSourcePath       func(string) (string, bool)

	ctx         context.Context
	cancel      context.CancelFunc
	mu          sync.RWMutex
	streams     map[string]*runningStream
	startMu     sync.Mutex
	startLocks  map[string]*playbackStartLock
	probeMu     sync.Mutex
	probes      map[string]*playbackProbe
	probeWG     sync.WaitGroup
	probeClosed bool
	closeOnce   sync.Once
}

type playbackStartLock struct {
	mu   sync.Mutex
	refs int
}

type playbackProbe struct {
	done   chan struct{}
	cancel context.CancelFunc
	probe  mediaProbe
	err    error
}

type runningStream struct {
	info                Stream
	dir                 string
	sourceURL           string
	requestedStart      float64
	ctx                 context.Context
	cancel              context.CancelFunc
	done                chan struct{}
	errMu               sync.RWMutex
	err                 error
	command             *exec.Cmd
	processMu           sync.Mutex
	parked              bool
	parkedAt            time.Time
	parkTimer           *time.Timer
	languages           []string
	bitmapSubtitleIndex int

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

type videoPacketProbe struct {
	Packets []struct {
		PTSTime string `json:"pts_time"`
		Flags   string `json:"flags"`
	} `json:"packets"`
}

type transientTimelineProbeError struct {
	err error
}

func (e *transientTimelineProbeError) Error() string { return e.err.Error() }
func (e *transientTimelineProbeError) Unwrap() error { return e.err }

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
	if cfg.ReadRate == 0 {
		cfg.ReadRate = defaultReadRate
	}
	if cfg.ReadRate < 1 || cfg.ReadRate > 4 {
		return nil, errors.New("HLS read rate must be between 1 and 4")
	}
	if cfg.SegmentSeconds <= 0 {
		cfg.SegmentSeconds = defaultSegmentSeconds
	}
	if cfg.ParkedTTL <= 0 {
		cfg.ParkedTTL = defaultParkedTTL
	}
	if cfg.ParkedResumeTimeout <= 0 {
		cfg.ParkedResumeTimeout = defaultParkedResumeTimeout
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	if cfg.BitmapSubtitleEncoder == "" {
		cfg.BitmapSubtitleEncoder = "libx264"
	}
	if cfg.BitmapSubtitleEncoder != "libx264" && cfg.BitmapSubtitleEncoder != "h264_nvenc" {
		return nil, fmt.Errorf("unsupported bitmap subtitle encoder %q", cfg.BitmapSubtitleEncoder)
	}
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return nil, fmt.Errorf("create HLS data directory: %w", err)
	}
	if err := clearDirectory(cfg.DataDir); err != nil {
		return nil, fmt.Errorf("clear HLS data directory: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Manager{
		dataDir:               cfg.DataDir,
		ffmpegPath:            ffmpegPath,
		ffprobePath:           ffprobePath,
		sourceBaseURL:         strings.TrimRight(cfg.SourceBaseURL, "/"),
		startupTimeout:        cfg.StartupTimeout,
		bufferSeconds:         cfg.BufferSeconds,
		readRate:              cfg.ReadRate,
		segmentSeconds:        cfg.SegmentSeconds,
		parkedTTL:             cfg.ParkedTTL,
		parkedResumeTimeout:   cfg.ParkedResumeTimeout,
		logger:                cfg.Logger,
		bitmapSubtitleEncoder: cfg.BitmapSubtitleEncoder,
		localSourcePath:       cfg.LocalSourcePath,
		ctx:                   ctx,
		cancel:                cancel,
		streams:               make(map[string]*runningStream),
		startLocks:            make(map[string]*playbackStartLock),
		probes:                make(map[string]*playbackProbe),
	}, nil
}

func (m *Manager) lockPlaybackStart(playbackID string) func() {
	m.startMu.Lock()
	lock := m.startLocks[playbackID]
	if lock == nil {
		lock = &playbackStartLock{}
		m.startLocks[playbackID] = lock
	}
	lock.refs++
	m.startMu.Unlock()
	lock.mu.Lock()
	return func() {
		lock.mu.Unlock()
		m.startMu.Lock()
		lock.refs--
		if lock.refs == 0 && m.startLocks[playbackID] == lock {
			delete(m.startLocks, playbackID)
		}
		m.startMu.Unlock()
	}
}

func (m *Manager) ProbeSubtitles(ctx context.Context, playbackID string) ([]SubtitleTrack, error) {
	if !validPlaybackID(playbackID) {
		return nil, errors.New("invalid playback ID")
	}
	sourceURL := m.sourceBaseURL + "/v1/playbacks/" + playbackID + "/stream"
	probeSource, _ := m.sourceForProbe(playbackID, sourceURL)
	probe, err := m.probePlayback(ctx, playbackID, probeSource)
	if err != nil {
		return nil, err
	}
	return supportedSubtitles(probe), nil
}

func (m *Manager) sourceForProbe(playbackID, sourceURL string) (string, bool) {
	if m.localSourcePath != nil {
		if path, ok := m.localSourcePath(playbackID); ok && path != "" {
			return path, true
		}
	}
	return sourceURL, false
}

func (m *Manager) Start(
	ctx context.Context,
	playbackID string,
	startSeconds float64,
	preferredLanguages []string,
	bitmapSubtitleIndex int,
) (Stream, error) {
	if !validPlaybackID(playbackID) {
		return Stream{}, errors.New("invalid playback ID")
	}
	if startSeconds < 0 {
		return Stream{}, errors.New("start_seconds cannot be negative")
	}
	unlockStart := m.lockPlaybackStart(playbackID)
	defer unlockStart()
	if stream, matched, err := m.resumePreparedStream(
		ctx, playbackID, startSeconds, preferredLanguages, bitmapSubtitleIndex,
	); matched {
		return stream, err
	}
	if stream, covered := m.resumeCoveredStream(
		playbackID, startSeconds, preferredLanguages, bitmapSubtitleIndex,
	); covered {
		return stream, nil
	}
	m.stopPlaybackStream(playbackID)

	sourceURL := m.sourceBaseURL + "/v1/playbacks/" + playbackID + "/stream"
	probeSource, probeIsLocal := m.sourceForProbe(playbackID, sourceURL)
	probe, err := m.probePlayback(ctx, playbackID, probeSource)
	if err != nil {
		return Stream{}, err
	}
	codec, duration, err := compatibleVideo(probe)
	if err != nil {
		return Stream{}, err
	}
	audioIndex := -1
	audioLanguage := ""
	if audio, found := preferredAudioStream(probe, preferredLanguages); found {
		audioIndex = audio.Index
		audioLanguage = canonicalLanguage(audio.Tags.Language)
	}
	subtitles := supportedSubtitles(probe)
	if bitmapSubtitleIndex >= 0 && !hasBitmapSubtitle(subtitles, bitmapSubtitleIndex) {
		return Stream{}, fmt.Errorf("bitmap subtitle track %d is unavailable", bitmapSubtitleIndex)
	}
	timelineStart := startSeconds
	retryKeyframeProbe := false
	if startSeconds > 0 {
		if keyframeStart, err := m.probeTimelineStart(ctx, probeSource, startSeconds); err != nil {
			var transient *transientTimelineProbeError
			if probeIsLocal || !errors.As(err, &transient) {
				return Stream{}, fmt.Errorf("probe HLS keyframe start: %w", err)
			}
			retryKeyframeProbe = true
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
	args := m.ffmpegArgs(sourceURL, dir, codec, timelineStart, audioIndex, bitmapSubtitleIndex)
	command := exec.CommandContext(streamContext, m.ffmpegPath, args...)
	command.Stdout = logFile
	command.Stderr = logFile
	if err := command.Start(); err != nil {
		cancel()
		logFile.Close()
		return Stream{}, fmt.Errorf("start FFmpeg: %w", err)
	}
	outputCodec := codec
	if bitmapSubtitleIndex >= 0 {
		outputCodec = "h264"
	}
	stream := &runningStream{
		info: Stream{
			PlaybackID: playbackID, RequestedStartSeconds: startSeconds,
			StartSeconds: timelineStart, DurationSeconds: duration,
			VideoCodec: outputCodec, Subtitles: subtitles,
			BurnedSubtitleIndex: optionalIndex(bitmapSubtitleIndex),
		},
		dir:                 dir,
		sourceURL:           sourceURL,
		requestedStart:      startSeconds,
		ctx:                 streamContext,
		cancel:              cancel,
		done:                make(chan struct{}),
		command:             command,
		languages:           append([]string(nil), preferredLanguages...),
		bitmapSubtitleIndex: bitmapSubtitleIndex,
		subtitleIndex:       -1,
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
	if retryKeyframeProbe {
		// The packager has now read through the cold source region. Re-probe once
		// and require the actual landing point before handing the stream to the
		// client; reporting the requested time after another failure would silently
		// desync the client timeline and WebVTT sidecar from stream-copied video.
		keyframeStart, err := m.probeTimelineStart(startupContext, sourceURL, startSeconds)
		if err != nil {
			m.logger.Warn("reprobe HLS keyframe start", "playback_id", playbackID, "error", err)
			m.Stop(playbackID)
			return Stream{}, fmt.Errorf("reprobe HLS keyframe start: %w", err)
		}
		if keyframeStart != timelineStart {
			stream.info.StartSeconds = keyframeStart
			timelineStart = keyframeStart
			m.logger.Info("corrected HLS keyframe start", "playback_id", playbackID,
				"timeline_start_seconds", keyframeStart)
		}
	}
	m.logger.Info("HLS stream ready", "playback_id", playbackID, "codec", outputCodec,
		"audio_stream_index", audioIndex, "audio_language", audioLanguage,
		"subtitle_tracks", len(subtitles), "burned_subtitle_index", bitmapSubtitleIndex,
		"requested_start_seconds", startSeconds, "timeline_start_seconds", timelineStart)
	return stream.info, nil
}

func (m *Manager) resumePreparedStream(
	ctx context.Context,
	playbackID string,
	startSeconds float64,
	preferredLanguages []string,
	bitmapSubtitleIndex int,
) (Stream, bool, error) {
	m.mu.RLock()
	stream := m.streams[playbackID]
	m.mu.RUnlock()
	if stream == nil || math.Abs(stream.requestedStart-startSeconds) > 0.5 ||
		!equalStrings(stream.languages, preferredLanguages) ||
		stream.bitmapSubtitleIndex != bitmapSubtitleIndex {
		return Stream{}, false, nil
	}

	stream.processMu.Lock()
	select {
	case <-stream.done:
		stream.processMu.Unlock()
		return Stream{}, false, nil
	default:
	}
	m.mu.RLock()
	isCurrent := m.streams[playbackID] == stream
	m.mu.RUnlock()
	if !isCurrent {
		stream.processMu.Unlock()
		return Stream{}, false, nil
	}
	wasParked := stream.parked
	bufferedSegments, _, _ := playlistStatus(stream.dir)
	if wasParked {
		if err := stream.command.Process.Signal(syscall.SIGCONT); err != nil {
			stream.processMu.Unlock()
			m.logger.Warn("resume prepared HLS stream", "playback_id", playbackID, "error", err)
			m.stopPlaybackStream(playbackID)
			return Stream{}, false, nil
		}
		stream.parked = false
		stream.parkedAt = time.Time{}
		if stream.parkTimer != nil {
			stream.parkTimer.Stop()
			stream.parkTimer = nil
		}
		m.logger.Info("resumed prepared HLS stream", "playback_id", playbackID,
			"start_seconds", stream.info.StartSeconds)
	}
	stream.processMu.Unlock()

	if wasParked {
		if err := m.waitForPlaylistGrowth(ctx, stream, bufferedSegments); err != nil {
			m.logger.Warn("prepared HLS source did not resume; rebuilding stream",
				"playback_id", playbackID, "error", err)
			m.stopPlaybackStream(playbackID)
			return Stream{}, false, nil
		}
	}

	startupContext, cancel := context.WithTimeout(ctx, m.startupTimeout)
	defer cancel()
	if err := m.waitUntilReady(startupContext, stream); err != nil {
		m.Stop(playbackID)
		return Stream{}, true, err
	}
	return stream.info, true, nil
}

func (m *Manager) Prepared(
	playbackID string,
	startSeconds float64,
	preferredLanguages []string,
	bitmapSubtitleIndex int,
	minimumSeconds int,
) bool {
	m.mu.RLock()
	stream := m.streams[playbackID]
	m.mu.RUnlock()
	if stream == nil || math.Abs(stream.requestedStart-startSeconds) > 0.5 ||
		!equalStrings(stream.languages, preferredLanguages) ||
		stream.bitmapSubtitleIndex != bitmapSubtitleIndex {
		return false
	}
	select {
	case <-stream.done:
		return false
	default:
	}
	if minimumSeconds <= 0 {
		minimumSeconds = m.bufferSeconds
	}
	return playlistReady(stream.dir, minimumSeconds)
}

func (m *Manager) resumeCoveredStream(
	playbackID string,
	startSeconds float64,
	preferredLanguages []string,
	bitmapSubtitleIndex int,
) (Stream, bool) {
	m.mu.RLock()
	stream := m.streams[playbackID]
	m.mu.RUnlock()
	if stream == nil || !equalStrings(stream.languages, preferredLanguages) ||
		stream.bitmapSubtitleIndex != bitmapSubtitleIndex {
		return Stream{}, false
	}
	stream.processMu.Lock()
	defer stream.processMu.Unlock()
	select {
	case <-stream.done:
		return Stream{}, false
	default:
	}
	m.mu.RLock()
	isCurrent := m.streams[playbackID] == stream
	m.mu.RUnlock()
	if !isCurrent {
		return Stream{}, false
	}
	_, packagedSeconds, _ := playlistStatus(stream.dir)
	position := startSeconds - stream.info.StartSeconds
	// The event playlist retains every packaged segment, so a request inside
	// the packaged range (with one segment of headroom) can seek within the
	// existing stream instead of discarding it and re-downloading from scratch.
	// Recovery requests land here after a client stall, where a rebuild would
	// throw away the buffer and re-probe the source at the worst possible time.
	headroom := float64(m.segmentSeconds)
	if position < 0 || position > packagedSeconds-headroom {
		m.logger.Info("prepared HLS stream does not cover requested position; rebuilding",
			"playback_id", playbackID, "requested_start_seconds", startSeconds,
			"timeline_start_seconds", stream.info.StartSeconds,
			"packaged_seconds", packagedSeconds)
		return Stream{}, false
	}
	if stream.parked {
		if err := stream.command.Process.Signal(syscall.SIGCONT); err != nil {
			m.logger.Warn("resume covered HLS stream", "playback_id", playbackID, "error", err)
			return Stream{}, false
		}
		stream.parked = false
		stream.parkedAt = time.Time{}
		if stream.parkTimer != nil {
			stream.parkTimer.Stop()
			stream.parkTimer = nil
		}
	}
	if !playlistReady(stream.dir, m.bufferSeconds) {
		return Stream{}, false
	}
	m.logger.Info("reused prepared HLS stream for in-range position",
		"playback_id", playbackID, "requested_start_seconds", startSeconds,
		"timeline_start_seconds", stream.info.StartSeconds,
		"packaged_seconds", packagedSeconds)
	return stream.info, true
}

func (m *Manager) Park(ctx context.Context, playbackID string, minimumSeconds int) error {
	unlockStart := m.lockPlaybackStart(playbackID)
	defer unlockStart()
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.RLock()
	stream := m.streams[playbackID]
	m.mu.RUnlock()
	if stream == nil {
		return os.ErrNotExist
	}
	if minimumSeconds <= 0 {
		minimumSeconds = m.bufferSeconds
	}
	startTime := time.Now()
	if err := m.waitUntilBuffered(ctx, stream, minimumSeconds); err != nil {
		return err
	}
	bufferWait := time.Since(startTime)

	stream.processMu.Lock()
	defer stream.processMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.RLock()
	isCurrent := m.streams[playbackID] == stream
	m.mu.RUnlock()
	if !isCurrent {
		return os.ErrNotExist
	}
	select {
	case <-stream.done:
		return errors.New("HLS packager is no longer running")
	default:
	}
	if stream.parked {
		return nil
	}
	// Keep the same FFmpeg process and source connection so resuming preserves
	// A/V timestamps while avoiding any additional download or packaging work.
	if err := stream.command.Process.Signal(syscall.SIGSTOP); err != nil {
		return fmt.Errorf("park HLS packager: %w", err)
	}
	stream.parked = true
	stream.parkedAt = time.Now()
	parkedAt := stream.parkedAt
	stream.parkTimer = time.AfterFunc(m.parkedTTL, func() {
		m.expireParkedStream(playbackID, stream, parkedAt)
	})
	m.logger.Info("parked prepared HLS stream", "playback_id", playbackID,
		"buffer_seconds", minimumSeconds, "buffer_wait_seconds", bufferWait.Seconds(),
		"expires_in", m.parkedTTL)
	return nil
}

func (m *Manager) expireParkedStream(playbackID string, stream *runningStream, parkedAt time.Time) {
	stream.processMu.Lock()
	if !stream.parked || !stream.parkedAt.Equal(parkedAt) {
		stream.processMu.Unlock()
		return
	}
	m.mu.Lock()
	if m.streams[playbackID] != stream {
		m.mu.Unlock()
		stream.processMu.Unlock()
		return
	}
	delete(m.streams, playbackID)
	m.mu.Unlock()
	stream.parked = false
	stream.parkedAt = time.Time{}
	stream.parkTimer = nil
	stream.processMu.Unlock()

	m.logger.Info("expired prepared HLS stream", "playback_id", playbackID)
	m.stopStream(playbackID, stream)
}

func (m *Manager) StartSubtitle(_ context.Context, playbackID string, index int) error {
	if !validPlaybackID(playbackID) || index < 0 {
		return os.ErrNotExist
	}
	m.mu.RLock()
	stream := m.streams[playbackID]
	m.mu.RUnlock()
	if stream == nil || !hasTextSubtitle(stream.info.Subtitles, index) {
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
	m.stopPlaybackStream(playbackID)
	m.forgetProbe(playbackID)
}

func (m *Manager) stopPlaybackStream(playbackID string) {
	m.mu.Lock()
	stream := m.streams[playbackID]
	delete(m.streams, playbackID)
	m.mu.Unlock()
	if stream == nil {
		return
	}
	stream.processMu.Lock()
	stream.parked = false
	stream.parkedAt = time.Time{}
	if stream.parkTimer != nil {
		stream.parkTimer.Stop()
		stream.parkTimer = nil
	}
	stream.processMu.Unlock()
	m.stopStream(playbackID, stream)
}

func (m *Manager) stopStream(playbackID string, stream *runningStream) {
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
		m.closeProbes()
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

func (m *Manager) probePlayback(
	parent context.Context,
	playbackID string,
	sourceURL string,
) (mediaProbe, error) {
	if err := parent.Err(); err != nil {
		return mediaProbe{}, err
	}

	m.probeMu.Lock()
	if m.probeClosed {
		m.probeMu.Unlock()
		return mediaProbe{}, errors.New("HLS manager is closed")
	}
	cached := m.probes[playbackID]
	if cached == nil {
		probeContext, cancel := context.WithCancel(m.ctx)
		cached = &playbackProbe{done: make(chan struct{}), cancel: cancel}
		m.probes[playbackID] = cached
		m.probeWG.Add(1)
		go m.runPlaybackProbe(probeContext, playbackID, sourceURL, cached)
	}
	m.probeMu.Unlock()

	select {
	case <-parent.Done():
		return mediaProbe{}, parent.Err()
	case <-cached.done:
		return cached.probe, cached.err
	}
}

func (m *Manager) runPlaybackProbe(
	ctx context.Context,
	playbackID string,
	sourceURL string,
	cached *playbackProbe,
) {
	defer m.probeWG.Done()
	probe, err := m.probe(ctx, sourceURL)

	m.probeMu.Lock()
	if m.probes[playbackID] != cached {
		probe = mediaProbe{}
		if err == nil {
			err = context.Canceled
		}
	} else if err != nil {
		delete(m.probes, playbackID)
	}
	cached.probe = probe
	cached.err = err
	cached.cancel()
	close(cached.done)
	m.probeMu.Unlock()
}

func (m *Manager) forgetProbe(playbackID string) {
	m.probeMu.Lock()
	cached := m.probes[playbackID]
	delete(m.probes, playbackID)
	m.probeMu.Unlock()
	if cached != nil {
		cached.cancel()
	}
}

func (m *Manager) closeProbes() {
	m.probeMu.Lock()
	m.probeClosed = true
	cached := make([]*playbackProbe, 0, len(m.probes))
	for playbackID, probe := range m.probes {
		delete(m.probes, playbackID)
		cached = append(cached, probe)
	}
	m.probeMu.Unlock()
	for _, probe := range cached {
		probe.cancel()
	}
	m.probeWG.Wait()
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

func (m *Manager) probeTimelineStart(parent context.Context, source string, requested float64) (float64, error) {
	minimumStart := math.Max(0, requested-60)
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	interval := strconv.FormatFloat(minimumStart, 'f', 3, 64) + "%" +
		strconv.FormatFloat(requested+0.5, 'f', 3, 64)
	command := exec.CommandContext(ctx, m.ffprobePath,
		"-v", "error",
		"-read_intervals", interval,
		"-select_streams", "v:0",
		"-show_entries", "packet=pts_time,flags",
		"-of", "json",
		source,
	)
	output, err := command.Output()
	if err != nil {
		return requested, &transientTimelineProbeError{
			err: fmt.Errorf("probe video keyframes: %w", err),
		}
	}
	var probe videoPacketProbe
	if err := json.Unmarshal(output, &probe); err != nil {
		return requested, fmt.Errorf("decode video keyframe probe: %w", err)
	}

	start := -1.0
	invalidTimestamp := ""
	for _, packet := range probe.Packets {
		timestamp, err := strconv.ParseFloat(packet.PTSTime, 64)
		if err != nil || math.IsNaN(timestamp) || math.IsInf(timestamp, 0) {
			if packet.PTSTime != "" {
				invalidTimestamp = packet.PTSTime
			}
			continue
		}
		if strings.Contains(packet.Flags, "K") && timestamp >= minimumStart &&
			timestamp <= requested+0.5 && timestamp > start {
			start = timestamp
			continue
		}
		if strings.Contains(packet.Flags, "K") {
			invalidTimestamp = packet.PTSTime
		}
	}
	if start >= 0 {
		return start, nil
	}
	if invalidTimestamp != "" {
		return requested, fmt.Errorf("invalid video keyframe timestamp %q", invalidTimestamp)
	}
	return requested, errors.New("probe video keyframes returned no timestamp near requested start")
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

func preferredAudioStream(probe mediaProbe, preferredLanguages []string) (mediaStream, bool) {
	var audioStreams []mediaStream
	for _, stream := range probe.Streams {
		if stream.CodecType == "audio" {
			audioStreams = append(audioStreams, stream)
		}
	}
	for _, preferred := range preferredLanguages {
		preferred = canonicalLanguage(preferred)
		for _, stream := range audioStreams {
			if canonicalLanguage(stream.Tags.Language) == preferred || canonicalLanguage(stream.Tags.Title) == preferred {
				return stream, true
			}
		}
	}
	for _, stream := range audioStreams {
		if stream.Disposition.Default != 0 {
			return stream, true
		}
	}
	if len(audioStreams) > 0 {
		return audioStreams[0], true
	}
	return mediaStream{}, false
}

func supportedSubtitles(probe mediaProbe) []SubtitleTrack {
	var tracks []SubtitleTrack
	for _, stream := range probe.Streams {
		if stream.CodecType != "subtitle" {
			continue
		}
		kind := ""
		switch {
		case isTextSubtitleCodec(stream.CodecName):
			kind = "text"
		case isBitmapSubtitleCodec(stream.CodecName):
			kind = "bitmap"
		default:
			continue
		}
		tracks = append(tracks, SubtitleTrack{
			Index:    stream.Index,
			Language: canonicalLanguage(stream.Tags.Language),
			Title:    strings.TrimSpace(stream.Tags.Title),
			Default:  stream.Disposition.Default != 0,
			Forced:   stream.Disposition.Forced != 0,
			Codec:    strings.ToLower(stream.CodecName),
			Kind:     kind,
		})
	}
	return tracks
}

func hasTextSubtitle(tracks []SubtitleTrack, index int) bool {
	for _, track := range tracks {
		if track.Index == index && track.Kind == "text" {
			return true
		}
	}
	return false
}

func hasBitmapSubtitle(tracks []SubtitleTrack, index int) bool {
	for _, track := range tracks {
		if track.Index == index && track.Kind == "bitmap" {
			return true
		}
	}
	return false
}

func optionalIndex(index int) *int {
	if index < 0 {
		return nil
	}
	return &index
}

func isTextSubtitleCodec(codec string) bool {
	switch strings.ToLower(codec) {
	case "ass", "mov_text", "ssa", "subrip", "text", "webvtt":
		return true
	default:
		return false
	}
}

func isBitmapSubtitleCodec(codec string) bool {
	switch strings.ToLower(codec) {
	case "dvb_subtitle", "dvd_subtitle", "hdmv_pgs_subtitle", "xsub":
		return true
	default:
		return false
	}
}

func canonicalLanguage(language string) string {
	language = strings.ToLower(strings.TrimSpace(language))
	aliases := map[string]string{
		"english": "en", "french": "fr", "german": "de", "italian": "it",
		"japanese": "ja", "korean": "ko", "portuguese": "pt", "russian": "ru",
		"spanish": "es", "chinese": "zh",
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

func (m *Manager) ffmpegArgs(
	sourceURL string,
	dir string,
	codec string,
	startSeconds float64,
	audioStreamIndex int,
	bitmapSubtitleIndex int,
) []string {
	// Allow one segment beyond the target buffer to be packaged without rate limiting.
	// Stream-copied video can only cut on keyframes, so segment lengths may exceed the target.
	initialBurstSeconds := m.bufferSeconds + m.segmentSeconds
	args := []string{
		"-hide_banner", "-loglevel", "warning", "-nostdin", "-y",
		"-copyts", "-start_at_zero",
		"-readrate", strconv.FormatFloat(m.readRate, 'f', -1, 64),
		"-readrate_initial_burst", strconv.Itoa(initialBurstSeconds),
	}
	if startSeconds > 0 {
		// Preserve both streams' source timestamps through the keyframe seek, then shift
		// the shared timeline to zero. Resetting either stream independently can desync
		// stream-copied video from transcoded audio.
		args = append(args, "-noaccurate_seek", "-ss", strconv.FormatFloat(startSeconds, 'f', 3, 64))
	}
	audioMap := "0:a:0?"
	if audioStreamIndex >= 0 {
		audioMap = fmt.Sprintf("0:%d?", audioStreamIndex)
	}
	args = append(args, "-i", sourceURL)
	if bitmapSubtitleIndex >= 0 {
		args = append(args,
			"-filter_complex", fmt.Sprintf("[0:v:0][0:%d]overlay=eof_action=pass[v]", bitmapSubtitleIndex),
			"-map", "[v]", "-map", audioMap, "-sn", "-dn",
		)
		args = append(args, m.bitmapVideoEncoderArgs()...)
		args = append(args,
			"-force_key_frames", fmt.Sprintf(
				"expr:if(isnan(prev_forced_t),1,gte(t,prev_forced_t+%d))",
				m.segmentSeconds,
			),
		)
	} else {
		args = append(args,
			"-map", "0:v:0", "-map", audioMap, "-sn", "-dn",
			"-c:v", "copy",
		)
		if codec == "hevc" {
			args = append(args, "-tag:v", "hvc1")
		}
	}
	args = append(args,
		"-c:a", "aac", "-b:a", "256k", "-ac", "2",
	)
	if startSeconds > 0 {
		args = append(args,
			"-output_ts_offset", strconv.FormatFloat(-startSeconds, 'f', 3, 64),
		)
	}
	args = append(args,
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

func (m *Manager) bitmapVideoEncoderArgs() []string {
	if m.bitmapSubtitleEncoder == "h264_nvenc" {
		return []string{
			"-c:v", "h264_nvenc", "-preset", "p5", "-tune", "hq",
			"-rc", "vbr", "-cq", "18", "-b:v", "0", "-maxrate", "16M", "-bufsize", "32M",
			"-profile:v", "high", "-pix_fmt", "yuv420p",
		}
	}
	return []string{
		"-c:v", "libx264", "-preset", "veryfast", "-crf", "18",
		"-profile:v", "high", "-pix_fmt", "yuv420p",
	}
}

func (m *Manager) subtitleArgs(stream *runningStream, index int) []string {
	initialBurstSeconds := m.bufferSeconds + m.segmentSeconds
	args := []string{
		"-hide_banner", "-loglevel", "warning", "-nostdin", "-y",
		"-readrate", strconv.FormatFloat(m.readRate, 'f', -1, 64),
		"-readrate_initial_burst", strconv.Itoa(initialBurstSeconds),
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
	nextProgress := time.Now().Add(startupProgressInterval)
	for {
		if playlistReady(stream.dir, m.bufferSeconds) {
			return nil
		}
		if time.Now().After(nextProgress) {
			nextProgress = time.Now().Add(startupProgressInterval)
			segments, seconds, complete := playlistStatus(stream.dir)
			m.logger.Info("HLS startup buffering", "playback_id", stream.info.PlaybackID,
				"packaged_segments", segments, "packaged_seconds", seconds,
				"target_seconds", m.bufferSeconds, "complete", complete)
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

func (m *Manager) waitUntilBuffered(ctx context.Context, stream *runningStream, minimumSeconds int) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	nextProgress := time.Now().Add(startupProgressInterval)
	for {
		if playlistReady(stream.dir, minimumSeconds) {
			return nil
		}
		if time.Now().After(nextProgress) {
			nextProgress = time.Now().Add(startupProgressInterval)
			segments, seconds, complete := playlistStatus(stream.dir)
			m.logger.Info("HLS prewarm buffering", "playback_id", stream.info.PlaybackID,
				"packaged_segments", segments, "packaged_seconds", seconds,
				"target_seconds", minimumSeconds, "complete", complete)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for HLS prewarm buffer: %w", ctx.Err())
		case <-stream.done:
			if playlistReady(stream.dir, minimumSeconds) {
				return nil
			}
			stream.errMu.RLock()
			err := stream.err
			stream.errMu.RUnlock()
			return fmt.Errorf("FFmpeg exited before HLS prewarm completed: %v", err)
		case <-ticker.C:
		}
	}
}

func (m *Manager) waitForPlaylistGrowth(
	parent context.Context,
	stream *runningStream,
	initialSegments int,
) error {
	ctx, cancel := context.WithTimeout(parent, m.parkedResumeTimeout)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		segments, _, complete := playlistStatus(stream.dir)
		if segments > initialSegments || complete {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-stream.done:
			segments, _, complete = playlistStatus(stream.dir)
			if segments > initialSegments || complete {
				return nil
			}
			return errors.New("HLS packager stopped without extending the playlist")
		case <-ticker.C:
		}
	}
}

func playlistReady(dir string, minimumSeconds int) bool {
	segmentCount, duration, complete := playlistStatus(dir)
	if segmentCount == 0 || (!complete && duration < float64(minimumSeconds)) {
		return false
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "segment-*.m4s"))
	return len(matches) >= segmentCount
}

func playlistStatus(dir string) (segments int, duration float64, complete bool) {
	playlist, err := os.ReadFile(filepath.Join(dir, "index.m3u8"))
	if err != nil {
		return 0, 0, false
	}
	for _, line := range strings.Split(string(playlist), "\n") {
		switch {
		case strings.HasPrefix(line, "#EXTINF:"):
			segments++
			value := strings.TrimSuffix(strings.TrimPrefix(line, "#EXTINF:"), ",")
			if seconds, err := strconv.ParseFloat(value, 64); err == nil && seconds > 0 {
				duration += seconds
			}
		case line == "#EXT-X-ENDLIST":
			complete = true
		}
	}
	return segments, duration, complete
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

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func defaultString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
