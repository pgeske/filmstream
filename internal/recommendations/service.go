package recommendations

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/pgeske/filmstream/internal/metadata"
)

const (
	MaxPromptBytes        = 4000
	defaultRefreshTTL     = 24 * time.Hour
	defaultRefreshTimeout = 10 * time.Minute
)

var (
	ErrInvalidPrompt = errors.New("prompt must be valid UTF-8")
	ErrPromptTooLong = errors.New("prompt is too long")
)

type RefreshReason int

const (
	RefreshAutomatic RefreshReason = iota
	RefreshManual
	refreshPromptChanged
)

type Response struct {
	GeneratedAt *time.Time       `json:"generated_at,omitempty"`
	Prompt      string           `json:"prompt"`
	Refreshing  bool             `json:"refreshing"`
	Items       []metadata.Movie `json:"items"`
}

type Manager interface {
	Snapshot() Response
	Refresh(RefreshReason) Response
	SetPrompt(string) (Response, error)
}

type ServiceOptions struct {
	RefreshTTL     time.Duration
	RefreshTimeout time.Duration
	Now            func() time.Time
	// PromptFile is an absolute path to a note whose text is the authoritative
	// taste prompt whenever the file exists and is non-empty. When it is unset,
	// missing, or empty, the stored prompt remains the taste prompt.
	PromptFile string
}

type Service struct {
	store      *Store
	storeReady bool
	logger     *slog.Logger

	mu             sync.Mutex
	state          persistedState
	generator      ItemGenerator
	refreshing     bool
	promptRevision uint64
	context        context.Context
	refreshTTL     time.Duration
	refreshTimeout time.Duration
	now            func() time.Time

	// filePrompt is the trimmed prompt file text; empty means the file is
	// unset, missing, unreadable, or blank and the stored prompt applies.
	filePrompt  string
	fileModTime time.Time
	promptFile  string
}

type refreshJob struct {
	context   context.Context
	generator ItemGenerator
	prompt    string
	revision  uint64
}

func NewService(store *Store, generator ItemGenerator, logger *slog.Logger, options ServiceOptions) *Service {
	refreshTTL := options.RefreshTTL
	if refreshTTL <= 0 {
		refreshTTL = defaultRefreshTTL
	}
	refreshTimeout := options.RefreshTimeout
	if refreshTimeout <= 0 {
		refreshTimeout = defaultRefreshTimeout
	}
	now := options.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	state := persistedState{Version: recommendationStateVersion, Items: []metadata.Movie{}}
	storeReady := true
	if store != nil {
		loaded, err := store.load()
		if err != nil {
			storeReady = false
			if logger != nil {
				logger.Warn("load recommendation state failed; starting empty", "error", err)
			}
		} else {
			state = loaded
		}
	}
	service := &Service{
		store: store, storeReady: storeReady, logger: logger, state: state, generator: generator,
		context: context.Background(), refreshTTL: refreshTTL, refreshTimeout: refreshTimeout, now: now,
		promptFile: options.PromptFile,
	}
	service.refreshPromptFile()
	return service
}

func (s *Service) Start(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	s.context = ctx
	var job refreshJob
	started := false
	if s.state.PromptRefreshPending {
		job, started = s.beginRefreshLocked(refreshPromptChanged)
	}
	s.mu.Unlock()
	if started {
		s.run(job)
	}
}

func (s *Service) SetGenerator(generator ItemGenerator) {
	s.mu.Lock()
	s.generator = generator
	var job refreshJob
	started := false
	if generator != nil && s.state.PromptRefreshPending {
		job, started = s.beginRefreshLocked(refreshPromptChanged)
	}
	s.mu.Unlock()
	if started {
		s.run(job)
	}
}

func (s *Service) Snapshot() Response {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshotLocked()
}

func (s *Service) Refresh(reason RefreshReason) Response {
	s.refreshPromptFile()
	s.mu.Lock()
	job, started := s.beginRefreshLocked(reason)
	response := s.snapshotLocked()
	s.mu.Unlock()
	if started {
		s.run(job)
	}
	return response
}

func (s *Service) SetPrompt(prompt string) (Response, error) {
	prompt = strings.TrimSpace(prompt)
	if !utf8.ValidString(prompt) {
		return s.Snapshot(), ErrInvalidPrompt
	}
	if len([]byte(prompt)) > MaxPromptBytes {
		return s.Snapshot(), ErrPromptTooLong
	}

	s.mu.Lock()
	if prompt == s.state.Prompt {
		response := s.snapshotLocked()
		s.mu.Unlock()
		return response, nil
	}
	next := s.state
	next.Prompt = prompt
	if s.filePrompt == "" {
		// Without an authoritative prompt file the stored prompt drives
		// generation, so a change schedules a refresh as before.
		next.PromptRefreshPending = true
	}
	if err := s.saveLocked(next); err != nil {
		response := s.snapshotLocked()
		s.mu.Unlock()
		return response, err
	}
	s.state = next
	s.promptRevision++
	var job refreshJob
	started := false
	if s.filePrompt == "" {
		job, started = s.beginRefreshLocked(refreshPromptChanged)
	}
	response := s.snapshotLocked()
	s.mu.Unlock()
	if started {
		s.run(job)
	}
	return response, nil
}

func (s *Service) beginRefreshLocked(reason RefreshReason) (refreshJob, bool) {
	if s.refreshing || s.generator == nil {
		return refreshJob{}, false
	}
	now := s.now().UTC()
	if reason == RefreshAutomatic && !s.automaticRefreshDueLocked(now) {
		return refreshJob{}, false
	}

	next := s.state
	next.LastAttemptAt = now
	next.PromptRefreshPending = false
	if s.storeReady {
		if err := s.saveLocked(next); err != nil && s.logger != nil {
			s.logger.Warn("persist recommendation refresh attempt", "error", err)
		}
	}
	s.state = next
	s.refreshing = true
	return refreshJob{
		context: s.context, generator: s.generator, prompt: s.effectivePromptLocked(), revision: s.promptRevision,
	}, true
}

