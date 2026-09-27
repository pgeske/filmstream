package torrentstream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"
)

const (
	// The stream window ahead of a reader starts at minLookahead and grows
	// with the length of the current contiguous read up to maxLookahead, so a
	// long sequential packager read keeps a deep buffer of requested pieces.
	minLookahead = 64 << 20
	maxLookahead = 1 << 30
	// A quarter of the lookahead, within these bounds, gets staggered piece
	// deadlines so Deluge fetches it in reading order; the rest of the window
	// is only raised to top priority.
	minDeadlineSpan = 32 << 20
	maxDeadlineSpan = 256 << 20
	// streamWindowTTL lets Deluge drop the window of a reader that vanished;
	// live readers refresh it every streamWindowRefresh.
	streamWindowTTL     = 30 * time.Second
	streamWindowRefresh = 10 * time.Second
	minWindowStep       = 8 << 20
	// waitSlice bounds one long poll so stall checks run between polls.
	waitSlice         = 5 * time.Second
	maxBitfieldPieces = 4096
	pluginRetryDelay  = time.Second
)

// pieceReader reads a playback file from the downloads directory. Before
// returning bytes it makes sure Deluge verified the pieces covering them, and
// it keeps a stream window of prioritized pieces ahead of the read position.
type pieceReader struct {
	e       *Engine
	ctx     context.Context
	session *Session
	t       *torrentState
	file    pluginFile
	stream  string

	data *os.File
	pos  int64
	// readyStart..readyEnd (file offsets) are known to be verified.
	readyStart, readyEnd int64
	// runStart is where the current contiguous read began.
	runStart int64

	windowOffset int64
	windowLength int64
	windowAt     time.Time
	windowErrors int
}

func (e *Engine) newReader(ctx context.Context, session *Session) *pieceReader {
	return &pieceReader{
		e: e, ctx: ctx, session: session, t: session.torrent, file: session.file,
		stream: "read-" + session.ID + "-" + strconv.FormatUint(e.streamSeq.Add(1), 10),
	}
}

// prepare positions the reader and waits until the byte at start is readable.
func (r *pieceReader) prepare(start int64) error {
	if _, err := r.Seek(start, io.SeekStart); err != nil {
		return err
	}
	if start >= r.file.Size {
		return nil
	}
	return r.await(start)
}

func (r *pieceReader) Read(p []byte) (int, error) {
	if r.pos >= r.file.Size {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	if r.pos < r.readyStart || r.pos >= r.readyEnd {
		if err := r.await(r.pos); err != nil {
			return 0, err
		}
	} else {
		r.moveWindow(false)
	}
	if r.data == nil {
		data, err := os.Open(r.session.localPath)
		if err != nil {
			return 0, fmt.Errorf("open torrent data: %w", err)
		}
		r.data = data
	}
	n := min(int64(len(p)), r.readyEnd-r.pos)
	read, err := r.data.ReadAt(p[:n], r.pos)
	r.pos += int64(read)
	if errors.Is(err, io.EOF) {
		if read > 0 {
			return read, nil
		}
		return 0, fmt.Errorf("torrent data file %s is shorter than its verified pieces", r.session.localPath)
	}
	return read, err
}

func (r *pieceReader) Seek(offset int64, whence int) (int64, error) {
	var target int64
	switch whence {
	case io.SeekStart:
		target = offset
	case io.SeekCurrent:
		target = r.pos + offset
	case io.SeekEnd:
		target = r.file.Size + offset
	default:
		return 0, errors.New("invalid seek whence")
	}
	if target < 0 {
		return 0, errors.New("negative seek position")
	}
	if target != r.pos {
		r.runStart = target
	}
	r.pos = target
	return target, nil
}

// Close releases the reader's stream window so its pieces lose priority.
func (r *pieceReader) Close() error {
	if r.data != nil {
		_ = r.data.Close()
	}
	if r.windowAt.IsZero() {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), pluginCallTimeout)
	defer cancel()
	if err := r.e.plugin.removeWindow(ctx, r.t.hash, r.stream); err != nil && !errors.Is(err, errTorrentNotFound) {
		r.e.logger.Debug("could not remove stream window", "id", r.session.ID, "error", err)
	}
	return nil
}

func (r *pieceReader) lookahead() int64 {
	return min(max(r.pos-r.runStart, minLookahead), maxLookahead)
}

// moveWindow re-centres the stream window on the read position when the
// reader moved or the lookahead grew noticeably, and refreshes its TTL.
func (r *pieceReader) moveWindow(force bool) {
	if r.pos >= r.file.Size {
		return
	}
	lookahead := r.lookahead()
	now := time.Now()
	step := max(minWindowStep, r.windowLength/8)
	if !force && !r.windowAt.IsZero() && now.Sub(r.windowAt) < streamWindowRefresh &&
		r.pos >= r.windowOffset && r.pos-r.windowOffset < step && lookahead < r.windowLength*3/2 {
		return
	}
	r.windowAt = now
	err := r.e.plugin.setWindow(r.ctx, r.t.hash, r.stream, pluginWindow{
		File: r.file.Index, Offset: r.pos, Length: lookahead,
		DeadlineBytes: min(max(lookahead/4, minDeadlineSpan), maxDeadlineSpan),
		TTLMillis:     streamWindowTTL.Milliseconds(),
	})
	if err != nil {
		if r.ctx.Err() == nil && r.windowErrors%10 == 0 {
			r.e.logger.Warn("could not move stream window", "id", r.session.ID, "offset", r.pos, "error", err)
		}
		r.windowErrors++
		return
	}
	r.windowOffset, r.windowLength = r.pos, lookahead
}

