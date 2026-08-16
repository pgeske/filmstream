package playbackcache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/pgeske/filmstream/internal/catalog"
)

type Entry struct {
	ID          string                  `json:"id"`
	MediaID     string                  `json:"media_id,omitempty"`
	Title       string                  `json:"title"`
	Year        int                     `json:"year,omitempty"`
	Selected    catalog.RankedCandidate `json:"selected"`
	UpdatedAt   time.Time               `json:"updated_at"`
	TorrentPath string                  `json:"-"`
}

type UsenetEntry struct {
	ID        string                  `json:"id"`
	MediaID   string                  `json:"media_id,omitempty"`
	Title     string                  `json:"title"`
	Year      int                     `json:"year,omitempty"`
	Selected  catalog.RankedCandidate `json:"selected"`
	UpdatedAt time.Time               `json:"updated_at"`
	NZBPath   string                  `json:"-"`
}

type Store struct {
	path               string
	torrentDir         string
	usenetPath         string
	usenetDir          string
	usenetFailuresPath string
	mu                 sync.Mutex
}

func New(stateDir string) *Store {
	return &Store{
		path:               filepath.Join(stateDir, "playback-cache.json"),
		torrentDir:         filepath.Join(stateDir, "playback-cache"),
		usenetPath:         filepath.Join(stateDir, "usenet-playback-cache.json"),
		usenetDir:          filepath.Join(stateDir, "playback-cache"),
		usenetFailuresPath: filepath.Join(stateDir, "usenet-failures.json"),
	}
}

func (s *Store) Lookup(mediaID, title string, year int) (Entry, bool, error) {
	mediaID = strings.TrimSpace(mediaID)
	title = strings.TrimSpace(title)

	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := s.read()
	if err != nil {
		return Entry{}, false, err
	}
	index := -1
	if mediaID != "" {
		for i := range entries {
			if entries[i].MediaID == mediaID {
				index = i
				break
			}
		}
	}
	if index < 0 {
		for i := range entries {
			if (mediaID == "" || entries[i].MediaID == "") &&
				entries[i].Year == year && strings.EqualFold(entries[i].Title, title) {
				index = i
				break
			}
		}
	}
	if index < 0 {
		return Entry{}, false, nil
	}
	entry := entries[index]
	if !validID(entry.ID) {
		return Entry{}, false, fmt.Errorf("playback cache contains invalid entry ID %q", entry.ID)
	}
	entry.TorrentPath = filepath.Join(s.torrentDir, entry.ID+".torrent")
	if _, err := os.Stat(entry.TorrentPath); errors.Is(err, os.ErrNotExist) {
		return Entry{}, false, nil
	} else if err != nil {
		return Entry{}, false, fmt.Errorf("inspect cached torrent: %w", err)
	}
	return entry, true, nil
}

func (s *Store) Remove(mediaID, title string, year int) error {
	mediaID = strings.TrimSpace(mediaID)
	title = strings.TrimSpace(title)

	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := s.read()
	if err != nil {
		return err
	}
	for i := range entries {
		matchingMediaID := mediaID != "" && entries[i].MediaID == mediaID
		matchingLegacyTitle := (mediaID == "" || entries[i].MediaID == "") &&
			entries[i].Year == year && strings.EqualFold(entries[i].Title, title)
		if !matchingMediaID && !matchingLegacyTitle {
			continue
		}
		if validID(entries[i].ID) {
			path := filepath.Join(s.torrentDir, entries[i].ID+".torrent")
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("remove cached torrent: %w", err)
			}
		}
		entries = append(entries[:i], entries[i+1:]...)
		return s.write(entries)
	}
	return nil
}

func (s *Store) Save(
	mediaID, title string,
	year int,
	selected catalog.RankedCandidate,
	metainfo []byte,
) (Entry, error) {
	mediaID = strings.TrimSpace(mediaID)
	title = strings.TrimSpace(title)
	if title == "" {
		return Entry{}, errors.New("cached playback title cannot be empty")
	}
	if len(metainfo) == 0 {
		return Entry{}, errors.New("cached torrent metainfo cannot be empty")
	}
	selected.Candidate.MagnetURI = ""
	selected.Candidate.TorrentURL = ""

	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := s.read()
	if err != nil {
		return Entry{}, err
	}
	id := cacheID(mediaID, title, year)
	index := -1
	for i := range entries {
		matchingMediaID := mediaID != "" && entries[i].MediaID == mediaID
		matchingLegacyTitle := (mediaID == "" || entries[i].MediaID == "") &&
			entries[i].Year == year && strings.EqualFold(entries[i].Title, title)
		if matchingMediaID || matchingLegacyTitle {
			id = entries[i].ID
			index = i
			break
		}
	}
	if !validID(id) {
		return Entry{}, fmt.Errorf("invalid playback cache ID %q", id)
	}
	if err := os.MkdirAll(s.torrentDir, 0o700); err != nil {
		return Entry{}, fmt.Errorf("create playback cache directory: %w", err)
	}
	torrentPath := filepath.Join(s.torrentDir, id+".torrent")
	if err := writePrivateFile(torrentPath, metainfo); err != nil {
		return Entry{}, fmt.Errorf("write cached torrent: %w", err)
	}
	entry := Entry{
		ID: id, MediaID: mediaID, Title: title, Year: year,
		Selected: selected, UpdatedAt: time.Now().UTC(), TorrentPath: torrentPath,
	}
	if index >= 0 {
		entries[index] = entry
	} else {
		entries = append(entries, entry)
	}
	if err := s.write(entries); err != nil {
		return Entry{}, err
	}
	return entry, nil
}

