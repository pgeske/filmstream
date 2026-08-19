package torrentstream

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
)

const managedTorrentStateVersion = 1

type managedTorrent struct {
	InfoHash        string    `json:"info_hash"`
	FileHint        string    `json:"file_hint,omitempty"`
	Started         bool      `json:"started"`
	SeededSeconds   float64   `json:"seeded_seconds,omitempty"`
	DownloadedBytes int64     `json:"downloaded_bytes,omitempty"`
	UploadedBytes   int64     `json:"uploaded_bytes,omitempty"`
	LastActivity    time.Time `json:"last_activity"`

	runtimeDownloaded int64
	runtimeUploaded   int64
	lastCountedAt     time.Time
}

type managedTorrentPayload struct {
	Version  int               `json:"version"`
	Torrents []*managedTorrent `json:"torrents"`
}

func (e *Engine) restoreManagedTorrents() error {
	contents, err := os.ReadFile(e.managedStatePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read managed torrent state: %w", err)
	}
	var payload managedTorrentPayload
	if err := json.Unmarshal(contents, &payload); err != nil {
		return fmt.Errorf("parse managed torrent state: %w", err)
	}
	if payload.Version != managedTorrentStateVersion {
		return fmt.Errorf("unsupported managed torrent state version %d", payload.Version)
	}

	for _, state := range payload.Torrents {
		if state == nil {
			return errors.New("managed torrent state contains a null entry")
		}
		if !validInfoHash(state.InfoHash) {
			return fmt.Errorf("managed torrent state contains invalid info hash %q", state.InfoHash)
		}
		e.lifecycleMu.Lock()
		err := e.restoreManagedTorrent(state, time.Now().UTC())
		e.lifecycleMu.Unlock()
		if err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) restoreManagedTorrent(state *managedTorrent, now time.Time) error {
	meta, err := metainfo.LoadFromFile(e.managedMetainfoPath(state.InfoHash))
	if err != nil {
		return fmt.Errorf("load managed torrent %s: %w", state.InfoHash, err)
	}
	t, err := e.client.AddTorrent(meta)
	if err != nil {
		return fmt.Errorf("restore managed torrent %s: %w", state.InfoHash, err)
	}
	if t.InfoHash().HexString() != state.InfoHash {
		t.Drop()
		return fmt.Errorf("managed torrent %s has mismatched metainfo", state.InfoHash)
	}
	file, err := selectVideoFile(t.Files(), state.FileHint)
	if err != nil && state.FileHint != "" {
		file, err = selectVideoFile(t.Files(), "")
	}
	if err != nil {
		t.Drop()
		return fmt.Errorf("restore managed torrent %s: %w", state.InfoHash, err)
	}
	id, err := randomID()
	if err != nil {
		t.Drop()
		return err
	}
	if state.LastActivity.IsZero() {
		state.LastActivity = now
	}
	stats := t.Stats()
	state.runtimeDownloaded = stats.BytesReadData.Int64()
	state.runtimeUploaded = stats.BytesWrittenData.Int64()
	if state.Started {
		state.lastCountedAt = now
	}

	e.mu.Lock()
	e.managed[t] = state
	for _, session := range e.sessions {
		if session.torrent == t {
			session.started = session.started || state.Started
			e.mu.Unlock()
			e.logger.Info("restored managed torrent state", "name", t.Name(),
				"seeded_hours", state.SeededSeconds/3600)
			return nil
		}
	}
	e.sessions[id] = &Session{
		ID: id, Name: t.Name(), FileName: file.DisplayPath(), FileSize: file.Length(), CreatedAt: now,
		torrent: t, file: file, started: state.Started, lastActivity: state.LastActivity,
	}
	e.mu.Unlock()
	e.logger.Info("restored managed torrent for seeding", "name", t.Name(),
		"seeded_hours", state.SeededSeconds/3600)
	return nil
}

func (e *Engine) ensureManagedTorrentLocked(t *torrent.Torrent, fileHint string, now time.Time) error {
	if state := e.managed[t]; state != nil {
		if state.FileHint == "" && fileHint != "" {
			state.FileHint = fileHint
			if err := e.persistManagedTorrentsLocked(); err != nil {
				e.logger.Warn("could not update managed torrent file hint", "error", err)
			}
		}
		return nil
	}

	infoHash := t.InfoHash().HexString()
	var contents bytes.Buffer
	meta := t.Metainfo()
	if err := meta.Write(&contents); err != nil {
		return fmt.Errorf("encode managed torrent metainfo: %w", err)
	}
	if err := os.MkdirAll(e.managedDir, 0o700); err != nil {
		return fmt.Errorf("create managed torrent directory: %w", err)
	}
	if err := writeManagedFile(e.managedMetainfoPath(infoHash), contents.Bytes()); err != nil {
		return fmt.Errorf("save managed torrent metainfo: %w", err)
	}
	stats := t.Stats()
	e.managed[t] = &managedTorrent{
		InfoHash: infoHash, FileHint: fileHint, LastActivity: now,
		runtimeDownloaded: stats.BytesReadData.Int64(),
		runtimeUploaded:   stats.BytesWrittenData.Int64(),
	}
	if err := e.persistManagedTorrentsLocked(); err != nil {
		delete(e.managed, t)
		_ = os.Remove(e.managedMetainfoPath(infoHash))
		return err
	}
	return nil
}

func (e *Engine) accountManagedTorrentLocked(t *torrent.Torrent, now time.Time) *managedTorrent {
	state := e.managed[t]
	if state == nil {
		return nil
	}
	stats := t.Stats()
	downloaded := stats.BytesReadData.Int64()
	uploaded := stats.BytesWrittenData.Int64()
	if downloaded >= state.runtimeDownloaded {
		state.DownloadedBytes += downloaded - state.runtimeDownloaded
	}
	if uploaded >= state.runtimeUploaded {
		state.UploadedBytes += uploaded - state.runtimeUploaded
	}
	state.runtimeDownloaded = downloaded
	state.runtimeUploaded = uploaded
	if state.Started {
		if !state.lastCountedAt.IsZero() && now.After(state.lastCountedAt) {
			state.SeededSeconds += now.Sub(state.lastCountedAt).Seconds()
		}
		state.lastCountedAt = now
	}
	return state
}

func (e *Engine) managedTransferTotalsLocked(t *torrent.Torrent, now time.Time) (downloaded, uploaded int64, seededFor time.Duration) {
	state := e.managed[t]
	if state == nil {
		stats := t.Stats()
		return stats.BytesReadData.Int64(), stats.BytesWrittenData.Int64(), 0
	}
	stats := t.Stats()
	downloaded = state.DownloadedBytes + max(0, stats.BytesReadData.Int64()-state.runtimeDownloaded)
	uploaded = state.UploadedBytes + max(0, stats.BytesWrittenData.Int64()-state.runtimeUploaded)
	seconds := state.SeededSeconds
	if state.Started && !state.lastCountedAt.IsZero() && now.After(state.lastCountedAt) {
		seconds += now.Sub(state.lastCountedAt).Seconds()
	}
	return downloaded, uploaded, time.Duration(seconds * float64(time.Second))
}

func (e *Engine) markManagedTorrentStartedLocked(t *torrent.Torrent, now time.Time) {
	state := e.managed[t]
	if state == nil {
		return
	}
	if !state.Started {
		state.Started = true
		state.lastCountedAt = now
	}
	state.LastActivity = now
}

func (e *Engine) updateManagedTorrentActivityLocked(t *torrent.Torrent, now time.Time) {
	if state := e.managed[t]; state != nil {
		state.LastActivity = now
	}
}

func (e *Engine) removeManagedTorrentLocked(t *torrent.Torrent) {
	state := e.managed[t]
	if state == nil {
		return
	}
	delete(e.managed, t)
	if err := os.Remove(e.managedMetainfoPath(state.InfoHash)); err != nil && !errors.Is(err, os.ErrNotExist) {
		e.logger.Warn("could not remove managed torrent metainfo", "error", err)
	}
}

func (e *Engine) persistManagedTorrentsLocked() error {
	states := make([]*managedTorrent, 0, len(e.managed))
	for _, state := range e.managed {
		copy := *state
		copy.runtimeDownloaded = 0
		copy.runtimeUploaded = 0
		copy.lastCountedAt = time.Time{}
		states = append(states, &copy)
	}
	sort.Slice(states, func(i, j int) bool { return states[i].InfoHash < states[j].InfoHash })
	if len(states) == 0 {
		if err := os.Remove(e.managedStatePath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove managed torrent state: %w", err)
		}
		return nil
	}
	payload := managedTorrentPayload{Version: managedTorrentStateVersion, Torrents: states}
	contents, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	contents = append(contents, '\n')
	if err := writeManagedFile(e.managedStatePath, contents); err != nil {
		return fmt.Errorf("save managed torrent state: %w", err)
	}
	return nil
}

func (e *Engine) managedMetainfoPath(infoHash string) string {
	return filepath.Join(e.managedDir, infoHash+".torrent")
}

func validInfoHash(value string) bool {
	if len(value) != 40 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func writeManagedFile(path string, contents []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".managed-torrent-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(contents); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}
