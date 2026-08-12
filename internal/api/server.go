package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pgeske/filmstream/internal/catalog"
	"github.com/pgeske/filmstream/internal/history"
	"github.com/pgeske/filmstream/internal/hls"
	"github.com/pgeske/filmstream/internal/indexer"
	"github.com/pgeske/filmstream/internal/metadata"
	"github.com/pgeske/filmstream/internal/resolver"
	"github.com/pgeske/filmstream/internal/torrentstream"
)

type CreatePlaybackRequest struct {
	Query       string              `json:"query,omitempty"`
	Year        int                 `json:"year,omitempty"`
	Preferences catalog.Preferences `json:"preferences,omitempty"`
	MagnetURI   string              `json:"magnet_uri,omitempty"`
	TorrentPath string              `json:"torrent_path,omitempty"`
}

const (
	maxLiveSwarmCandidates = 3
	minimumLivePeers       = 3
	strongLiveSwarmPeers   = 20
	liveSwarmWait          = 16 * time.Second
)

type CreatePlaybackResponse struct {
	ID        string                   `json:"id"`
	Name      string                   `json:"name"`
	FileName  string                   `json:"file_name"`
	FileSize  int64                    `json:"file_size"`
	StreamURL string                   `json:"stream_url"`
	Selected  *catalog.RankedCandidate `json:"selected,omitempty"`
}

type HLSStreamManager interface {
	Start(context.Context, string, float64) (hls.Stream, error)
	StartSubtitle(context.Context, string, int) error
	AssetPath(string, string) (string, error)
	Stop(string)
}

type Server struct {
	indexers       *indexer.Registry
	engine         *torrentstream.Engine
	defaults       catalog.Preferences
	logger         *slog.Logger
	reloadIndexers func() error
	reloadResolver func() error

	resolverMu       sync.RWMutex
	movieResolver    resolver.Resolver
	metadataMu       sync.RWMutex
	metadataProvider metadata.Provider
	historyStore     *history.Store
	hlsMu            sync.RWMutex
	hlsManager       HLSStreamManager
	mu               sync.RWMutex
	selected         map[string]catalog.RankedCandidate
}

func New(indexers *indexer.Registry, engine *torrentstream.Engine, defaults catalog.Preferences, logger *slog.Logger) *Server {
	server := &Server{
		indexers: indexers,
		engine:   engine,
		defaults: defaults,
		logger:   logger,
		selected: make(map[string]catalog.RankedCandidate),
	}
	engine.SetCleanupHandler(func(id, _ string) {
		server.mu.Lock()
		delete(server.selected, id)
		server.mu.Unlock()
		server.hlsMu.RLock()
		manager := server.hlsManager
		server.hlsMu.RUnlock()
		if manager != nil {
			manager.Stop(id)
		}
	})
	return server
}

func (s *Server) SetIndexerReloader(reload func() error) {
	s.reloadIndexers = reload
}

func (s *Server) SetMovieResolver(movieResolver resolver.Resolver) {
	s.resolverMu.Lock()
	s.movieResolver = movieResolver
	s.resolverMu.Unlock()
}

func (s *Server) SetResolverReloader(reload func() error) {
	s.reloadResolver = reload
}

func (s *Server) SetMetadataProvider(provider metadata.Provider) {
	s.metadataMu.Lock()
	s.metadataProvider = provider
	s.metadataMu.Unlock()
}

func (s *Server) SetHistoryStore(store *history.Store) {
	s.historyStore = store
}

