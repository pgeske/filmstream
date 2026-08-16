package history

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const resumeThresholdSeconds = 30

type Entry struct {
	ID              string    `json:"id"`
	MediaID         string    `json:"media_id,omitempty"`
	MediaType       string    `json:"media_type,omitempty"`
	Title           string    `json:"title"`
	Year            int       `json:"year,omitempty"`
	Overview        string    `json:"overview,omitempty"`
	PosterURL       string    `json:"poster_url,omitempty"`
	BackdropURL     string    `json:"backdrop_url,omitempty"`
	Genres          []string  `json:"genres,omitempty"`
	NumberOfSeasons int       `json:"number_of_seasons,omitempty"`
	SeriesID        string    `json:"series_id,omitempty"`
	SeriesTitle     string    `json:"series_title,omitempty"`
	SeasonNumber    int       `json:"season_number,omitempty"`
	EpisodeNumber   int       `json:"episode_number,omitempty"`
	EpisodeTitle    string    `json:"episode_title,omitempty"`
	PositionSeconds float64   `json:"position_seconds"`
	DurationSeconds float64   `json:"duration_seconds,omitempty"`
	Completed       bool      `json:"completed"`
	UpdatedAt       time.Time `json:"updated_at"`
}

func (e Entry) ResumePosition() float64 {
	if e.Completed || e.PositionSeconds < resumeThresholdSeconds {
		return 0
	}
	return e.PositionSeconds
}

func (e Entry) Progress() float64 {
	if e.DurationSeconds <= 0 {
		return 0
	}
	return max(0, min(1, e.PositionSeconds/e.DurationSeconds))
}

func (e Entry) CanContinue() bool {
	return !e.Completed && e.ResumePosition() > 0
}

type Store struct {
	path string
	mu   sync.Mutex
}

func New(stateDir string) *Store {
	return &Store{path: filepath.Join(stateDir, "history.json")}
}

func (s *Store) List() ([]Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := s.read()
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].UpdatedAt.After(entries[j].UpdatedAt)
	})
	return entries, nil
}