func (s *Store) LookupUsenet(mediaID, title string, year int) (UsenetEntry, bool, error) {
	mediaID = strings.TrimSpace(mediaID)
	title = strings.TrimSpace(title)

	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := s.readUsenet()
	if err != nil {
		return UsenetEntry{}, false, err
	}
	index := -1
	if mediaID != "" {
		for i := range entries {
			if entries[i].MediaID == mediaID {
				index = i
				break
			}
		}
	}
	if index < 0 {
		for i := range entries {
			if (mediaID == "" || entries[i].MediaID == "") &&
				entries[i].Year == year && strings.EqualFold(entries[i].Title, title) {
				index = i
				break
			}
		}
	}
	if index < 0 {
		return UsenetEntry{}, false, nil
	}
	entry := entries[index]
	if !validID(entry.ID) {
		return UsenetEntry{}, false, fmt.Errorf("Usenet playback cache contains invalid entry ID %q", entry.ID)
	}
	entry.NZBPath = filepath.Join(s.usenetDir, entry.ID+".nzb")
	if _, err := os.Stat(entry.NZBPath); errors.Is(err, os.ErrNotExist) {
		return UsenetEntry{}, false, nil
	} else if err != nil {
		return UsenetEntry{}, false, fmt.Errorf("inspect cached NZB: %w", err)
	}
	return entry, true, nil
}

func (s *Store) RemoveUsenet(mediaID, title string, year int) error {
	mediaID = strings.TrimSpace(mediaID)
	title = strings.TrimSpace(title)

	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := s.readUsenet()
	if err != nil {
		return err
	}
	for i := range entries {
		matchingMediaID := mediaID != "" && entries[i].MediaID == mediaID
		matchingLegacyTitle := (mediaID == "" || entries[i].MediaID == "") &&
			entries[i].Year == year && strings.EqualFold(entries[i].Title, title)
		if !matchingMediaID && !matchingLegacyTitle {
			continue
		}
		if validID(entries[i].ID) {
			path := filepath.Join(s.usenetDir, entries[i].ID+".nzb")
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("remove cached NZB: %w", err)
			}
		}
		entries = append(entries[:i], entries[i+1:]...)
		return s.writeUsenet(entries)
	}
	return nil
}

func (s *Store) SaveUsenet(
	mediaID, title string,
	year int,
	selected catalog.RankedCandidate,
	nzb []byte,
) (UsenetEntry, error) {
	mediaID = strings.TrimSpace(mediaID)
	title = strings.TrimSpace(title)
	if title == "" {
		return UsenetEntry{}, errors.New("cached Usenet playback title cannot be empty")
	}
	if len(nzb) == 0 {
		return UsenetEntry{}, errors.New("cached NZB cannot be empty")
	}
	selected.Candidate.MagnetURI = ""
	selected.Candidate.TorrentURL = ""
	selected.Candidate.NZBURL = ""

	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := s.readUsenet()
	if err != nil {
		return UsenetEntry{}, err
	}
	id := cacheID(mediaID, title, year)
	index := -1
	for i := range entries {
		matchingMediaID := mediaID != "" && entries[i].MediaID == mediaID
		matchingLegacyTitle := (mediaID == "" || entries[i].MediaID == "") &&
			entries[i].Year == year && strings.EqualFold(entries[i].Title, title)
		if matchingMediaID || matchingLegacyTitle {
			id = entries[i].ID
			index = i
			break
		}
	}
	if !validID(id) {
		return UsenetEntry{}, fmt.Errorf("invalid Usenet playback cache ID %q", id)
	}
	if err := os.MkdirAll(s.usenetDir, 0o700); err != nil {
		return UsenetEntry{}, fmt.Errorf("create Usenet playback cache directory: %w", err)
	}
	nzbPath := filepath.Join(s.usenetDir, id+".nzb")
	if err := writePrivateFile(nzbPath, nzb); err != nil {
		return UsenetEntry{}, fmt.Errorf("write cached NZB: %w", err)
	}
	entry := UsenetEntry{
		ID: id, MediaID: mediaID, Title: title, Year: year,
		Selected: selected, UpdatedAt: time.Now().UTC(), NZBPath: nzbPath,
	}
	if index >= 0 {
		entries[index] = entry
	} else {
		entries = append(entries, entry)
	}
	if err := s.writeUsenet(entries); err != nil {
		return UsenetEntry{}, err
	}
	return entry, nil
}

