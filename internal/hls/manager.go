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
	"sync/atomic"
	"syscall"
	"time"
)

const (
	defaultStartupTimeout       = 90 * time.Second
	defaultStartupBufferSeconds = 8
	defaultSegmentSeconds       = 4
	defaultParkedTTL            = 2 * time.Hour
	// A live packager that has produced no new segment for this long is
	// reported as buffering. A stall alone never fails a playback.
	defaultStallWindow = 45 * time.Second
	// The packager pauses once it is this far ahead of the client's playhead
	// and continues below the lower mark. This bounds disk use without pacing
	// source reads, so a recovering torrent refills the client buffer at full speed.
	maxLeadSeconds    = 600.0
	resumeLeadSeconds = 540.0
	// A resume request this far beyond the packaged edge waits for the running
	// packager instead of restarting it at a new seek point.
	joinReachSeconds          = 60.0
	monitorInterval           = 500 * time.Millisecond
	startupProgressInterval   = 5 * time.Second
	timelineTimestampEpsilon  = 1e-6
	sourceVideoAnchorFileName = "source-video-anchor.framehash"
	// Torrent reads legitimately block while pieces arrive. A generous read
	// timeout with reconnects surfaces a hung connection without failing slow
	// pieces; FFmpeg gives up only after several consecutive timed-out reconnects.
	sourceReadTimeout              = 60 * time.Second
	sourceReconnectDelayMaxSeconds = 16
	encoderProbeTimeout            = 30 * time.Second
)

// ErrBuffering reports that a live packager has not yet produced the requested
// media within the startup timeout. The stream keeps running; retrying joins it.
var ErrBuffering = errors.New("HLS stream is still buffering")

// errSourceEndedEarly marks a packager that FFmpeg finished cleanly after its
// source connection failed. FFmpeg then writes a final playlist for truncated media.
var errSourceEndedEarly = errors.New("source read failed before the end of the media")

type Config struct {
	DataDir               string
	FFmpegPath            string
	FFprobePath           string
	SourceBaseURL         string
	StartupTimeout        time.Duration
	BufferSeconds         int
	SegmentSeconds        int
	ParkedTTL             time.Duration
	Logger                *slog.Logger
	BitmapSubtitleEncoder string
	LocalSourcePath       func(string) (string, bool)
	SourceUnavailable     func(string) error
}

type Stream struct {
	PlaybackID              string          `json:"playback_id"`
	RequestedStartSeconds   float64         `json:"requested_start_seconds"`
	TimelineOriginSeconds   float64         `json:"start_seconds"`
	PlayerTimeOffsetSeconds float64         `json:"player_time_offset_seconds"`
	DurationSeconds         float64         `json:"duration_seconds,omitempty"`
	VideoCodec              string          `json:"video_codec"`
	Subtitles               []SubtitleTrack `json:"subtitles"`
	BurnedSubtitleIndex     *int            `json:"burned_subtitle_index,omitempty"`
	AudioTracks             []AudioTrack    `json:"audio_tracks,omitempty"`
	AudioStreamIndex        *int            `json:"audio_stream_index,omitempty"`
}

type SubtitleTrack struct {
	Index    int    `json:"index"`
	Language string `json:"language,omitempty"`
	Title    string `json:"title,omitempty"`
	Default  bool   `json:"default,omitempty"`
	Forced   bool   `json:"forced,omitempty"`
	Codec    string `json:"codec,omitempty"`
	Kind     string `json:"kind,omitempty"`
	// RenditionName is the unique NAME of the track's subtitle rendition in
	// master.m3u8. Bitmap tracks have no rendition.
	RenditionName string `json:"rendition_name,omitempty"`
}

// AudioTrack describes one source audio stream offered to clients. The
// packaged HLS stream carries a single audio rendition chosen from this list.
type AudioTrack struct {
	Index    int    `json:"index"`
	Language string `json:"language,omitempty"`
	Title    string `json:"title,omitempty"`
	Channels int    `json:"channels,omitempty"`
	Default  bool   `json:"default,omitempty"`
}

// Asset is one servable HLS resource: a file written by the packager, or
// generated playlist and subtitle segment bytes.
type Asset struct {
	Path        string
	Content     []byte
	ContentType string
}

type Manager struct {
	dataDir               string
	ffmpegPath            string
	ffprobePath           string
	sourceBaseURL         string
	startupTimeout        time.Duration
	bufferSeconds         int
	segmentSeconds        int
	parkedTTL             time.Duration
	stallWindow           time.Duration
	logger                *slog.Logger
	bitmapSubtitleEncoder string
	localSourcePath       func(string) (string, bool)
	sourceUnavailable     func(string) error

	ctx         context.Context
	cancel      context.CancelFunc
	mu          sync.RWMutex
	streams     map[string]*runningStream
	playbacks   map[string]*playbackLifetime
	probeMu     sync.Mutex
	probes      map[string]*playbackProbe
	probeWG     sync.WaitGroup
	probeClosed bool
	closeOnce   sync.Once
}

// A Stop retires this generation, including unpublished and queued work. A
// replay gets a new lifetime; old callbacks may only clean up their own files.
type playbackLifetime struct {
	ctx     context.Context
	cancel  context.CancelFunc
	startMu sync.Mutex
}

type playbackProbe struct {
	lifetime *playbackLifetime
	done     chan struct{}
	cancel   context.CancelFunc
	probe    mediaProbe
	err      error
}

// runningStream is one packager process. It is registered as soon as FFmpeg
// starts so status can report startup progress, but its assets stay private
// until the packaged timeline has been verified and the stream is published.
type runningStream struct {
	lifetime  *playbackLifetime
	dir       string
	ctx       context.Context
	cancel    context.CancelFunc
	done      chan struct{}
	errMu     sync.RWMutex
	err       error
	command   *exec.Cmd
	startedAt time.Time
	stopOnce  sync.Once
	published atomic.Bool

	// Immutable after launch.
	requestedSeconds    float64
	languages           []string
	bitmapSubtitleIndex int
	audioStreamIndex    int
	preferredAudioIndex int
	bandwidth           int

	// Written by the Start that publishes the stream, read-only afterwards.
	info     Stream
	timeline playbackTimeline

	processMu sync.Mutex
	parked    bool
	parkedAt  time.Time
	parkTimer *time.Timer
	throttled bool
	stopped   bool

	progressMu      sync.Mutex
	segmentEnds     []float64
	complete        bool
	lastProgress    time.Time
	playheadSeconds float64
}

type mediaProbe struct {
	Streams []mediaStream `json:"streams"`
	Format  struct {
		StartTime string `json:"start_time"`
		Duration  string `json:"duration"`
		BitRate   string `json:"bit_rate"`
	} `json:"format"`
}

// playbackTimeline maps source media time, packaged HLS time, and player time.
// Media time is the source clock after FFmpeg's start_at_zero shift; the text
// subtitle outputs use the same clock.
type playbackTimeline struct {
	requestedSeconds float64
	seekSeconds      float64
	// The first packaged video frame, in media time and in packaged time.
	sourceVideoPTSSeconds   float64
	packagedVideoPTSSeconds float64
	originSeconds           float64
	playerTimeOffsetSeconds float64
}

type sourceVideoAnchor struct {
	ptsSeconds float64
	packetSize int
	packetHash string
}

func newPlaybackTimeline(requestedSeconds float64) playbackTimeline {
	return playbackTimeline{
		requestedSeconds: requestedSeconds,
		seekSeconds:      requestedSeconds,
	}
}

