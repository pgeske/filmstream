package hls

import (
	"context"
	"errors"
	"fmt"
	"time"
)

var ErrProducerStopped = errors.New("HLS producer stopped")

// Status describes packaging, not torrent membership or HTTP-reader count.
// Buffering is an observation, not proof that a release should be quarantined.
//
// States: starting (the startup buffer is not published yet), ready,
// buffering (the packager is alive but has produced no segment for the stall
// window), parked, complete, and failed.
type Status struct {
	State            string  `json:"state"`
	PackagedSegments int     `json:"packaged_segments"`
	PackagedSeconds  float64 `json:"packaged_seconds"`
	Complete         bool    `json:"complete"`
	// TargetSeconds is the startup buffer the stream must package before
	// playback starts.
	TargetSeconds        int     `json:"target_seconds"`
	SecondsSinceProgress float64 `json:"seconds_since_progress"`
	// Throttled reports that the packager is paused because it is far ahead
	// of the client's playhead.
	Throttled bool   `json:"throttled"`
	Error     string `json:"error,omitempty"`
}

func (m *Manager) Status(playbackID string) (Status, bool) {
	m.mu.RLock()
	stream := m.streams[playbackID]
	m.mu.RUnlock()
	if stream == nil {
		return Status{}, false
	}
	snapshot := stream.observe()
	stream.processMu.Lock()
	parked, throttled := stream.parked, stream.throttled
	stream.processMu.Unlock()
	sinceProgress := time.Since(snapshot.lastProgress)
	status := Status{
		State:                "ready",
		PackagedSegments:     len(snapshot.segmentEnds),
		PackagedSeconds:      snapshot.seconds(),
		Complete:             snapshot.complete,
		TargetSeconds:        m.bufferSeconds,
		SecondsSinceProgress: sinceProgress.Seconds(),
		Throttled:            throttled,
	}
	producerErr := stream.producerError()
	switch {
	case producerErr != nil:
		status.State = "failed"
		status.Error = producerErr.Error()
	case snapshot.complete:
		status.State = "complete"
	case parked:
		status.State = "parked"
	case !stream.published.Load():
		status.State = "starting"
	case !throttled && sinceProgress >= m.stallWindow:
		status.State = "buffering"
	}
	return status, true
}

func (stream *runningStream) producerError() error {
	if cause := context.Cause(stream.ctx); cause != nil {
		return fmt.Errorf("%w: %w", ErrProducerStopped, cause)
	}
	select {
	case <-stream.done:
		stream.errMu.RLock()
		err := stream.err
		stream.errMu.RUnlock()
		if err != nil {
			return fmt.Errorf("%w: %w", ErrProducerStopped, err)
		}
		if segments, _, complete := playlistStatus(stream.dir); !complete || segments == 0 {
			return fmt.Errorf("%w without a final playlist", ErrProducerStopped)
		}
	default:
	}
	return nil
}
