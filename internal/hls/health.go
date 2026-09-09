package hls

import (
	"context"
	"errors"
	"fmt"
)

var ErrProducerStopped = errors.New("HLS producer stopped")

// Status describes packaging, not torrent membership or HTTP-reader count.
// Buffering is an observation, not proof that a release should be quarantined.
type Status struct {
	State            string  `json:"state"`
	PackagedSegments int     `json:"packaged_segments"`
	PackagedSeconds  float64 `json:"packaged_seconds"`
	Complete         bool    `json:"complete"`
	Error            string  `json:"error,omitempty"`
}

func (m *Manager) Status(playbackID string) (Status, bool) {
	m.mu.RLock()
	stream := m.streams[playbackID]
	m.mu.RUnlock()
	if stream == nil {
		return Status{}, false
	}
	stream.processMu.Lock()
	defer stream.processMu.Unlock()
	segments, seconds, complete := playlistStatus(stream.dir)
	status := Status{
		State: "ready", PackagedSegments: segments, PackagedSeconds: seconds, Complete: complete,
	}
	producerErr := stream.producerError()
	switch {
	case producerErr != nil:
		status.State = "failed"
		status.Error = producerErr.Error()
	case complete:
		status.State = "complete"
	case stream.parked:
		status.State = "parked"
	case m.preparedStreamNeedsGrowth(stream, false, false):
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
			return fmt.Errorf("%w: %v", ErrProducerStopped, err)
		}
		if segments, _, complete := playlistStatus(stream.dir); !complete || segments == 0 {
			return fmt.Errorf("%w without a final playlist", ErrProducerStopped)
		}
	default:
	}
	return nil
}