// alignToPackagedVideo derives the public timeline from the first packaged
// video frame. The recorded source packet is only a cross-check: the MP4 muxer
// rewrites Annex-B packets from MPEG-TS sources, and a demuxer without a seek
// index can land after the requested time. Both are reported, never fatal.
func (t *playbackTimeline) alignToPackagedVideo(
	anchor sourceVideoAnchor,
	anchorErr error,
	packaged packagedTimelineStart,
	streamCopiedVideo bool,
) []string {
	var warnings []string
	sourcePTS := anchor.ptsSeconds
	if anchorErr != nil {
		warnings = append(warnings, fmt.Sprintf(
			"source video anchor unavailable (%v); assuming it matches the seek position", anchorErr))
		sourcePTS = t.seekSeconds
	} else {
		if streamCopiedVideo && (anchor.packetSize != packaged.videoPacketSize ||
			anchor.packetHash != packaged.videoPacketHash) {
			warnings = append(warnings,
				"packaged first video packet differs from the recorded source packet; using measured timestamps")
		}
		// FFmpeg receives the seek rounded to milliseconds.
		if t.seekSeconds > 0 && sourcePTS > math.Round(t.seekSeconds*1000)/1000+timelineTimestampEpsilon {
			warnings = append(warnings, fmt.Sprintf(
				"source seek landed at %.6f after the requested %.6f", sourcePTS, t.seekSeconds))
		}
	}
	t.sourceVideoPTSSeconds = sourcePTS
	t.packagedVideoPTSSeconds = packaged.videoPTS
	if t.requestedSeconds == 0 {
		t.originSeconds = 0
		t.playerTimeOffsetSeconds = 0
		return warnings
	}
	origin := sourcePTS - packaged.videoPTS
	if math.IsNaN(origin) || math.IsInf(origin, 0) {
		origin = t.seekSeconds
	}
	if origin < 0 {
		// B-frame decode timestamps can put HLS zero just before media time zero.
		// The public full-media timeline is intentionally bounded at zero.
		origin = 0
	}
	t.originSeconds = origin
	// AVPlayer normalizes the first playlist presentation timestamp to player
	// time zero. Adding this offset restores the packaged HLS packet clock.
	t.playerTimeOffsetSeconds = sourcePTS - origin
	return warnings
}

func (t playbackTimeline) playerSecondsForMedia(mediaSeconds float64) float64 {
	return mediaSeconds - t.originSeconds - t.playerTimeOffsetSeconds
}

type packagedTimelineProbe struct {
	Packets []struct {
		StreamIndex int    `json:"stream_index"`
		PTSTime     string `json:"pts_time"`
		DTSTime     string `json:"dts_time"`
		Size        string `json:"size"`
		Flags       string `json:"flags"`
		Hash        string `json:"data_hash"`
	} `json:"packets"`
	Streams []struct {
		Index     int    `json:"index"`
		CodecType string `json:"codec_type"`
		StartTime string `json:"start_time"`
	} `json:"streams"`
}

type packagedTimelineStart struct {
	videoPTS        float64
	videoDTS        float64
	videoPacketSize int
	videoPacketHash string
	audioPTS        float64
}