func (s *Store) LoadUsenetFailures() (map[string]time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	contents, err := os.ReadFile(s.usenetFailuresPath)
	if errors.Is(err, os.ErrNotExist) {
		return make(map[string]time.Time), nil
	}
	if err != nil {
		return nil, fmt.Errorf("read Usenet failure cache: %w", err)
	}
	var payload struct {
		Version  int                  `json:"version"`
		Failures map[string]time.Time `json:"failures"`
	}
	if err := json.Unmarshal(contents, &payload); err != nil {
		return nil, fmt.Errorf("parse Usenet failure cache: %w", err)
	}
	if payload.Failures == nil {
		payload.Failures = make(map[string]time.Time)
	}
	now := time.Now()
	for key, expiresAt := range payload.Failures {
		if !expiresAt.After(now) {
			delete(payload.Failures, key)
		}
	}
	return payload.Failures, nil
}

func (s *Store) SaveUsenetFailures(failures map[string]time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	active := make(map[string]time.Time, len(failures))
	for key, expiresAt := range failures {
		if expiresAt.After(now) {
			active[key] = expiresAt
		}
	}
	if len(active) == 0 {
		if err := os.Remove(s.usenetFailuresPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove Usenet failure cache: %w", err)
		}
		return nil
	}
	payload := struct {
		Version  int                  `json:"version"`
		Failures map[string]time.Time `json:"failures"`
	}{Version: 1, Failures: active}
	contents, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	contents = append(contents, '\n')
	if err := os.MkdirAll(filepath.Dir(s.usenetFailuresPath), 0o700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	if err := writePrivateFile(s.usenetFailuresPath, contents); err != nil {
		return fmt.Errorf("replace Usenet failure cache: %w", err)
	}
	return nil
}

func (s *Store) read() ([]Entry, error) {
	contents, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read playback cache: %w", err)
	}
	var payload struct {
		Version int     `json:"version"`
		Entries []Entry `json:"entries"`
	}
	if err := json.Unmarshal(contents, &payload); err != nil {
		return nil, fmt.Errorf("parse playback cache: %w", err)
	}
	return payload.Entries, nil
}

func (s *Store) write(entries []Entry) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	payload := struct {
		Version int     `json:"version"`
		Entries []Entry `json:"entries"`
	}{Version: 1, Entries: entries}
	contents, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	contents = append(contents, '\n')
	if err := writePrivateFile(s.path, contents); err != nil {
		return fmt.Errorf("replace playback cache: %w", err)
	}
	return nil
}

func (s *Store) readUsenet() ([]UsenetEntry, error) {
	contents, err := os.ReadFile(s.usenetPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read Usenet playback cache: %w", err)
	}
	var payload struct {
		Version int           `json:"version"`
		Entries []UsenetEntry `json:"entries"`
	}
	if err := json.Unmarshal(contents, &payload); err != nil {
		return nil, fmt.Errorf("parse Usenet playback cache: %w", err)
	}
	return payload.Entries, nil
}

func (s *Store) writeUsenet(entries []UsenetEntry) error {
	if err := os.MkdirAll(filepath.Dir(s.usenetPath), 0o700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	payload := struct {
		Version int           `json:"version"`
		Entries []UsenetEntry `json:"entries"`
	}{Version: 1, Entries: entries}
	contents, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	contents = append(contents, '\n')
	if err := writePrivateFile(s.usenetPath, contents); err != nil {
		return fmt.Errorf("replace Usenet playback cache: %w", err)
	}
	return nil
}

func writePrivateFile(path string, contents []byte) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), ".playback-cache-*")
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

func cacheID(mediaID, title string, year int) string {
	identity := strings.TrimSpace(mediaID)
	if identity == "" {
		identity = strings.ToLower(strings.Join(strings.Fields(title), " "))
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s/%d", identity, year)))
	return hex.EncodeToString(sum[:8])
}

func validID(id string) bool {
	if len(id) != 16 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}