func (s *Service) automaticRefreshDueLocked(now time.Time) bool {
	if s.state.PromptRefreshPending {
		return true
	}
	generatedForCurrentPrompt := s.state.GeneratedPromptHash == promptHash(s.effectivePromptLocked())
	if generatedForCurrentPrompt && !s.state.GeneratedAt.IsZero() && now.Sub(s.state.GeneratedAt) < s.refreshTTL {
		return false
	}
	return s.state.LastAttemptAt.IsZero() || now.Sub(s.state.LastAttemptAt) >= s.refreshTTL
}

func (s *Service) run(job refreshJob) {
	go func() {
		ctx, cancel := context.WithTimeout(job.context, s.refreshTimeout)
		defer cancel()
		items, err := job.generator.Generate(ctx, job.prompt)
		s.finishRefresh(job, items, err)
	}()
}

func (s *Service) finishRefresh(job refreshJob, items []metadata.Movie, generationErr error) {
	s.mu.Lock()
	s.refreshing = false
	currentPrompt := job.revision == s.promptRevision && job.prompt == s.effectivePromptLocked()
	if generationErr == nil && currentPrompt {
		next := s.state
		next.GeneratedAt = s.now().UTC()
		next.GeneratedPromptHash = promptHash(job.prompt)
		next.Items = cloneMovies(items)
		next.PromptRefreshPending = false
		if err := s.saveLocked(next); err != nil {
			if s.logger != nil {
				s.logger.Warn("persist generated recommendations", "error", err)
			}
		} else {
			s.state = next
		}
	} else if generationErr != nil && s.logger != nil {
		// Model/provider errors can contain private request data, so do not log the error text.
		s.logger.Warn("recommendation refresh failed; retaining cached items")
	}

	var nextJob refreshJob
	started := false
	if !currentPrompt && s.state.PromptRefreshPending {
		nextJob, started = s.beginRefreshLocked(refreshPromptChanged)
	}
	s.mu.Unlock()
	if started {
		s.run(nextJob)
	}
}

// effectivePromptLocked returns the taste prompt generation uses. A configured,
// non-empty prompt file is authoritative and overrides the stored prompt.
func (s *Service) effectivePromptLocked() string {
	if s.filePrompt != "" {
		return s.filePrompt
	}
	return s.state.Prompt
}

// refreshPromptFile reloads the prompt file after its modification time
// changes. The hot path pays one stat per refresh; the file itself is only
// read when the mtime moved, outside the service lock. A missing or empty
// file falls back to the stored prompt.
func (s *Service) refreshPromptFile() {
	if s.promptFile == "" {
		return
	}
	info, err := os.Stat(s.promptFile)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) && s.logger != nil {
			s.logger.Warn("stat recommendation prompt file failed; using stored prompt", "error", err)
		}
		s.mu.Lock()
		s.filePrompt = ""
		s.fileModTime = time.Time{}
		s.markPromptRefreshPendingLocked()
		s.mu.Unlock()
		return
	}
	modTime := info.ModTime()
	s.mu.Lock()
	unchanged := modTime.Equal(s.fileModTime)
	s.mu.Unlock()
	if unchanged {
		return
	}

	contents, readErr := os.ReadFile(s.promptFile)
	if readErr != nil && s.logger != nil {
		s.logger.Warn("read recommendation prompt file failed; using stored prompt", "error", readErr)
	}
	s.mu.Lock()
	s.fileModTime = modTime
	s.filePrompt = ""
	if readErr == nil {
		s.filePrompt = strings.TrimSpace(string(contents))
	}
	s.markPromptRefreshPendingLocked()
	s.mu.Unlock()
}

// markPromptRefreshPendingLocked reuses the persisted pending flag whenever the
// effective prompt no longer matches the last generation, so note edits are
// honored on the next GET and survive restarts.
func (s *Service) markPromptRefreshPendingLocked() {
	effective := s.effectivePromptLocked()
	if effective == "" && s.state.GeneratedPromptHash == "" {
		// Nothing has been generated yet and there is no prompt; leave
		// scheduling to the normal TTL path.
		return
	}
	if s.state.GeneratedPromptHash == promptHash(effective) {
		return
	}
	s.state.PromptRefreshPending = true
	if s.storeReady {
		if err := s.saveLocked(s.state); err != nil && s.logger != nil {
			s.logger.Warn("persist recommendation prompt file change", "error", err)
		}
	}
}

func (s *Service) snapshotLocked() Response {
	response := Response{
		Prompt: s.state.Prompt, Refreshing: s.refreshing, Items: cloneMovies(s.state.Items),
	}
	if response.Items == nil {
		response.Items = []metadata.Movie{}
	}
	if !s.state.GeneratedAt.IsZero() {
		generatedAt := s.state.GeneratedAt
		response.GeneratedAt = &generatedAt
	}
	return response
}

func (s *Service) saveLocked(state persistedState) error {
	if s.store == nil {
		return nil
	}
	if err := s.store.save(state); err != nil {
		return err
	}
	s.storeReady = true
	return nil
}

func promptHash(prompt string) string {
	sum := sha256.Sum256([]byte(prompt))
	return hex.EncodeToString(sum[:])
}

func cloneMovies(items []metadata.Movie) []metadata.Movie {
	if len(items) == 0 {
		return []metadata.Movie{}
	}
	cloned := append([]metadata.Movie(nil), items...)
	for index := range cloned {
		cloned[index].Genres = append([]string(nil), items[index].Genres...)
	}
	return cloned
}