type mediaStream struct {
	Index        int    `json:"index"`
	CodecName    string `json:"codec_name"`
	CodecType    string `json:"codec_type"`
	Channels     int    `json:"channels"`
	AvgFrameRate string `json:"avg_frame_rate"`
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
	if cfg.ParkedTTL <= 0 {
		cfg.ParkedTTL = defaultParkedTTL
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
	if cfg.BitmapSubtitleEncoder != "libx264" {
		if err := probeVideoEncoder(ffmpegPath, cfg.BitmapSubtitleEncoder); err != nil {
			cfg.Logger.Warn("bitmap subtitle encoder is unavailable; falling back to libx264",
				"encoder", cfg.BitmapSubtitleEncoder, "error", err)
			cfg.BitmapSubtitleEncoder = "libx264"
		}
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
		segmentSeconds:        cfg.SegmentSeconds,
		parkedTTL:             cfg.ParkedTTL,
		stallWindow:           defaultStallWindow,
		logger:                cfg.Logger,
		bitmapSubtitleEncoder: cfg.BitmapSubtitleEncoder,
		localSourcePath:       cfg.LocalSourcePath,
		sourceUnavailable:     cfg.SourceUnavailable,
		ctx:                   ctx,
		cancel:                cancel,
		streams:               make(map[string]*runningStream),
		playbacks:             make(map[string]*playbackLifetime),
		probes:                make(map[string]*playbackProbe),
	}, nil
}

// probeVideoEncoder encodes one tiny frame, proving the encoder and any GPU it
// needs are usable before a playback depends on them.
func probeVideoEncoder(ffmpegPath, encoder string) error {
	ctx, cancel := context.WithTimeout(context.Background(), encoderProbeTimeout)
	defer cancel()
	output, err := exec.CommandContext(ctx, ffmpegPath,
		"-hide_banner", "-loglevel", "error", "-nostdin",
		"-f", "lavfi", "-i", "color=c=black:s=256x144:r=24:d=0.2",
		"-frames:v", "1", "-c:v", encoder, "-f", "null", "-",
	).CombinedOutput()
	if err != nil {
		if details := strings.TrimSpace(string(output)); details != "" {
			return fmt.Errorf("%w: %s", err, details)
		}
		return err
	}
	return nil
}

func (m *Manager) lifetimeForPlayback(playbackID string) *playbackLifetime {
	m.mu.Lock()
	defer m.mu.Unlock()
	lifetime := m.playbacks[playbackID]
	if lifetime == nil {
		ctx, cancel := context.WithCancel(m.ctx)
		lifetime = &playbackLifetime{ctx: ctx, cancel: cancel}
		m.playbacks[playbackID] = lifetime
	}
	return lifetime
}

func (m *Manager) lockPlaybackStart(parent context.Context, playbackID string) (context.Context, *playbackLifetime, func()) {
	lifetime := m.lifetimeForPlayback(playbackID)
	ctx, cancel := context.WithCancel(parent)
	stop := context.AfterFunc(lifetime.ctx, cancel)
	lifetime.startMu.Lock()
	if lifetime.ctx.Err() != nil {
		cancel()
	}
	return ctx, lifetime, func() {
		lifetime.startMu.Unlock()
		stop()
		cancel()
	}
}

func (m *Manager) ProbeSubtitles(ctx context.Context, playbackID string) ([]SubtitleTrack, error) {
	if !validPlaybackID(playbackID) {
		return nil, errors.New("invalid playback ID")
	}
	lifetime := m.lifetimeForPlayback(playbackID)
	probeSource, _ := m.sourceForProbe(playbackID, m.sourceURL(playbackID))
	probe, err := m.probePlayback(ctx, playbackID, probeSource, lifetime)
	if err != nil {
		return nil, m.preferSourceUnavailable(playbackID, err)
	}
	return supportedSubtitles(probe), nil
}

func (m *Manager) sourceURL(playbackID string) string {
	return m.sourceBaseURL + "/v1/playbacks/" + playbackID + "/stream"
}

func (m *Manager) sourceForProbe(playbackID, sourceURL string) (string, bool) {
	if m.localSourcePath != nil {
		if path, ok := m.localSourcePath(playbackID); ok && path != "" {
			return path, true
		}
	}
	return sourceURL, false
}

// Start returns a playable stream at startSeconds. A running packager with the
// same selections is joined when it covers or will soon reach the position;
// otherwise a new packager starts there. Only hard failures (FFmpeg errors,
// unsupported media, an unavailable source) are errors; a slow source returns
// ErrBuffering and keeps packaging.
func (m *Manager) Start(
	ctx context.Context,
	playbackID string,
	startSeconds float64,
	preferredLanguages []string,
	bitmapSubtitleIndex int,
	audioStreamIndex int,
) (Stream, error) {
	if !validPlaybackID(playbackID) {
		return Stream{}, errors.New("invalid playback ID")
	}
	if startSeconds < 0 {
		return Stream{}, errors.New("start_seconds cannot be negative")
	}
	ctx, lifetime, unlockStart := m.lockPlaybackStart(ctx, playbackID)
	defer unlockStart()
	if err := ctx.Err(); err != nil {
		return Stream{}, err
	}
	startupContext, startupCancel := context.WithTimeout(ctx, m.startupTimeout)
	defer startupCancel()

	if stream := m.currentStream(playbackID, lifetime); stream != nil {
		if m.joinable(playbackID, stream, startSeconds, preferredLanguages, bitmapSubtitleIndex, audioStreamIndex) {
			return m.join(ctx, startupContext, playbackID, stream, startSeconds)
		}
		m.stopPlaybackStream(playbackID, lifetime)
	}
	stream, err := m.launch(
		startupContext, playbackID, lifetime, startSeconds, preferredLanguages, bitmapSubtitleIndex, audioStreamIndex,
	)
	if err != nil {
		return Stream{}, err
	}
	return m.join(ctx, startupContext, playbackID, stream, startSeconds)
}

func (m *Manager) currentStream(playbackID string, lifetime *playbackLifetime) *runningStream {
	m.mu.RLock()
	defer m.mu.RUnlock()
	stream := m.streams[playbackID]
	if stream == nil || stream.lifetime != lifetime {
		return nil
	}
	return stream
}

// matches reports whether the stream packages the requested selections. An
// audio index of -1 asks for the preferred track, which the stream may already carry.
func (stream *runningStream) matches(languages []string, bitmapSubtitleIndex, audioStreamIndex int) bool {
	if !equalStrings(stream.languages, languages) || stream.bitmapSubtitleIndex != bitmapSubtitleIndex {
		return false
	}
	if audioStreamIndex < 0 {
		return stream.audioStreamIndex == stream.preferredAudioIndex
	}
	return stream.audioStreamIndex == audioStreamIndex
}

func (m *Manager) joinable(
	playbackID string,
	stream *runningStream,
	startSeconds float64,
	preferredLanguages []string,
	bitmapSubtitleIndex int,
	audioStreamIndex int,
) bool {
	if !stream.matches(preferredLanguages, bitmapSubtitleIndex, audioStreamIndex) {
		return false
	}
	if err := stream.producerError(); err != nil {
		m.logger.Info("HLS packager stopped; rebuilding", "playback_id", playbackID,
			"requested_start_seconds", startSeconds, "error", err)
		return false
	}
	if math.Abs(stream.requestedSeconds-startSeconds) <= 0.5 {
		return true
	}
	if !stream.published.Load() {
		return false
	}
	// The event playlist retains every packaged segment, so a position inside
	// the packaged range, or shortly past it, keeps the running packager and its
	// buffer instead of re-probing and re-seeking the source.
	position := stream.timeline.playerSecondsForMedia(startSeconds)
	snapshot := stream.observe()
	limit := snapshot.seconds() + joinReachSeconds
	if snapshot.complete {
		limit = snapshot.seconds()
	}
	if position < 0 || position > limit {
		m.logger.Info("prepared HLS stream does not cover requested position; rebuilding",
			"playback_id", playbackID, "requested_start_seconds", startSeconds,
			"timeline_origin_seconds", stream.info.TimelineOriginSeconds,
			"packaged_seconds", snapshot.seconds())
		return false
	}
	return true
}

// join waits for a stream to cover startSeconds. An unpublished stream (only
// joined for its own start position) needs its startup buffer; a published
// stream needs the buffer past the requested position.
func (m *Manager) join(
	parent context.Context,
	startupContext context.Context,
	playbackID string,
	stream *runningStream,
	startSeconds float64,
) (Stream, error) {
	m.unpark(playbackID, stream)
	if !stream.published.Load() {
		if err := m.waitForMedia(startupContext, stream, float64(m.bufferSeconds), "startup"); err != nil {
			return Stream{}, m.joinFailure(parent, playbackID, stream, err, false)
		}
		if err := m.publish(startupContext, playbackID, stream); err != nil {
			return Stream{}, m.joinFailure(parent, playbackID, stream, err, false)
		}
		return stream.info, nil
	}
	position := max(0, stream.timeline.playerSecondsForMedia(startSeconds))
	stream.setPlayhead(position)
	m.applyThrottle(playbackID, stream)
	if err := m.waitForMedia(startupContext, stream, position+float64(m.bufferSeconds), "resume"); err != nil {
		packaged := stream.observe().seconds()
		if errors.Is(err, context.DeadlineExceeded) && parent.Err() == nil && stream.producerError() == nil &&
			packaged > position {
			// The position is packaged; let the client play what exists while the
			// source catches up rather than failing a working release.
			m.logger.Warn("resuming HLS stream before its buffer refilled",
				"playback_id", playbackID, "requested_start_seconds", startSeconds,
				"packaged_seconds", packaged)
			return stream.info, nil
		}
		return Stream{}, m.joinFailure(parent, playbackID, stream, err, true)
	}
	m.logger.Info("reused prepared HLS stream", "playback_id", playbackID,
		"requested_start_seconds", startSeconds,
		"timeline_origin_seconds", stream.info.TimelineOriginSeconds,
		"packaged_seconds", stream.observe().seconds())
	return stream.info, nil
}

// joinFailure classifies a failed wait. A waiter that goes away leaves the
// shared packager alone; a slow source keeps packaging; only a stopped
// packager is a failure.
func (m *Manager) joinFailure(parent context.Context, playbackID string, stream *runningStream, err error, published bool) error {
	if parentErr := parent.Err(); parentErr != nil {
		return parentErr
	}
	if stream.lifetime.ctx.Err() != nil {
		return context.Canceled
	}
	if producerErr := stream.producerError(); producerErr != nil {
		m.stopPlaybackStream(playbackID, stream.lifetime)
		m.forgetProbe(playbackID, stream.lifetime)
		return m.preferSourceUnavailable(playbackID, producerErr)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		snapshot := stream.observe()
		m.logger.Warn("HLS stream is still buffering", "playback_id", playbackID,
			"published", published, "packaged_segments", len(snapshot.segmentEnds),
			"packaged_seconds", snapshot.seconds(),
			"seconds_since_progress", time.Since(snapshot.lastProgress).Seconds())
		return fmt.Errorf("%w: %.1f seconds packaged", ErrBuffering, snapshot.seconds())
	}
	// The packaged output could not be verified; keeping it would serve a
	// timeline the client cannot map.
	m.stopPlaybackStream(playbackID, stream.lifetime)
	m.forgetProbe(playbackID, stream.lifetime)
	return err
}

// launch probes the source and starts a packager, registered but unpublished.
func (m *Manager) launch(
	ctx context.Context,
	playbackID string,
	lifetime *playbackLifetime,
	startSeconds float64,
	preferredLanguages []string,
	bitmapSubtitleIndex int,
	audioStreamIndex int,
) (*runningStream, error) {
	sourceURL := m.sourceURL(playbackID)
	probeSource, _ := m.sourceForProbe(playbackID, sourceURL)
	probe, err := m.probePlayback(ctx, playbackID, probeSource, lifetime)
	if err != nil {
		return nil, m.preferSourceUnavailable(playbackID, err)
	}
	video, err := compatibleVideo(probe)
	if err != nil {
		return nil, err
	}
	preferredAudioIndex := -1
	if audio, found := preferredAudioStream(probe, preferredLanguages); found {
		preferredAudioIndex = audio.Index
	}
	audioIndex := preferredAudioIndex
	if audioStreamIndex >= 0 {
		if !hasAudioStream(probe, audioStreamIndex) {
			return nil, fmt.Errorf("audio track %d is unavailable", audioStreamIndex)
		}
		audioIndex = audioStreamIndex
	}
	audioLanguage := ""
	for _, stream := range probe.Streams {
		if stream.Index == audioIndex {
			audioLanguage = canonicalLanguage(stream.Tags.Language)
		}
	}
	subtitles := supportedSubtitles(probe)
	if bitmapSubtitleIndex >= 0 && !hasBitmapSubtitle(subtitles, bitmapSubtitleIndex) {
		return nil, fmt.Errorf("bitmap subtitle track %d is unavailable", bitmapSubtitleIndex)
	}
	var textSubtitles []int
	for _, track := range subtitles {
		if track.Kind == "text" {
			textSubtitles = append(textSubtitles, track.Index)
		}
	}
	outputCodec := video.codec
	if bitmapSubtitleIndex >= 0 {
		outputCodec = "h264"
	}

	// A retired generation can still be reaping children while a replay starts.
	// Never let its cleanup touch the replacement's assets.
	dir, err := os.MkdirTemp(m.dataDir, playbackID+"-")
	if err != nil {
		return nil, fmt.Errorf("create playback HLS directory: %w", err)
	}
	logPath := filepath.Join(dir, "ffmpeg.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("create FFmpeg log: %w", err)
	}

	streamContext, cancelSource := m.monitorSource(lifetime.ctx, playbackID)
	timeline := newPlaybackTimeline(startSeconds)
	args := m.ffmpegArgs(sourceURL, dir, packagerPlan{
		codec:               video.codec,
		frameRate:           video.frameRate,
		timeline:            timeline,
		audioStreamIndex:    audioIndex,
		bitmapSubtitleIndex: bitmapSubtitleIndex,
		textSubtitles:       textSubtitles,
	})
	command := exec.CommandContext(streamContext, m.ffmpegPath, args...)
	command.Stdout = logFile
	command.Stderr = logFile
	now := time.Now()
	stream := &runningStream{
		lifetime:            lifetime,
		dir:                 dir,
		ctx:                 streamContext,
		cancel:              func() { cancelSource(context.Canceled) },
		done:                make(chan struct{}),
		command:             command,
		startedAt:           now,
		requestedSeconds:    startSeconds,
		languages:           append([]string(nil), preferredLanguages...),
		bitmapSubtitleIndex: bitmapSubtitleIndex,
		audioStreamIndex:    audioIndex,
		preferredAudioIndex: preferredAudioIndex,
		bandwidth:           estimatedBandwidth(probe),
		timeline:            timeline,
		lastProgress:        now,
		info: Stream{
			PlaybackID: playbackID, RequestedStartSeconds: startSeconds,
			DurationSeconds: video.duration, VideoCodec: outputCodec, Subtitles: subtitles,
			BurnedSubtitleIndex: optionalIndex(bitmapSubtitleIndex),
			AudioTracks:         supportedAudioTracks(probe),
			AudioStreamIndex:    optionalIndex(audioIndex),
		},
	}
	if err := command.Start(); err != nil {
		stream.cancel()
		logFile.Close()
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("start FFmpeg: %w", err)
	}
	go func() {
		err := command.Wait()
		_ = logFile.Close()
		if err == nil && sourceEndedEarly(logPath) {
			err = errSourceEndedEarly
		}
		stream.errMu.Lock()
		stream.err = err
		stream.errMu.Unlock()
		close(stream.done)
		if streamContext.Err() == nil {
			snapshot := stream.observe()
			if err != nil || !snapshot.complete {
				m.logger.Warn("HLS packager stopped", "playback_id", playbackID, "error", err,
					"packaged_segments", len(snapshot.segmentEnds), "packaged_seconds", snapshot.seconds(),
					"complete", snapshot.complete, "details", tailFile(logPath, 4096))
			}
		}
	}()
	m.mu.Lock()
	if m.playbacks[playbackID] != lifetime || lifetime.ctx.Err() != nil {
		m.mu.Unlock()
		stream.cancel()
		<-stream.done
		_ = os.RemoveAll(dir)
		return nil, context.Canceled
	}
	m.streams[playbackID] = stream
	m.mu.Unlock()
	m.logger.Info("HLS packager started", "playback_id", playbackID, "codec", outputCodec,
		"audio_stream_index", audioIndex, "audio_language", audioLanguage,
		"text_subtitle_tracks", len(textSubtitles), "burned_subtitle_index", bitmapSubtitleIndex,
		"requested_start_seconds", startSeconds)
	go m.monitorStream(playbackID, stream)
	return stream, nil
}

// publish verifies the packaged timeline and exposes the stream's assets.
func (m *Manager) publish(ctx context.Context, playbackID string, stream *runningStream) error {
	timeline := stream.timeline
	anchor, anchorErr := readPackagerSourceAnchor(filepath.Join(stream.dir, sourceVideoAnchorFileName))
	packagedStart, err := m.probePackagedTimelineStart(ctx, stream.dir)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return fmt.Errorf("verify packaged HLS timeline: %w", err)
	}
	for _, warning := range timeline.alignToPackagedVideo(anchor, anchorErr, packagedStart, stream.bitmapSubtitleIndex < 0) {
		m.logger.Warn("HLS timeline check", "playback_id", playbackID, "warning", warning)
	}
	if err := stream.producerError(); err != nil {
		return err
	}
	stream.timeline = timeline
	stream.info.TimelineOriginSeconds = timeline.originSeconds
	stream.info.PlayerTimeOffsetSeconds = timeline.playerTimeOffsetSeconds
	m.mu.Lock()
	if m.streams[playbackID] != stream || m.playbacks[playbackID] != stream.lifetime || stream.lifetime.ctx.Err() != nil {
		m.mu.Unlock()
		return context.Canceled
	}
	if err := ctx.Err(); err != nil {
		m.mu.Unlock()
		return err
	}
	stream.published.Store(true)
	m.mu.Unlock()
	snapshot := stream.observe()
	m.logger.Info("HLS stream ready", "playback_id", playbackID, "codec", stream.info.VideoCodec,
		"startup_seconds", time.Since(stream.startedAt).Seconds(),
		"packaged_seconds", snapshot.seconds(),
		"requested_start_seconds", timeline.requestedSeconds,
		"source_seek_seconds", timeline.seekSeconds,
		"source_video_pts_seconds", timeline.sourceVideoPTSSeconds,
		"timeline_origin_seconds", timeline.originSeconds,
		"player_time_offset_seconds", timeline.playerTimeOffsetSeconds,
		"hls_first_video_pts", packagedStart.videoPTS,
		"hls_first_video_dts", packagedStart.videoDTS,
		"hls_first_audio_pts", packagedStart.audioPTS)
	return nil
}

