package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/pgeske/filmstream/internal/catalog"
	"github.com/pgeske/filmstream/internal/indexer"
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

type CreatePlaybackResponse struct {
	ID        string                   `json:"id"`
	Name      string                   `json:"name"`
	FileName  string                   `json:"file_name"`
	FileSize  int64                    `json:"file_size"`
	StreamURL string                   `json:"stream_url"`
	Selected  *catalog.RankedCandidate `json:"selected,omitempty"`
}

type Server struct {
	indexers       *indexer.Registry
	engine         *torrentstream.Engine
	defaults       catalog.Preferences
	logger         *slog.Logger
	reloadIndexers func() error
	reloadResolver func() error

	resolverMu    sync.RWMutex
	movieResolver resolver.Resolver
	mu            sync.RWMutex
	selected      map[string]catalog.RankedCandidate
}

func New(indexers *indexer.Registry, engine *torrentstream.Engine, defaults catalog.Preferences, logger *slog.Logger) *Server {
	return &Server{
		indexers: indexers,
		engine:   engine,
		defaults: defaults,
		logger:   logger,
		selected: make(map[string]catalog.RankedCandidate),
	}
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

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("POST /v1/indexers/reload", s.reloadIndexerConfiguration)
	mux.HandleFunc("POST /v1/resolver/reload", s.reloadResolverConfiguration)
	mux.HandleFunc("POST /v1/resolve", s.resolveMovie)
	mux.HandleFunc("POST /v1/playbacks", s.createPlayback)
	mux.HandleFunc("GET /v1/playbacks/{id}", s.playbackStatus)
	mux.HandleFunc("GET /v1/playbacks/{id}/stream", s.streamPlayback)
	mux.HandleFunc("HEAD /v1/playbacks/{id}/stream", s.streamPlayback)
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

	var source torrentstream.Source
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
		selected = &ranked[0]
		resolved, err := s.indexers.Resolve(r.Context(), selected.Candidate)
		if err != nil {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		source = torrentstream.Source{MagnetURI: resolved.MagnetURI, TorrentURL: resolved.TorrentURL}
	} else {
		source = torrentstream.Source{MagnetURI: request.MagnetURI, TorrentPath: request.TorrentPath}
	}

	session, err := s.engine.Create(r.Context(), source)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
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
		StreamURL: fmt.Sprintf("http://%s/v1/playbacks/%s/stream", r.Host, session.ID),
		Selected:  selected,
	})
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

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