func (s *Server) SetHLSManager(manager HLSStreamManager) {
	s.hlsMu.Lock()
	s.hlsManager = manager
	s.hlsMu.Unlock()
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("POST /v1/indexers/reload", s.reloadIndexerConfiguration)
	mux.HandleFunc("POST /v1/resolver/reload", s.reloadResolverConfiguration)
	mux.HandleFunc("POST /v1/resolve", s.resolveMovie)
	mux.HandleFunc("GET /v1/catalog/search", s.searchCatalog)
	mux.HandleFunc("GET /v1/watch-history", s.listWatchHistory)
	mux.HandleFunc("PUT /v1/watch-history", s.updateWatchProgress)
	mux.HandleFunc("POST /v1/playbacks", s.createPlayback)
	mux.HandleFunc("GET /v1/playbacks/{id}", s.playbackStatus)
	mux.HandleFunc("GET /v1/playbacks/{id}/stream", s.streamPlayback)
	mux.HandleFunc("HEAD /v1/playbacks/{id}/stream", s.streamPlayback)
	mux.HandleFunc("POST /v1/playbacks/{id}/hls", s.startHLSPlayback)
	mux.HandleFunc("DELETE /v1/playbacks/{id}/hls", s.stopHLSPlayback)
	mux.HandleFunc("POST /v1/playbacks/{id}/hls/subtitles/{index}", s.startHLSSubtitle)
	mux.HandleFunc("GET /v1/playbacks/{id}/hls/{asset}", s.serveHLSAsset)
	return mux
}