func (m *Manager) Prepared(
	playbackID string,
	startSeconds float64,
	preferredLanguages []string,
	bitmapSubtitleIndex int,
	audioStreamIndex int,
	minimumSeconds int,
) bool {
	m.mu.RLock()
	stream := m.streams[playbackID]
	m.mu.RUnlock()
	if stream == nil || !stream.published.Load() || math.Abs(stream.requestedSeconds-startSeconds) > 0.5 ||
		!stream.matches(preferredLanguages, bitmapSubtitleIndex, audioStreamIndex) {
		return false
	}
	if minimumSeconds <= 0 {
		minimumSeconds = m.bufferSeconds
	}
	return stream.producerError() == nil && playlistReady(stream.dir, float64(minimumSeconds))
}

func (m *Manager) Park(ctx context.Context, playbackID string, minimumSeconds int) error {
	ctx, lifetime, unlockStart := m.lockPlaybackStart(ctx, playbackID)
	defer unlockStart()
	if err := ctx.Err(); err != nil {
		return err
	}
	stream := m.currentStream(playbackID, lifetime)
	if stream == nil || !stream.published.Load() {
		return os.ErrNotExist
	}
	if minimumSeconds <= 0 {
		minimumSeconds = m.bufferSeconds
	}
	startTime := time.Now()
	if err := m.waitForMedia(ctx, stream, float64(minimumSeconds), "prewarm"); err != nil {
		return err
	}
	bufferWait := time.Since(startTime)

	stream.processMu.Lock()
	defer stream.processMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if m.currentStream(playbackID, lifetime) != stream {
		return os.ErrNotExist
	}
	select {
	case <-stream.done:
		return stream.producerError()
	default:
	}
	if stream.parked {
		return nil
	}
	// Keep the same FFmpeg process and source connection so resuming preserves
	// A/V timestamps while avoiding any additional download or packaging work.
	stream.parked = true
	if err := stream.applyProcessStateLocked(); err != nil {
		stream.parked = false
		return fmt.Errorf("park HLS packager: %w", err)
	}
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

func (m *Manager) unpark(playbackID string, stream *runningStream) {
	stream.processMu.Lock()
	defer stream.processMu.Unlock()
	if !stream.parked {
		return
	}
	stream.parked = false
	stream.parkedAt = time.Time{}
	if stream.parkTimer != nil {
		stream.parkTimer.Stop()
		stream.parkTimer = nil
	}
	if err := stream.applyProcessStateLocked(); err != nil {
		m.logger.Warn("resume prepared HLS stream", "playback_id", playbackID, "error", err)
		return
	}
	m.logger.Info("resumed prepared HLS stream", "playback_id", playbackID)
}

// applyProcessStateLocked stops FFmpeg while it is parked or throttled and
// continues it otherwise. The caller holds processMu.
func (stream *runningStream) applyProcessStateLocked() error {
	want := stream.parked || stream.throttled
	if want == stream.stopped {
		return nil
	}
	select {
	case <-stream.done:
		stream.stopped = false
		return nil
	default:
	}
	signal := syscall.SIGCONT
	if want {
		signal = syscall.SIGSTOP
	}
	if err := stream.command.Process.Signal(signal); err != nil {
		return err
	}
	stream.stopped = want
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

// monitorStream samples packaging progress and pauses FFmpeg while it is far
// ahead of the client, until the packager exits or is stopped.
func (m *Manager) monitorStream(playbackID string, stream *runningStream) {
	ticker := time.NewTicker(monitorInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stream.ctx.Done():
			return
		case <-stream.done:
			return
		case <-ticker.C:
		}
		before := stream.segmentCount()
		snapshot := stream.observe()
		if before == 0 && len(snapshot.segmentEnds) > 0 {
			m.logger.Info("HLS first segment packaged", "playback_id", playbackID,
				"seconds_since_start", time.Since(stream.startedAt).Seconds())
		}
		m.applyThrottle(playbackID, stream)
	}
}

func (m *Manager) applyThrottle(playbackID string, stream *runningStream) {
	snapshot := stream.observe()
	lead := snapshot.seconds() - snapshot.playheadSeconds
	stream.processMu.Lock()
	defer stream.processMu.Unlock()
	throttle := stream.throttled
	switch {
	case snapshot.complete:
		throttle = false
	case !throttle && lead > maxLeadSeconds:
		throttle = true
	case throttle && lead < resumeLeadSeconds:
		throttle = false
	}
	if throttle == stream.throttled {
		return
	}
	stream.throttled = throttle
	if err := stream.applyProcessStateLocked(); err != nil {
		stream.throttled = !throttle
		return
	}
	m.logger.Info("HLS packager pacing", "playback_id", playbackID, "paused", throttle,
		"packaged_seconds", snapshot.seconds(), "playhead_seconds", snapshot.playheadSeconds)
}

// packagingSnapshot is one reading of the packager's event playlist.
type packagingSnapshot struct {
	segmentEnds     []float64
	complete        bool
	lastProgress    time.Time
	playheadSeconds float64
}

func (snapshot packagingSnapshot) seconds() float64 {
	if len(snapshot.segmentEnds) == 0 {
		return 0
	}
	return snapshot.segmentEnds[len(snapshot.segmentEnds)-1]
}

// observe reads the event playlist and records when it last gained a segment.
func (stream *runningStream) observe() packagingSnapshot {
	playlist, _ := readMediaPlaylist(stream.dir)
	stream.progressMu.Lock()
	defer stream.progressMu.Unlock()
	if len(playlist.durations) > len(stream.segmentEnds) {
		ends := make([]float64, len(playlist.durations))
		total := 0.0
		for index, duration := range playlist.durations {
			total += duration
			ends[index] = total
		}
		stream.segmentEnds = ends
		stream.lastProgress = time.Now()
	}
	stream.complete = stream.complete || playlist.complete
	return packagingSnapshot{
		segmentEnds:     stream.segmentEnds,
		complete:        stream.complete,
		lastProgress:    stream.lastProgress,
		playheadSeconds: stream.playheadSeconds,
	}
}

func (stream *runningStream) segmentCount() int {
	stream.progressMu.Lock()
	defer stream.progressMu.Unlock()
	return len(stream.segmentEnds)
}

func (stream *runningStream) setPlayhead(seconds float64) {
	stream.progressMu.Lock()
	stream.playheadSeconds = seconds
	stream.progressMu.Unlock()
}

// noteSegmentRequest moves the playhead to the end of a segment the client fetched.
func (stream *runningStream) noteSegmentRequest(number int) {
	snapshot := stream.observe()
	if number < 0 || number >= len(snapshot.segmentEnds) {
		return
	}
	stream.setPlayhead(snapshot.segmentEnds[number])
}

// StartSubtitle validates a text track for clients that fetch the full WebVTT
// file. Every text track is already extracted by the packager in lockstep with
// the video, so there is nothing to start.
func (m *Manager) StartSubtitle(_ context.Context, playbackID string, index int) error {
	if !validPlaybackID(playbackID) || index < 0 {
		return os.ErrNotExist
	}
	m.mu.RLock()
	stream := m.streams[playbackID]
	m.mu.RUnlock()
	if stream == nil || !stream.published.Load() || !hasTextSubtitle(stream.info.Subtitles, index) {
		return os.ErrNotExist
	}
	return nil
}

// Asset resolves one HLS resource. Playlists are generated so they carry the
// playback start hint and the native subtitle renditions.
func (m *Manager) Asset(playbackID, name string) (Asset, error) {
	asset, ok := parseAssetName(name)
	if !validPlaybackID(playbackID) || !ok {
		return Asset{}, os.ErrNotExist
	}
	m.mu.RLock()
	stream := m.streams[playbackID]
	m.mu.RUnlock()
	if stream == nil || !stream.published.Load() {
		return Asset{}, os.ErrNotExist
	}
	if asset.subtitleIndex >= 0 && !hasTextSubtitle(stream.info.Subtitles, asset.subtitleIndex) {
		return Asset{}, os.ErrNotExist
	}
	switch asset.kind {
	case assetMasterPlaylist:
		if err := stream.producerError(); err != nil {
			return Asset{}, err
		}
		return Asset{Content: masterPlaylist(stream.info.Subtitles, stream.bandwidth), ContentType: playlistContentType}, nil
	case assetMediaPlaylist:
		if err := stream.producerError(); err != nil {
			return Asset{}, err
		}
		playlist, err := os.ReadFile(filepath.Join(stream.dir, "index.m3u8"))
		if err != nil {
			return Asset{}, os.ErrNotExist
		}
		return Asset{Content: withPlaybackStart(playlist), ContentType: playlistContentType}, nil
	case assetSubtitlePlaylist:
		if err := stream.producerError(); err != nil {
			return Asset{}, err
		}
		playlist, final := stream.subtitleSource()
		return Asset{
			Content:     subtitlePlaylist(playlist, asset.subtitleIndex, final),
			ContentType: playlistContentType,
		}, nil
	case assetSubtitleSegment:
		playlist, final := stream.subtitleSource()
		start, end, ok := subtitleSegmentWindow(playlist, asset.sequence, final)
		if !ok {
			return Asset{}, os.ErrNotExist
		}
		contents, _ := os.ReadFile(filepath.Join(stream.dir, subtitleFileName(asset.subtitleIndex)))
		return Asset{
			Content: subtitleSegment(
				parseWebVTTCues(contents), start, end,
				stream.timeline.sourceVideoPTSSeconds, stream.timeline.packagedVideoPTSSeconds,
			),
			ContentType: webVTTContentType,
		}, nil
	case assetMediaSegment:
		stream.noteSegmentRequest(asset.sequence)
	}
	path := filepath.Join(stream.dir, name)
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return Asset{}, os.ErrNotExist
	}
	return Asset{Path: path, ContentType: asset.contentType()}, nil
}

