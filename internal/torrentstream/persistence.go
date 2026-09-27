package torrentstream

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/pgeske/filmstream/internal/config"
)

const managedTorrentStateVersion = 2

// managedTorrent is Filmstream's durable record of a torrent it added to
// Deluge. Deluge persists the torrent itself; this keeps what Filmstream needs
// to honour its seeding obligation across restarts.
type managedTorrent struct {
	InfoHash string `json:"info_hash"`
	Name     string `json:"name,omitempty"`
	// Indexer is the configured indexer the release came from.
	Indexer string           `json:"indexer,omitempty"`
	Private bool             `json:"private,omitempty"`
	Seed    *config.SeedRule `json:"seed_rule,omitempty"`
	// Files are the playback file indices selected so far.
	Files []int `json:"files,omitempty"`
	// Started is set by the first payload read of any playback.
	Started bool `json:"started"`
	// Obligated marks a private torrent that must be completed and seeded
	// until its rule is met, even if it was never played.
	Obligated bool `json:"obligated,omitempty"`
	// WantAll records that every file was requested from Deluge.
	WantAll      bool      `json:"want_all,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	LastActivity time.Time `json:"last_activity"`
}

type managedTorrentPayload struct {
	Version  int               `json:"version"`
	Torrents []json.RawMessage `json:"torrents"`
}

// loadManagedTorrents reads the persisted records. Unreadable entries are
// logged and kept verbatim so the next save cannot silently drop an obligation.
func (e *Engine) loadManagedTorrents() error {
	contents, err := os.ReadFile(e.managedStatePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read managed torrent state: %w", err)
	}
	var payload managedTorrentPayload
	if err := json.Unmarshal(contents, &payload); err != nil {
		// Never overwrite a state file we cannot read: set it aside.
		backup := fmt.Sprintf("%s.corrupt-%d", e.managedStatePath, time.Now().Unix())
		if renameErr := os.Rename(e.managedStatePath, backup); renameErr != nil {
			return fmt.Errorf("parse managed torrent state: %w (and could not set it aside: %v)", err, renameErr)
		}
		e.logger.Error("managed torrent state is corrupt; moved aside for manual recovery",
			"path", backup, "error", err)
		return nil
	}
	now := time.Now().UTC()
	for index, raw := range payload.Torrents {
		var record managedTorrent
		if err := json.Unmarshal(raw, &record); err != nil || !validInfoHash(record.InfoHash) {
			if err == nil {
				err = fmt.Errorf("invalid info hash %q", record.InfoHash)
			}
			e.logger.Error("could not restore managed torrent entry; keeping it for manual recovery",
				"index", index, "error", err)
			e.unreadableRecords = append(e.unreadableRecords, raw)
			continue
		}
		record.InfoHash = strings.ToLower(record.InfoHash)
		if payload.Version < 2 && record.Started {
			// Version 1 predates Deluge; its started torrents are completed
			// so any tracker obligation they carry is honoured.
			record.WantAll = true
		}
		if record.CreatedAt.IsZero() {
			record.CreatedAt = now
		}
		if record.LastActivity.IsZero() {
			record.LastActivity = now
		}
		recordCopy := record
		e.torrents[record.InfoHash] = &torrentState{hash: record.InfoHash, record: &recordCopy, restored: true}
	}
	return nil
}

// persistLocked writes every managed torrent record. Callers hold e.mu.
func (e *Engine) persistLocked() {
	records := make([]*managedTorrent, 0, len(e.torrents))
	for _, t := range e.torrents {
		if t.record != nil {
			records = append(records, t.record)
		}
	}
	sort.Slice(records, func(i, j int) bool { return records[i].InfoHash < records[j].InfoHash })
	entries := make([]json.RawMessage, 0, len(records)+len(e.unreadableRecords))
	for _, record := range records {
		encoded, err := json.Marshal(record)
		if err != nil {
			e.logger.Error("could not encode managed torrent state", "info_hash", record.InfoHash, "error", err)
			continue
		}
		entries = append(entries, encoded)
	}
	entries = append(entries, e.unreadableRecords...)
	if len(entries) == 0 {
		if err := os.Remove(e.managedStatePath); err != nil && !errors.Is(err, os.ErrNotExist) {
			e.logger.Warn("could not remove managed torrent state", "error", err)
		}
		return
	}
	contents, err := json.MarshalIndent(managedTorrentPayload{Version: managedTorrentStateVersion, Torrents: entries}, "", "  ")
	if err != nil {
		e.logger.Error("could not encode managed torrent state", "error", err)
		return
	}
	if err := writeManagedFile(e.managedStatePath, append(contents, '\n')); err != nil {
		e.logger.Warn("could not save managed torrent state", "error", err)
	}
}

func (e *Engine) managedMetainfoPath(infoHash string) string {
	return filepath.Join(e.managedDir, infoHash+".torrent")
}

// saveMetainfo keeps a copy of the .torrent so an obligation survives even if
// Deluge loses its own state.
func (e *Engine) saveMetainfo(infoHash string, contents []byte) {
	if len(contents) == 0 {
		return
	}
	path := e.managedMetainfoPath(infoHash)
	if _, err := os.Stat(path); err == nil {
		return
	}
	if err := writeManagedFile(path, contents); err != nil {
		e.logger.Warn("could not save torrent metainfo", "info_hash", infoHash, "error", err)
	}
}

func (e *Engine) removeMetainfo(infoHash string) {
	if err := os.Remove(e.managedMetainfoPath(infoHash)); err != nil && !errors.Is(err, os.ErrNotExist) {
		e.logger.Warn("could not remove torrent metainfo", "info_hash", infoHash, "error", err)
	}
}

func (r *managedTorrent) addFile(index int) bool {
	if slices.Contains(r.Files, index) {
		return false
	}
	r.Files = append(r.Files, index)
	slices.Sort(r.Files)
	return true
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