func (s *Store) Upsert(title string, year int) (Entry, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return Entry{}, errors.New("history title cannot be empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := s.read()
	if err != nil {
		return Entry{}, err
	}
	id := entryID("", title, year)
	now := time.Now().UTC()
	for i := range entries {
		if entries[i].ID == id {
			entries[i].Title = title
			entries[i].Year = year
			entries[i].UpdatedAt = now
			if err := s.write(entries); err != nil {
				return Entry{}, err
			}
			return entries[i], nil
		}
	}
	entry := Entry{ID: id, Title: title, Year: year, UpdatedAt: now}
	entries = append(entries, entry)
	if err := s.write(entries); err != nil {
		return Entry{}, err
	}
	return entry, nil
}

func (s *Store) RecordProgress(update Entry) (Entry, error) {
	update.Title = strings.TrimSpace(update.Title)
	if update.Title == "" {
		return Entry{}, errors.New("history title cannot be empty")
	}
	update.MediaID = strings.TrimSpace(update.MediaID)
	update.MediaType = strings.TrimSpace(update.MediaType)
	update.SeriesID = strings.TrimSpace(update.SeriesID)
	update.SeriesTitle = strings.TrimSpace(update.SeriesTitle)
	update.EpisodeTitle = strings.TrimSpace(update.EpisodeTitle)
	update.Overview = strings.TrimSpace(update.Overview)
	update.PosterURL = strings.TrimSpace(update.PosterURL)
	update.BackdropURL = strings.TrimSpace(update.BackdropURL)
	update.Genres = normalizeGenres(update.Genres)
	update.PositionSeconds = max(0, update.PositionSeconds)
	update.DurationSeconds = max(0, update.DurationSeconds)
	if update.DurationSeconds > 0 {
		update.PositionSeconds = min(update.PositionSeconds, update.DurationSeconds)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := s.read()
	if err != nil {
		return Entry{}, err
	}
	update.ID = entryID(update.MediaID, update.Title, update.Year)
	update.UpdatedAt = time.Now().UTC()
	for i := range entries {
		if entries[i].ID != update.ID && (update.MediaID == "" || entries[i].MediaID != update.MediaID) {
			continue
		}
		update.ID = entries[i].ID
		if update.MediaID == "" {
			update.MediaID = entries[i].MediaID
		}
		if update.MediaType == "" {
			update.MediaType = entries[i].MediaType
		}
		if update.SeriesID == "" {
			update.SeriesID = entries[i].SeriesID
		}
		if update.SeriesTitle == "" {
			update.SeriesTitle = entries[i].SeriesTitle
		}
		if update.SeasonNumber == 0 {
			update.SeasonNumber = entries[i].SeasonNumber
		}
		if update.EpisodeNumber == 0 {
			update.EpisodeNumber = entries[i].EpisodeNumber
		}
		if update.EpisodeTitle == "" {
			update.EpisodeTitle = entries[i].EpisodeTitle
		}
		if update.NumberOfSeasons == 0 {
			update.NumberOfSeasons = entries[i].NumberOfSeasons
		}
		if update.Overview == "" {
			update.Overview = entries[i].Overview
		}
		if update.PosterURL == "" {
			update.PosterURL = entries[i].PosterURL
		}
		if update.BackdropURL == "" {
			update.BackdropURL = entries[i].BackdropURL
		}
		if len(update.Genres) == 0 {
			update.Genres = entries[i].Genres
		}
		if update.DurationSeconds == 0 {
			update.DurationSeconds = entries[i].DurationSeconds
		}
		update.Completed = update.DurationSeconds > 0 && update.Progress() >= 0.9
		entries[i] = update
		if err := s.write(entries); err != nil {
			return Entry{}, err
		}
		return update, nil
	}
	update.Completed = update.DurationSeconds > 0 && update.Progress() >= 0.9
	entries = append(entries, update)
	if err := s.write(entries); err != nil {
		return Entry{}, err
	}
	return update, nil
}

func (s *Store) UpdateMetadata(id string, metadata Entry) (Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := s.read()
	if err != nil {
		return Entry{}, err
	}
	for i := range entries {
		if entries[i].ID != id {
			continue
		}
		if value := strings.TrimSpace(metadata.MediaID); value != "" {
			entries[i].MediaID = value
		}
		if value := strings.TrimSpace(metadata.MediaType); value != "" {
			entries[i].MediaType = value
		}
		if value := strings.TrimSpace(metadata.SeriesID); value != "" {
			entries[i].SeriesID = value
		}
		if value := strings.TrimSpace(metadata.SeriesTitle); value != "" {
			entries[i].SeriesTitle = value
		}
		if metadata.NumberOfSeasons > 0 {
			entries[i].NumberOfSeasons = metadata.NumberOfSeasons
		}
		if value := strings.TrimSpace(metadata.Overview); value != "" {
			entries[i].Overview = value
		}
		if value := strings.TrimSpace(metadata.PosterURL); value != "" {
			entries[i].PosterURL = value
		}
		if value := strings.TrimSpace(metadata.BackdropURL); value != "" {
			entries[i].BackdropURL = value
		}
		if genres := normalizeGenres(metadata.Genres); len(genres) > 0 {
			entries[i].Genres = genres
		}
		if err := s.write(entries); err != nil {
			return Entry{}, err
		}
		return entries[i], nil
	}
	return Entry{}, fmt.Errorf("history entry %q not found", id)
}

func (s *Store) UpdateProgress(id string, position, duration float64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := s.read()
	if err != nil {
		return err
	}
	for i := range entries {
		if entries[i].ID != id {
			continue
		}
		position = max(0, position)
		duration = max(0, duration)
		if duration > 0 {
			position = min(position, duration)
		}
		entries[i].PositionSeconds = position
		if duration > 0 {
			entries[i].DurationSeconds = duration
		}
		entries[i].Completed = entries[i].DurationSeconds > 0 && entries[i].Progress() >= 0.9
		entries[i].UpdatedAt = time.Now().UTC()
		return s.write(entries)
	}
	return fmt.Errorf("history entry %q not found", id)
}

func normalizeGenres(genres []string) []string {
	normalized := make([]string, 0, len(genres))
	seen := make(map[string]struct{}, len(genres))
	for _, genre := range genres {
		genre = strings.TrimSpace(genre)
		key := strings.ToLower(genre)
		if genre == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		normalized = append(normalized, genre)
	}
	return normalized
}

func (s *Store) MarkUnwatched(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := s.read()
	if err != nil {
		return err
	}
	for i := range entries {
		if entries[i].ID != id {
			continue
		}
		entries[i].PositionSeconds = 0
		entries[i].Completed = false
		entries[i].UpdatedAt = time.Now().UTC()
		return s.write(entries)
	}
	return fmt.Errorf("history entry %q not found", id)
}

func (s *Store) Remove(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := s.read()
	if err != nil {
		return err
	}
	for i := range entries {
		if entries[i].ID == id {
			entries = append(entries[:i], entries[i+1:]...)
			return s.write(entries)
		}
	}
	return nil
}

func (s *Store) RemoveSeries(seriesID string) error {
	seriesID = strings.TrimSpace(seriesID)
	if seriesID == "" {
		return errors.New("series ID cannot be empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := s.read()
	if err != nil {
		return err
	}
	filtered := entries[:0]
	for _, entry := range entries {
		if entry.SeriesID != seriesID {
			filtered = append(filtered, entry)
		}
	}
	return s.write(filtered)
}

func (s *Store) read() ([]Entry, error) {
	contents, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read watch history: %w", err)
	}
	var payload struct {
		Version int     `json:"version"`
		Entries []Entry `json:"entries"`
	}
	if err := json.Unmarshal(contents, &payload); err != nil {
		return nil, fmt.Errorf("parse watch history: %w", err)
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
	}{Version: 2, Entries: entries}
	contents, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	contents = append(contents, '\n')
	temporary, err := os.CreateTemp(filepath.Dir(s.path), ".history-*.json")
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
	if err := os.Rename(temporaryPath, s.path); err != nil {
		return fmt.Errorf("replace watch history: %w", err)
	}
	return nil
}

func entryID(mediaID, title string, year int) string {
	identity := strings.TrimSpace(mediaID)
	if identity == "" {
		identity = fmt.Sprintf("%s/%d", strings.ToLower(strings.Join(strings.Fields(title), " ")), year)
	}
	sum := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(sum[:8])
}