// subtitleSource returns the video playlist that subtitle segments follow and
// whether it is final: the packager exited cleanly, so every cue is written.
func (stream *runningStream) subtitleSource() (mediaPlaylist, bool) {
	playlist, _ := readMediaPlaylist(stream.dir)
	final := false
	select {
	case <-stream.done:
		final = playlist.complete && stream.producerError() == nil
	default:
	}
	return playlist, final
}

func (m *Manager) Stop(playbackID string) {
	m.mu.Lock()
	lifetime := m.playbacks[playbackID]
	delete(m.playbacks, playbackID)
	stream := m.streams[playbackID]
	delete(m.streams, playbackID)
	if lifetime != nil {
		lifetime.cancel()
	}
	m.mu.Unlock()
	m.forgetProbe(playbackID, lifetime)
	if stream != nil {
		m.stopStream(playbackID, stream)
	}
	if lifetime != nil {
		// Cancellation reaches probes, unpublished startups, and queued requests.
		// Wait for their owner to finish, without blocking a new generation.
		lifetime.startMu.Lock()
		lifetime.startMu.Unlock()
		m.logger.Info("stopped HLS playback", "playback_id", playbackID)
	}
}

func (m *Manager) stopPlaybackStream(playbackID string, lifetime *playbackLifetime) {
	m.mu.Lock()
	stream := m.streams[playbackID]
	if stream != nil && stream.lifetime == lifetime {
		delete(m.streams, playbackID)
	} else {
		stream = nil
	}
	m.mu.Unlock()
	if stream != nil {
		m.stopStream(playbackID, stream)
	}
}