// await blocks until the piece holding pos is verified, then records how far
// the verified run extends. It gives up when the torrent stops making
// progress (see checkStall) or the request ends.
func (r *pieceReader) await(pos int64) error {
	pieceLength := r.t.pieceLength
	piece := int((r.file.Offset + pos) / pieceLength)
	lastPiece := int((r.file.Offset + r.file.Size - 1) / pieceLength)
	r.moveWindow(r.windowAt.IsZero() || pos < r.windowOffset || pos >= r.windowOffset+r.windowLength)
	waitStarted := time.Now()
	// The first poll does not block, so a playback already judged unavailable
	// fails at once instead of after a long poll.
	timeout := time.Duration(0)
	for {
		lookaheadPieces := int(r.lookahead()/pieceLength) + 1
		through := min(lastPiece, piece+min(lookaheadPieces, maxBitfieldPieces-1))
		pieces, err := r.e.plugin.wait(r.ctx, r.t.hash, piece, through, timeout)
		if err == nil && pieces.has(piece) {
			last := piece
			for last < through && pieces.has(last+1) {
				last++
			}
			r.readyStart = max(0, int64(piece)*pieceLength-r.file.Offset)
			r.readyEnd = min(r.file.Size, int64(last+1)*pieceLength-r.file.Offset)
			if r.e.markServing(r.session) {
				// A played private torrent is snatched: complete and seed all of it.
				r.e.pushWantAll(r.ctx, r.t)
			}
			return nil
		}
		if r.ctx.Err() != nil {
			return r.ctx.Err()
		}
		if err != nil {
			if errors.Is(err, errTorrentNotFound) {
				return r.e.markUnavailable(r.session, errors.New("torrent was removed from Deluge"))
			}
			r.e.logger.Warn("waiting for torrent piece failed", "id", r.session.ID, "piece", piece, "error", err)
			timer := time.NewTimer(pluginRetryDelay)
			select {
			case <-r.ctx.Done():
				timer.Stop()
				return r.ctx.Err()
			case <-timer.C:
			}
		}
		if err := r.e.checkStall(r.ctx, r.session, waitStarted); err != nil {
			return err
		}
		r.moveWindow(false)
		timeout = r.e.waitTimeout(r.session)
	}
}

// waitTimeout is the next long-poll duration: waitSlice, shortened so a
// startup deadline is checked when it passes.
func (e *Engine) waitTimeout(session *Session) time.Duration {
	e.mu.Lock()
	defer e.mu.Unlock()
	if session.served || session.serveDeadline.IsZero() {
		return waitSlice
	}
	return min(waitSlice, max(50*time.Millisecond, time.Until(session.serveDeadline)))
}

// markServing records that the playback delivered data: its startup deadline
// and any unavailable verdict no longer apply. The first data served from a
// torrent marks it started, which makes it a seeding obligation; markServing
// reports whether that just happened to a private torrent.
func (e *Engine) markServing(session *Session) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	session.served = true
	session.serveDeadline = time.Time{}
	session.unavailable = nil
	session.unavailableUntil = time.Time{}
	record := session.torrent.record
	if record == nil || record.Started {
		return false
	}
	record.Started = true
	if record.Private {
		record.WantAll = true
	}
	e.persistLocked()
	return record.Private
}

// checkStall decides whether a blocked read should give up. A playback that
// never served data fails after PeerlessStartupWait without a connected peer.
// Any read fails once the torrent gained no verified data for StallTimeout.
// Both mark the playback unavailable for sourceUnavailableHold only.
func (e *Engine) checkStall(ctx context.Context, session *Session, waitStarted time.Time) error {
	if err := e.SourceUnavailable(session.ID); err != nil {
		return err
	}
	t := session.torrent
	detail, err := e.detail(ctx, t)
	if err != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	now := time.Now()
	e.mu.Lock()
	served := session.served
	if !served && session.serveDeadline.IsZero() {
		// Consecutive startup reads (probe, packager) share one deadline.
		session.serveDeadline = waitStarted.Add(e.peerlessWait)
	}
	deadline := session.serveDeadline
	e.mu.Unlock()

	if !served && err == nil && detail.NumPeers == 0 && now.After(deadline) {
		percent := int64(0)
		if session.file.Index < len(detail.FileProgress) && session.FileSize > 0 {
			percent = 100 * detail.FileProgress[session.file.Index] / session.FileSize
		}
		return e.markUnavailable(session, fmt.Errorf("no peers connected within %s and only %d%% of %s is available locally",
			e.peerlessWait, percent, session.FileName))
	}
	progressAt := e.lastProgress(t)
	if progressAt.Before(waitStarted) {
		progressAt = waitStarted
	}
	if stalled := now.Sub(progressAt); stalled >= e.stallTimeout {
		return e.markUnavailable(session, fmt.Errorf("torrent downloaded nothing for %s with %d peers connected",
			stalled.Round(time.Second), detail.NumPeers))
	}
	return nil
}