func (s *Server) resolveMovie(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	var request struct {
		Query string `json:"query"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	request.Query = strings.TrimSpace(request.Query)
	if request.Query == "" {
		writeError(w, http.StatusBadRequest, "query cannot be empty")
		return
	}
	s.resolverMu.RLock()
	movieResolver := s.movieResolver
	s.resolverMu.RUnlock()
	if movieResolver == nil {
		writeJSON(w, http.StatusOK, resolver.Result{
			Input:      request.Query,
			Candidates: []resolver.Candidate{{Title: request.Query, Confidence: 1}},
		})
		return
	}
	result, err := movieResolver.Resolve(r.Context(), request.Query)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) searchCatalog(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("query"))
	if query == "" {
		query = strings.TrimSpace(r.URL.Query().Get("q"))
	}
	if query == "" {
		writeError(w, http.StatusBadRequest, "query cannot be empty")
		return
	}
	movies, err := s.searchMovies(r.Context(), query)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	if movies == nil {
		movies = []metadata.Movie{}
	}
	writeJSON(w, http.StatusOK, struct {
		Items []metadata.Movie `json:"items"`
	}{Items: movies})
}

func (s *Server) searchMovies(ctx context.Context, query string) ([]metadata.Movie, error) {
	s.metadataMu.RLock()
	provider := s.metadataProvider
	s.metadataMu.RUnlock()
	if provider != nil {
		movies, err := provider.Search(ctx, query)
		if err != nil {
			return nil, err
		}
		if len(movies) > 0 {
			return movies, nil
		}
	}

	s.resolverMu.RLock()
	movieResolver := s.movieResolver
	s.resolverMu.RUnlock()
	if movieResolver == nil {
		return []metadata.Movie{fallbackMovie(query, 0)}, nil
	}
	result, err := movieResolver.Resolve(ctx, query)
	if err != nil {
		return nil, err
	}
	if provider == nil {
		movies := make([]metadata.Movie, 0, len(result.Candidates))
		for _, candidate := range result.Candidates {
			movies = append(movies, fallbackMovie(candidate.Title, candidate.Year))
		}
		return movies, nil
	}

	movies := make([]metadata.Movie, 0, len(result.Candidates))
	seen := make(map[string]bool)
	for _, candidate := range result.Candidates {
		results, err := provider.Search(ctx, candidate.Title)
		if err != nil {
			return nil, err
		}
		for _, movie := range results {
			if candidate.Year > 0 && movie.Year > 0 && candidate.Year != movie.Year {
				continue
			}
			if !seen[movie.ID] {
				movies = append(movies, movie)
				seen[movie.ID] = true
			}
			break
		}
	}
	return movies, nil
}

func fallbackMovie(title string, year int) metadata.Movie {
	normalized := strings.ToLower(strings.Join(strings.Fields(title), "-"))
	return metadata.Movie{
		ID:    fmt.Sprintf("filmstream:%s:%d", normalized, year),
		Title: strings.TrimSpace(title),
		Year:  year,
	}
}

func (s *Server) listWatchHistory(w http.ResponseWriter, r *http.Request) {
	if s.historyStore == nil {
		writeError(w, http.StatusServiceUnavailable, "watch history is not configured")
		return
	}
	entries, err := s.historyStore.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if entries == nil {
		entries = []history.Entry{}
	}
	entries = s.enrichWatchHistory(r.Context(), entries)
	if r.URL.Query().Get("continue") == "true" {
		filtered := entries[:0]
		for _, entry := range entries {
			if entry.CanContinue() {
				filtered = append(filtered, entry)
			}
		}
		entries = filtered
	}
	writeJSON(w, http.StatusOK, struct {
		Entries []history.Entry `json:"entries"`
	}{Entries: entries})
}

func (s *Server) enrichWatchHistory(ctx context.Context, entries []history.Entry) []history.Entry {
	s.metadataMu.RLock()
	provider := s.metadataProvider
	s.metadataMu.RUnlock()
	if provider == nil {
		return entries
	}
	for i, entry := range entries {
		if strings.HasPrefix(entry.MediaID, "tmdb:") {
			continue
		}
		movies, err := provider.Search(ctx, entry.Title)
		if err != nil {
			if s.logger != nil {
				s.logger.Warn("enrich watch history", "title", entry.Title, "error", err)
			}
			continue
		}
		for _, movie := range movies {
			if entry.Year > 0 && movie.Year > 0 && entry.Year != movie.Year {
				continue
			}
			enriched, err := s.historyStore.UpdateMetadata(entry.ID, history.Entry{
				MediaID:     movie.ID,
				Overview:    movie.Overview,
				PosterURL:   movie.PosterURL,
				BackdropURL: movie.BackdropURL,
			})
			if err == nil {
				entries[i] = enriched
			} else if s.logger != nil {
				s.logger.Warn("save watch history metadata", "title", entry.Title, "error", err)
			}
			break
		}
	}
	return entries
}

func (s *Server) updateWatchProgress(w http.ResponseWriter, r *http.Request) {
	if s.historyStore == nil {
		writeError(w, http.StatusServiceUnavailable, "watch history is not configured")
		return
	}
	defer r.Body.Close()
	var request struct {
		MediaID         string  `json:"media_id,omitempty"`
		Title           string  `json:"title"`
		Year            int     `json:"year,omitempty"`
		Overview        string  `json:"overview,omitempty"`
		PosterURL       string  `json:"poster_url,omitempty"`
		BackdropURL     string  `json:"backdrop_url,omitempty"`
		PositionSeconds float64 `json:"position_seconds"`
		DurationSeconds float64 `json:"duration_seconds,omitempty"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	entry, err := s.historyStore.RecordProgress(history.Entry{
		MediaID:         request.MediaID,
		Title:           request.Title,
		Year:            request.Year,
		Overview:        request.Overview,
		PosterURL:       request.PosterURL,
		BackdropURL:     request.BackdropURL,
		PositionSeconds: request.PositionSeconds,
		DurationSeconds: request.DurationSeconds,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, entry)
}

func (s *Server) reloadResolverConfiguration(w http.ResponseWriter, _ *http.Request) {
	if s.reloadResolver == nil {
		writeError(w, http.StatusNotImplemented, "resolver reload is not configured")
		return
	}
	if err := s.reloadResolver(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) reloadIndexerConfiguration(w http.ResponseWriter, _ *http.Request) {
	if s.reloadIndexers == nil {
		writeError(w, http.StatusNotImplemented, "indexer reload is not configured")
		return
	}
	if err := s.reloadIndexers(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) createPlayback(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	var request CreatePlaybackRequest
	if err := decoder.Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}

	preferences := mergePreferences(s.defaults, request.Preferences)
	sourceCount := 0
	if strings.TrimSpace(request.Query) != "" {
		sourceCount++
	}
	if request.MagnetURI != "" {
		sourceCount++
	}
	if request.TorrentPath != "" {
		sourceCount++
	}
	if sourceCount != 1 {
		writeError(w, http.StatusBadRequest, "provide exactly one of query, magnet_uri, or torrent_path")
		return
	}

	var session *torrentstream.Session
	var selected *catalog.RankedCandidate
	if request.Query != "" {
		search := catalog.SearchRequest{Query: strings.TrimSpace(request.Query), Year: request.Year, Preferences: preferences}
		candidates, err := s.indexers.Search(r.Context(), search)
		if err != nil {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		ranked := catalog.Rank(search, candidates)
		if len(ranked) == 0 {
			writeError(w, http.StatusNotFound, "no matching torrent candidates found")
			return
		}
		session, selected, err = s.createRankedPlayback(r.Context(), ranked, preferences.StreamingOptimized)
		if err != nil {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
	} else {
		var err error
		session, err = s.engine.Create(r.Context(), torrentstream.Source{
			MagnetURI: request.MagnetURI, TorrentPath: request.TorrentPath,
		})
		if err != nil {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
	}
	if selected != nil {
		s.mu.Lock()
		s.selected[session.ID] = *selected
		s.mu.Unlock()
	}

	s.logger.Info("created playback", "id", session.ID, "name", session.Name, "file", session.FileName)
	writeJSON(w, http.StatusCreated, CreatePlaybackResponse{
		ID:        session.ID,
		Name:      session.Name,
		FileName:  session.FileName,
		FileSize:  session.FileSize,
		StreamURL: fmt.Sprintf("%s://%s/v1/playbacks/%s/stream", requestScheme(r), r.Host, session.ID),
		Selected:  selected,
	})
}

func (s *Server) createRankedPlayback(
	ctx context.Context,
	ranked []catalog.RankedCandidate,
	validateSwarm bool,
) (*torrentstream.Session, *catalog.RankedCandidate, error) {
	type option struct {
		session   *torrentstream.Session
		candidate catalog.RankedCandidate
		peers     int
	}

	attempts := 1
	if validateSwarm {
		attempts = min(maxLiveSwarmCandidates, len(ranked))
	}
	options := make([]option, 0, attempts)
	var failures []string
	for i := 0; i < attempts; i++ {
		candidate := ranked[i]
		resolved, err := s.indexers.Resolve(ctx, candidate.Candidate)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", candidate.Candidate.Name, err))
			continue
		}
		session, err := s.engine.Create(ctx, torrentstream.Source{
			MagnetURI: resolved.MagnetURI, TorrentURL: resolved.TorrentURL,
		})
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", candidate.Candidate.Name, err))
			continue
		}
		if !validateSwarm {
			return session, &candidate, nil
		}
		options = append(options, option{session: session, candidate: candidate})
	}
	if len(options) == 0 {
		if len(failures) > 0 {
			return nil, nil, errors.New(strings.Join(failures, "; "))
		}
		return nil, nil, errors.New("no usable torrent candidates found")
	}

	deadline := time.NewTimer(liveSwarmWait)
	ticker := time.NewTicker(250 * time.Millisecond)
	defer deadline.Stop()
	defer ticker.Stop()
observe:
	for {
		strongest := 0
		for i := range options {
			status, ok := s.engine.Status(options[i].session.ID)
			if !ok {
				continue
			}
			options[i].peers = max(status.ActivePeers, status.ConnectedSeeders)
			strongest = max(strongest, options[i].peers)
		}
		if strongest >= strongLiveSwarmPeers {
			break
		}
		select {
		case <-ctx.Done():
			for _, option := range options {
				_ = s.engine.Drop(option.session.ID)
			}
			return nil, nil, ctx.Err()
		case <-deadline.C:
			break observe
		case <-ticker.C:
		}
	}

	best := 0
	for i := 1; i < len(options); i++ {
		if options[i].peers > options[best].peers {
			best = i
		}
	}
	chosen := options[best]
	for i := range options {
		if i != best {
			_ = s.engine.Drop(options[i].session.ID)
		}
	}
	if chosen.peers >= minimumLivePeers {
		s.logger.Info("selected strongest live playback swarm", "id", chosen.session.ID,
			"name", chosen.candidate.Candidate.Name, "live_peers", chosen.peers,
			"reported_seeders", chosen.candidate.Candidate.Seeders)
	} else {
		s.logger.Warn("using best available playback despite weak live swarm", "id", chosen.session.ID,
			"name", chosen.candidate.Candidate.Name, "live_peers", chosen.peers,
			"reported_seeders", chosen.candidate.Candidate.Seeders)
	}
	return chosen.session, &chosen.candidate, nil
}

func (s *Server) playbackStatus(w http.ResponseWriter, r *http.Request) {
	status, ok := s.engine.Status(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "playback not found")
		return
	}
	type response struct {
		torrentstream.Status
		Selected *catalog.RankedCandidate `json:"selected,omitempty"`
	}
	var selected *catalog.RankedCandidate
	s.mu.RLock()
	if value, ok := s.selected[status.ID]; ok {
		copy := value
		selected = &copy
	}
	s.mu.RUnlock()
	writeJSON(w, http.StatusOK, response{Status: status, Selected: selected})
}

func (s *Server) streamPlayback(w http.ResponseWriter, r *http.Request) {
	if err := s.engine.ServeHTTP(w, r, r.PathValue("id")); err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		writeError(w, http.StatusNotFound, err.Error())
	}
}

func (s *Server) startHLSPlayback(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.engine.Get(id); !ok {
		writeError(w, http.StatusNotFound, "playback not found")
		return
	}
	s.hlsMu.RLock()
	manager := s.hlsManager
	s.hlsMu.RUnlock()
	if manager == nil {
		writeError(w, http.StatusNotImplemented, "HLS playback is not configured")
		return
	}
	defer r.Body.Close()
	var request struct {
		StartSeconds float64 `json:"start_seconds,omitempty"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	stream, err := manager.Start(r.Context(), id, request.StartSeconds)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, struct {
		hls.Stream
		PlaylistURL string `json:"playlist_url"`
	}{
		Stream:      stream,
		PlaylistURL: fmt.Sprintf("%s://%s/v1/playbacks/%s/hls/index.m3u8", requestScheme(r), r.Host, id),
	})
}

func (s *Server) startHLSSubtitle(w http.ResponseWriter, r *http.Request) {
	s.hlsMu.RLock()
	manager := s.hlsManager
	s.hlsMu.RUnlock()
	if manager == nil {
		writeError(w, http.StatusNotImplemented, "HLS playback is not configured")
		return
	}
	index, err := strconv.Atoi(r.PathValue("index"))
	if err != nil || index < 0 {
		writeError(w, http.StatusBadRequest, "invalid subtitle index")
		return
	}
	if err := manager.StartSubtitle(r.Context(), r.PathValue("id"), index); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeError(w, http.StatusNotFound, "subtitle track not found")
			return
		}
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "preparing"})
}

func (s *Server) stopHLSPlayback(w http.ResponseWriter, r *http.Request) {
	s.hlsMu.RLock()
	manager := s.hlsManager
	s.hlsMu.RUnlock()
	if manager != nil {
		manager.Stop(r.PathValue("id"))
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) serveHLSAsset(w http.ResponseWriter, r *http.Request) {
	s.hlsMu.RLock()
	manager := s.hlsManager
	s.hlsMu.RUnlock()
	if manager == nil {
		http.NotFound(w, r)
		return
	}
	name := r.PathValue("asset")
	path, err := manager.AssetPath(r.PathValue("id"), name)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			http.NotFound(w, r)
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	switch {
	case strings.HasSuffix(name, ".m3u8"):
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	case strings.HasSuffix(name, ".m4s"):
		w.Header().Set("Content-Type", "video/iso.segment")
	case strings.HasSuffix(name, ".mp4"):
		w.Header().Set("Content-Type", "video/mp4")
	case strings.HasSuffix(name, ".vtt"):
		w.Header().Set("Content-Type", "text/vtt; charset=utf-8")
	}
	http.ServeFile(w, r, path)
}

func mergePreferences(defaults, requested catalog.Preferences) catalog.Preferences {
	result := requested
	if result.Resolution == "" {
		result.Resolution = defaults.Resolution
	}
	if len(result.Codecs) == 0 {
		result.Codecs = append([]string(nil), defaults.Codecs...)
	}
	if len(result.Languages) == 0 {
		result.Languages = append([]string(nil), defaults.Languages...)
	}
	if result.MaxSizeBytes == 0 {
		result.MaxSizeBytes = defaults.MaxSizeBytes
	}
	return result
}

func requestScheme(r *http.Request) string {
	if forwarded := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0]); forwarded == "http" || forwarded == "https" {
		return forwarded
	}
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