func (m *Manager) stopStream(playbackID string, stream *runningStream) {
	stream.stopOnce.Do(func() {
		stream.processMu.Lock()
		stream.parked = false
		stream.parkedAt = time.Time{}
		if stream.parkTimer != nil {
			stream.parkTimer.Stop()
			stream.parkTimer = nil
		}
		stream.processMu.Unlock()
		snapshot := stream.observe()
		m.logger.Info("removing HLS stream", "playback_id", playbackID,
			"packaged_segments", len(snapshot.segmentEnds), "packaged_seconds", snapshot.seconds(),
			"complete", snapshot.complete)
		stream.cancel()
		select {
		case <-stream.done:
		case <-time.After(5 * time.Second):
		}
		if err := os.RemoveAll(stream.dir); err != nil {
			m.logger.Warn("remove HLS stream", "playback_id", playbackID, "error", err)
		}
	})
}

func (m *Manager) Close() error {
	m.closeOnce.Do(func() {
		m.cancel()
		m.closeProbes()
		m.mu.RLock()
		ids := make([]string, 0, len(m.playbacks))
		for id := range m.playbacks {
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
	lifetime *playbackLifetime,
) (mediaProbe, error) {
	if err := parent.Err(); err != nil {
		return mediaProbe{}, err
	}

	m.probeMu.Lock()
	if lifetime.ctx.Err() != nil {
		m.probeMu.Unlock()
		return mediaProbe{}, lifetime.ctx.Err()
	}
	if m.probeClosed {
		m.probeMu.Unlock()
		return mediaProbe{}, errors.New("HLS manager is closed")
	}
	cached := m.probes[playbackID]
	if cached == nil || cached.lifetime != lifetime {
		probeContext, cancelSource := m.monitorSource(lifetime.ctx, playbackID)
		cancel := func() { cancelSource(context.Canceled) }
		cached = &playbackProbe{lifetime: lifetime, done: make(chan struct{}), cancel: cancel}
		m.probes[playbackID] = cached
		m.probeWG.Add(1)
		go m.runPlaybackProbe(probeContext, playbackID, sourceURL, cached)
	}
	m.probeMu.Unlock()

	select {
	case <-parent.Done():
		return mediaProbe{}, context.Cause(parent)
	case <-cached.done:
		return cached.probe, m.preferSourceUnavailable(playbackID, cached.err)
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
	if cause := context.Cause(ctx); cause != nil {
		err = cause
	}

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

func (m *Manager) forgetProbe(playbackID string, lifetime *playbackLifetime) {
	m.probeMu.Lock()
	cached := m.probes[playbackID]
	if cached != nil && cached.lifetime == lifetime {
		delete(m.probes, playbackID)
	} else {
		cached = nil
	}
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

// monitorSource cancels a probe or packager when the source engine reports the
// playback's source as unavailable. This is the engine's hard verdict; the
// packager never derives one from a stall.
func (m *Manager) monitorSource(parent context.Context, playbackID string) (context.Context, context.CancelCauseFunc) {
	ctx, cancel := context.WithCancelCause(parent)
	if m.sourceUnavailable == nil {
		return ctx, cancel
	}
	if err := m.sourceUnavailable(playbackID); err != nil {
		cancel(err)
		return ctx, cancel
	}
	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := m.sourceUnavailable(playbackID); err != nil {
					cancel(err)
					return
				}
			}
		}
	}()
	return ctx, cancel
}

func (m *Manager) preferSourceUnavailable(playbackID string, fallback error) error {
	// Retirement remains cancellation even if the source also becomes unavailable.
	// Otherwise an abandoned operation can invalidate a cache or trigger replacement.
	if errors.Is(fallback, context.Canceled) {
		return fallback
	}
	if m.sourceUnavailable != nil {
		if err := m.sourceUnavailable(playbackID); err != nil {
			return err
		}
	}
	return fallback
}

func (m *Manager) probe(parent context.Context, sourceURL string) (mediaProbe, error) {
	ctx, cancel := context.WithTimeout(parent, m.startupTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, m.ffprobePath,
		"-v", "error",
		// stream_side_data_list (all entries) is accepted by FFprobe 4.4 through
		// 8.x; the newer per-entry stream_side_data section is not.
		"-show_entries", "stream=index,codec_name,codec_type,channels,avg_frame_rate:stream_tags=language,title:stream_disposition=default,forced:stream_side_data_list:format=start_time,duration,bit_rate",
		"-of", "json",
		sourceURL,
	)
	output, err := command.Output()
	if err != nil {
		if cause := context.Cause(ctx); cause != nil {
			return mediaProbe{}, fmt.Errorf("probe playback media: %w", cause)
		}
		return mediaProbe{}, fmt.Errorf("probe playback media: %w", err)
	}
	var probe mediaProbe
	if err := json.Unmarshal(output, &probe); err != nil {
		return mediaProbe{}, fmt.Errorf("decode media probe: %w", err)
	}
	return probe, nil
}

// readPackagerSourceAnchor reads the first source video packet (or decoded
// frame, for burn-in) that the packager's own seek delivered.
func readPackagerSourceAnchor(path string) (sourceVideoAnchor, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return sourceVideoAnchor{}, fmt.Errorf("read source video packet: %w", err)
	}

	timeBase := 0.0
	var packetFields []string
	packetCount := 0
	for _, line := range strings.Split(string(contents), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#tb 0:") {
			parts := strings.Split(strings.TrimSpace(strings.TrimPrefix(line, "#tb 0:")), "/")
			if len(parts) != 2 {
				return sourceVideoAnchor{}, errors.New("invalid source video packet time base")
			}
			numerator, numeratorErr := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
			denominator, denominatorErr := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
			if numeratorErr != nil || denominatorErr != nil || denominator <= 0 ||
				math.IsNaN(numerator) || math.IsInf(numerator, 0) ||
				math.IsNaN(denominator) || math.IsInf(denominator, 0) {
				return sourceVideoAnchor{}, errors.New("invalid source video packet time base")
			}
			timeBase = numerator / denominator
			continue
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		packetCount++
		packetFields = strings.Split(line, ",")
	}
	if packetCount == 0 {
		return sourceVideoAnchor{}, errors.New("packager recorded no source video packet")
	}
	if packetCount != 1 {
		return sourceVideoAnchor{}, errors.New("packager recorded ambiguous source video packets")
	}
	if timeBase <= 0 || len(packetFields) != 6 {
		return sourceVideoAnchor{}, errors.New("invalid source video packet record")
	}

	packetPTS, err := strconv.ParseInt(strings.TrimSpace(packetFields[2]), 10, 64)
	if err != nil {
		return sourceVideoAnchor{}, errors.New("invalid source video packet timestamp")
	}
	packetSize, err := strconv.Atoi(strings.TrimSpace(packetFields[4]))
	if err != nil || packetSize <= 0 {
		return sourceVideoAnchor{}, errors.New("invalid source video packet size")
	}
	packetHash := strings.ToLower(strings.TrimSpace(packetFields[5]))
	if len(packetHash) != 64 {
		return sourceVideoAnchor{}, errors.New("invalid source video packet hash")
	}
	for _, character := range packetHash {
		if !strings.ContainsRune("0123456789abcdef", character) {
			return sourceVideoAnchor{}, errors.New("invalid source video packet hash")
		}
	}

	mediaPTS := float64(packetPTS) * timeBase
	if math.IsNaN(mediaPTS) || math.IsInf(mediaPTS, 0) || mediaPTS < -timelineTimestampEpsilon {
		return sourceVideoAnchor{}, errors.New("invalid source video packet timestamp")
	}
	if mediaPTS < 0 {
		mediaPTS = 0
	}
	return sourceVideoAnchor{
		ptsSeconds: mediaPTS,
		packetSize: packetSize,
		packetHash: "SHA256:" + packetHash,
	}, nil
}

func (m *Manager) probePackagedTimelineStart(
	parent context.Context,
	dir string,
) (packagedTimelineStart, error) {
	segments, err := filepath.Glob(filepath.Join(dir, "segment-*.m4s"))
	if err != nil || len(segments) == 0 {
		return packagedTimelineStart{}, errors.New("packaged HLS has no media segment")
	}
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	input := "concat:" + filepath.Join(dir, "init.mp4") + "|" + segments[0]
	command := exec.CommandContext(ctx, m.ffprobePath,
		"-v", "error",
		"-show_entries", "stream=index,codec_type,start_time:packet=stream_index,pts_time,dts_time,size,flags,data_hash",
		"-show_data_hash", "sha256",
		"-read_intervals", "%+#1",
		"-of", "json",
		input,
	)
	output, err := command.Output()
	if err != nil {
		if cause := context.Cause(ctx); cause != nil {
			return packagedTimelineStart{}, fmt.Errorf("probe first HLS segment: %w", cause)
		}
		return packagedTimelineStart{}, fmt.Errorf("probe first HLS segment: %w", err)
	}
	var probe packagedTimelineProbe
	if err := json.Unmarshal(output, &probe); err != nil {
		return packagedTimelineStart{}, fmt.Errorf("decode first HLS segment probe: %w", err)
	}

	videoIndex := -1
	start := packagedTimelineStart{audioPTS: -1}
	for _, stream := range probe.Streams {
		seconds, err := parseTimelineTimestamp(stream.StartTime)
		if err != nil {
			return packagedTimelineStart{}, fmt.Errorf(
				"invalid packaged %s start timestamp %q", stream.CodecType, stream.StartTime,
			)
		}
		switch stream.CodecType {
		case "video":
			videoIndex = stream.Index
			start.videoPTS = seconds
		case "audio":
			start.audioPTS = seconds
		}
	}
	if videoIndex < 0 {
		return packagedTimelineStart{}, errors.New("packaged HLS has no video stream")
	}
	if len(probe.Packets) == 0 || probe.Packets[0].StreamIndex != videoIndex ||
		!strings.Contains(probe.Packets[0].Flags, "K") {
		return packagedTimelineStart{}, errors.New("packaged HLS does not begin with a video keyframe")
	}
	packetPTS, err := parseTimelineTimestamp(probe.Packets[0].PTSTime)
	if err != nil || math.Abs(packetPTS-start.videoPTS) > timelineTimestampEpsilon {
		return packagedTimelineStart{}, errors.New("packaged HLS video start does not match its first packet")
	}
	// DTS precedes PTS when B-frames reorder, so it may be slightly negative.
	start.videoDTS, err = strconv.ParseFloat(strings.TrimSpace(probe.Packets[0].DTSTime), 64)
	if err != nil || math.IsNaN(start.videoDTS) || math.IsInf(start.videoDTS, 0) {
		return packagedTimelineStart{}, fmt.Errorf(
			"invalid packaged video DTS %q", probe.Packets[0].DTSTime,
		)
	}
	start.videoPacketSize, _ = strconv.Atoi(probe.Packets[0].Size)
	start.videoPacketHash = probe.Packets[0].Hash
	return start, nil
}

func parseTimelineTimestamp(value string) (float64, error) {
	seconds, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil || math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds < 0 {
		return 0, errors.New("invalid timeline timestamp")
	}
	return seconds, nil
}

type videoInfo struct {
	codec     string
	duration  float64
	frameRate float64
}

func compatibleVideo(probe mediaProbe) (videoInfo, error) {
	for _, stream := range probe.Streams {
		if stream.CodecType != "video" {
			continue
		}
		codec := strings.ToLower(stream.CodecName)
		if codec != "h264" && codec != "hevc" {
			return videoInfo{}, fmt.Errorf("video codec %q is not supported by native Apple playback", codec)
		}
		for _, sideData := range stream.SideDataList {
			name := strings.ToLower(sideData.SideDataType)
			if strings.Contains(name, "dovi") || strings.Contains(name, "dolby vision") {
				return videoInfo{}, errors.New("this Dolby Vision profile is not supported by native Apple playback")
			}
		}
		duration, _ := strconv.ParseFloat(probe.Format.Duration, 64)
		return videoInfo{codec: codec, duration: duration, frameRate: parseFrameRate(stream.AvgFrameRate)}, nil
	}
	return videoInfo{}, errors.New("playback has no video stream")
}

func parseFrameRate(value string) float64 {
	numerator, denominator, found := strings.Cut(value, "/")
	top, err := strconv.ParseFloat(numerator, 64)
	if err != nil {
		return 0
	}
	bottom := 1.0
	if found {
		if bottom, err = strconv.ParseFloat(denominator, 64); err != nil || bottom <= 0 {
			return 0
		}
	}
	rate := top / bottom
	if math.IsNaN(rate) || math.IsInf(rate, 0) || rate <= 0 || rate > 240 {
		return 0
	}
	return rate
}

// estimatedBandwidth is the master playlist's peak bits per second: the source
// bit rate with headroom, plus the packaged audio.
func estimatedBandwidth(probe mediaProbe) int {
	const fallback = 20_000_000
	bitRate, err := strconv.ParseFloat(probe.Format.BitRate, 64)
	if err != nil || bitRate <= 0 || math.IsInf(bitRate, 0) {
		return fallback
	}
	return int(bitRate*1.25) + 256_000
}

// isCommentaryAudioTrack reports whether the stream looks like a commentary
// or descriptive narration track rather than the feature dialogue audio.
func isCommentaryAudioTrack(stream mediaStream) bool {
	title := strings.ToLower(stream.Tags.Title)
	return strings.Contains(title, "comment") || strings.Contains(title, "descri") ||
		strings.Contains(title, "narrat")
}

// bestAudioStream prefers the default-flagged track, then the track with the
// most channels. Feature dialogue is usually 5.1 while commentary tracks are
// stereo, so channel count is a useful tiebreaker when tags are missing.
func bestAudioStream(streams []mediaStream) mediaStream {
	best := streams[0]
	for _, stream := range streams[1:] {
		switch {
		case stream.Disposition.Default != 0 && best.Disposition.Default == 0:
			best = stream
		case (stream.Disposition.Default != 0) == (best.Disposition.Default != 0) &&
			stream.Channels > best.Channels:
			best = stream
		}
	}
	return best
}

func preferredAudioStream(probe mediaProbe, preferredLanguages []string) (mediaStream, bool) {
	var audioStreams []mediaStream
	for _, stream := range probe.Streams {
		if stream.CodecType == "audio" {
			audioStreams = append(audioStreams, stream)
		}
	}
	if len(audioStreams) == 0 {
		return mediaStream{}, false
	}
	// Commentary and descriptive tracks are rarely the feature audio; only
	// consider them when the release carries nothing else. Language tags are
	// frequently missing or wrong on rips, so an English-tagged commentary
	// track must not win over an untagged main track.
	mainStreams := make([]mediaStream, 0, len(audioStreams))
	for _, stream := range audioStreams {
		if !isCommentaryAudioTrack(stream) {
			mainStreams = append(mainStreams, stream)
		}
	}
	if len(mainStreams) == 0 {
		mainStreams = audioStreams
	}
	for _, preferred := range preferredLanguages {
		preferred = canonicalLanguage(preferred)
		var matches []mediaStream
		for _, stream := range mainStreams {
			if canonicalLanguage(stream.Tags.Language) == preferred || canonicalLanguage(stream.Tags.Title) == preferred {
				matches = append(matches, stream)
			}
		}
		if len(matches) > 0 {
			return bestAudioStream(matches), true
		}
	}
	return bestAudioStream(mainStreams), true
}

func supportedAudioTracks(probe mediaProbe) []AudioTrack {
	var tracks []AudioTrack
	for _, stream := range probe.Streams {
		if stream.CodecType != "audio" {
			continue
		}
		tracks = append(tracks, AudioTrack{
			Index:    stream.Index,
			Language: canonicalLanguage(stream.Tags.Language),
			Title:    strings.TrimSpace(stream.Tags.Title),
			Channels: stream.Channels,
			Default:  stream.Disposition.Default != 0,
		})
	}
	return tracks
}

func hasAudioStream(probe mediaProbe, index int) bool {
	for _, stream := range probe.Streams {
		if stream.CodecType == "audio" && stream.Index == index {
			return true
		}
	}
	return false
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
	assignRenditionNames(tracks)
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

type packagerPlan struct {
	codec               string
	frameRate           float64
	timeline            playbackTimeline
	audioStreamIndex    int
	bitmapSubtitleIndex int
	textSubtitles       []int
}

// ffmpegArgs builds the single packager process: a source anchor record, one
// full WebVTT file per text subtitle track, and the fMP4 HLS output. Every
// output reads the same demuxed packets, so subtitles advance with the video.
// The HLS playlist is the last argument.
func (m *Manager) ffmpegArgs(sourceURL string, dir string, plan packagerPlan) []string {
	args := []string{
		"-hide_banner", "-loglevel", "warning", "-nostdin", "-y",
		"-copyts", "-start_at_zero",
	}
	if strings.HasPrefix(sourceURL, "http://") || strings.HasPrefix(sourceURL, "https://") {
		args = append(args,
			"-rw_timeout", strconv.FormatInt(sourceReadTimeout.Microseconds(), 10),
			"-reconnect", "1", "-reconnect_on_network_error", "1",
			"-reconnect_delay_max", strconv.Itoa(sourceReconnectDelayMaxSeconds),
		)
	}
	if plan.timeline.seekSeconds > 0 {
		// Preserve both streams' source timestamps through the keyframe seek, then shift
		// the shared timeline together. The final source-to-HLS origin is measured from
		// the packaged first video packet rather than inferred from these FFmpeg options.
		args = append(args, "-noaccurate_seek", "-ss", strconv.FormatFloat(plan.timeline.seekSeconds, 'f', 3, 64))
	}
	args = append(args, "-i", sourceURL)
	burnIn := plan.bitmapSubtitleIndex >= 0
	if burnIn {
		args = append(args,
			"-filter_complex", fmt.Sprintf(
				"[0:v:0][0:%d]overlay=eof_action=pass,"+
					"scale=w='min(1920,iw)':h='min(1080,ih)':force_original_aspect_ratio=decrease:force_divisible_by=2[v]",
				plan.bitmapSubtitleIndex,
			),
		)
	}
	// Record the first video delivered by this exact demux seek. Stream-copy
	// records the packet; bitmap burn-in records the first decoded frame
	// because the packaged packet is necessarily different.
	sourceAnchorCodec := "copy"
	if burnIn {
		sourceAnchorCodec = "rawvideo"
	}
	args = append(args,
		"-map", "0:v:0", "-c:v", sourceAnchorCodec, "-frames:v", "1",
		"-flush_packets", "1", "-hash", "sha256", "-f", "framehash",
		filepath.Join(dir, sourceVideoAnchorFileName),
	)
	for _, index := range plan.textSubtitles {
		args = append(args,
			"-map", fmt.Sprintf("0:%d", index), "-c:s", "webvtt",
			"-flush_packets", "1", "-f", "webvtt", filepath.Join(dir, subtitleFileName(index)),
		)
	}
	if burnIn {
		args = append(args, "-map", "[v]")
		args = append(args, m.bitmapVideoEncoderArgs(plan.frameRate)...)
	} else {
		args = append(args, "-map", "0:v:0", "-c:v", "copy")
		if plan.codec == "hevc" {
			args = append(args, "-tag:v", "hvc1")
		}
	}
	if plan.audioStreamIndex >= 0 {
		// Fill gaps and trim overlaps in source audio timestamps. Without this the
		// muxer stretches sample durations across a gap while AVPlayer plays the
		// samples back to back, so audio drifts ahead after every gap. first_pts=0
		// also pads a late audio start, but only without a seek: the filter sees
		// source timestamps under -copyts, so after a seek it would prepend the
		// entire skipped duration as silence.
		resample := "aresample=async=1"
		if plan.timeline.seekSeconds == 0 {
			resample += ":first_pts=0"
		}
		args = append(args,
			"-map", fmt.Sprintf("0:%d", plan.audioStreamIndex),
			"-c:a", "aac", "-b:a", "256k", "-ac", "2", "-af", resample,
		)
	}
	args = append(args, "-sn", "-dn")
	if plan.timeline.seekSeconds > 0 {
		args = append(args,
			"-output_ts_offset", strconv.FormatFloat(-plan.timeline.seekSeconds, 'f', 3, 64),
		)
	}
	return append(args,
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
}

// bitmapVideoEncoderArgs encodes burned-in video with an IDR frame at every
// segment boundary, so the HLS muxer can cut segments of the configured length.
func (m *Manager) bitmapVideoEncoderArgs(frameRate float64) []string {
	var args []string
	if m.bitmapSubtitleEncoder == "h264_nvenc" {
		// NVENC turns forced keyframes into non-IDR intra frames unless
		// forced-idr is set, and the HLS muxer only cuts on IDR frames.
		args = []string{
			"-c:v", "h264_nvenc", "-preset", "p5", "-tune", "hq",
			"-rc", "vbr", "-cq", "18", "-b:v", "0", "-maxrate", "16M", "-bufsize", "32M",
			"-profile:v", "high", "-pix_fmt", "yuv420p",
			"-forced-idr", "1", "-no-scenecut", "1",
		}
	} else {
		args = []string{
			"-c:v", "libx264", "-preset", "veryfast", "-crf", "18",
			"-profile:v", "high", "-pix_fmt", "yuv420p",
		}
	}
	if frameRate > 0 {
		args = append(args, "-g", strconv.Itoa(int(math.Ceil(frameRate*float64(m.segmentSeconds)))))
	}
	return append(args,
		"-force_key_frames", fmt.Sprintf(
			"expr:if(isnan(prev_forced_t),1,gte(t,prev_forced_t+%d))",
			m.segmentSeconds,
		),
	)
}

// sourceEndedEarly reports whether FFmpeg logged a source read failure. FFmpeg
// treats a failed demuxer read as the end of input and exits successfully with
// a final playlist, so the exit status alone cannot tell truncation from the end.
func sourceEndedEarly(logPath string) bool {
	contents, err := os.ReadFile(logPath)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(contents), "\n") {
		if strings.Contains(line, "Stream ends prematurely") ||
			(strings.Contains(line, "Input/output error") &&
				(strings.Contains(line, "Error during demuxing") || strings.Contains(line, "Error retrieving a packet"))) {
			return true
		}
	}
	return false
}

// waitForMedia waits until the packager has published media through
// endSeconds of player time, or its playlist is complete.
func (m *Manager) waitForMedia(ctx context.Context, stream *runningStream, endSeconds float64, stage string) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	nextProgress := time.Now().Add(startupProgressInterval)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := stream.producerError(); err != nil {
			return err
		}
		if playlistReady(stream.dir, endSeconds) {
			return nil
		}
		if time.Now().After(nextProgress) {
			nextProgress = time.Now().Add(startupProgressInterval)
			snapshot := stream.observe()
			m.logger.Info("HLS "+stage+" buffering", "playback_id", stream.info.PlaybackID,
				"packaged_segments", len(snapshot.segmentEnds), "packaged_seconds", snapshot.seconds(),
				"target_seconds", endSeconds, "complete", snapshot.complete)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for HLS %s buffer: %w", stage, context.Cause(ctx))
		case <-stream.done:
			// Recheck producer outcome before accepting its cached buffer.
		case <-ticker.C:
		}
	}
}

// playlistReady reports whether the playlist and its segment files cover
// minimumSeconds, or the complete media when it is shorter.
func playlistReady(dir string, minimumSeconds float64) bool {
	segmentCount, duration, complete := playlistStatus(dir)
	if segmentCount == 0 || (!complete && duration < minimumSeconds) {
		return false
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "segment-*.m4s"))
	return len(matches) >= segmentCount
}

func playlistStatus(dir string) (segments int, duration float64, complete bool) {
	playlist, _ := readMediaPlaylist(dir)
	for _, seconds := range playlist.durations {
		duration += seconds
	}
	return len(playlist.durations), duration, playlist.complete
}

type mediaPlaylist struct {
	durations      []float64
	targetDuration int
	complete       bool
}

func readMediaPlaylist(dir string) (mediaPlaylist, error) {
	contents, err := os.ReadFile(filepath.Join(dir, "index.m3u8"))
	if err != nil {
		return mediaPlaylist{}, err
	}
	var playlist mediaPlaylist
	for _, line := range strings.Split(string(contents), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "#EXTINF:"):
			value, _, _ := strings.Cut(strings.TrimPrefix(line, "#EXTINF:"), ",")
			seconds, err := strconv.ParseFloat(value, 64)
			if err != nil || seconds < 0 || math.IsNaN(seconds) || math.IsInf(seconds, 0) {
				seconds = 0
			}
			playlist.durations = append(playlist.durations, seconds)
		case strings.HasPrefix(line, "#EXT-X-TARGETDURATION:"):
			playlist.targetDuration, _ = strconv.Atoi(strings.TrimPrefix(line, "#EXT-X-TARGETDURATION:"))
		case line == "#EXT-X-ENDLIST":
			playlist.complete = true
		}
	}
	return playlist, nil
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
