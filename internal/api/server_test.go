package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pgeske/filmstream/internal/catalog"
	"github.com/pgeske/filmstream/internal/history"
	"github.com/pgeske/filmstream/internal/hls"
	"github.com/pgeske/filmstream/internal/metadata"
	"github.com/pgeske/filmstream/internal/resolver"
	"github.com/pgeske/filmstream/internal/torrentstream"
)

type fakeResolver struct{}

type fakeMetadata struct{}

type fakeRatings struct{}

type fakeHLSManager struct {
	dir              string
	stopped          string
	subtitlePlayback string
	subtitleIndex    int
	subtitleTracks   map[string][]hls.SubtitleTrack
}

func (f *fakeHLSManager) ProbeSubtitles(_ context.Context, id string) ([]hls.SubtitleTrack, error) {
	return f.subtitleTracks[id], nil
}

func (f *fakeHLSManager) Start(_ context.Context, id string, start float64) (hls.Stream, error) {
	return hls.Stream{PlaybackID: id, StartSeconds: start, VideoCodec: "h264"}, nil
}

func (f *fakeHLSManager) StartSubtitle(_ context.Context, playbackID string, index int) error {
	f.subtitlePlayback = playbackID
	f.subtitleIndex = index
	return nil
}

func (f *fakeHLSManager) AssetPath(_, name string) (string, error) {
	return filepath.Join(f.dir, name), nil
}

func (f *fakeHLSManager) Stop(id string) {
	f.stopped = id
}

func (fakeMetadata) IMDbID(_ context.Context, mediaID string) (string, error) {
	if mediaID == "tmdb:1" {
		return "tt1234567", nil
	}
	return "", errors.New("movie has no IMDb ID")
}

func (fakeMetadata) Search(_ context.Context, _ string) ([]metadata.Movie, error) {
	return []metadata.Movie{{ID: "tmdb:1", Title: "The Movie", Year: 2001, PosterURL: "https://image.example/poster.jpg"}}, nil
}

func (fakeRatings) RatingsByIMDbID(_ context.Context, imdbID string) (metadata.MovieRatings, error) {
	if imdbID != "tt1234567" {
		return metadata.MovieRatings{}, errors.New("unexpected IMDb ID")
	}
	imdb := 9.1
	rottenTomatoes := 95
	return metadata.MovieRatings{IMDb: &imdb, RottenTomatoes: &rottenTomatoes}, nil
}

func (fakeRatings) Ratings(_ context.Context, title string, year int) (metadata.MovieRatings, error) {
	if title != "The Movie" || year != 2001 {
		return metadata.MovieRatings{}, nil
	}
	imdb := 8.4
	rottenTomatoes := 91
	return metadata.MovieRatings{IMDb: &imdb, RottenTomatoes: &rottenTomatoes}, nil
}

func (fakeMetadata) Discover(_ context.Context, collection metadata.Collection) ([]metadata.Movie, error) {
	switch collection {
	case metadata.CollectionPopular:
		return []metadata.Movie{{ID: "tmdb:2", Title: "Popular Movie", Year: 2026}}, nil
	case metadata.CollectionTopRated:
		return []metadata.Movie{{ID: "tmdb:3", Title: "Top Rated Movie", Year: 1972}}, nil
	default:
		return nil, nil
	}
}

func (fakeResolver) Resolve(_ context.Context, input string) (resolver.Result, error) {
	return resolver.Result{Input: input, Candidates: []resolver.Candidate{{Title: "The Movie", Year: 2001, Confidence: 0.9}}}, nil
}

func TestResolveMovie(t *testing.T) {
	server := &Server{movieResolver: fakeResolver{}}
	request := httptest.NewRequest(http.MethodPost, "/v1/resolve", strings.NewReader(`{"query":"rough description"}`))
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "The Movie") {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestSearchCatalogReturnsMetadata(t *testing.T) {
	server := &Server{metadataProvider: fakeMetadata{}}
	request := httptest.NewRequest(http.MethodGet, "/v1/catalog/search?query=The+Movie", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "poster.jpg") {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestCatalogRatingsReturnsExternalScores(t *testing.T) {
	server := &Server{ratingsProvider: fakeRatings{}}
	request := httptest.NewRequest(http.MethodGet, "/v1/catalog/ratings?title=The+Movie&year=2001", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"imdb":8.4`) || !strings.Contains(response.Body.String(), `"rotten_tomatoes":91`) {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestCatalogRatingsUsesTMDBMovieIDForReliableLookup(t *testing.T) {
	server := &Server{metadataProvider: fakeMetadata{}, ratingsProvider: fakeRatings{}}
	request := httptest.NewRequest(http.MethodGet, "/v1/catalog/ratings?title=Localized+Movie&year=2001&media_id=tmdb%3A1", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"imdb":9.1`) || !strings.Contains(response.Body.String(), `"rotten_tomatoes":95`) {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestCatalogRatingsRequiresProvider(t *testing.T) {
	server := &Server{}
	request := httptest.NewRequest(http.MethodGet, "/v1/catalog/ratings?title=The+Movie&year=2001", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestDiscoverCatalogReturnsMovieSections(t *testing.T) {
	server := &Server{metadataProvider: fakeMetadata{}}
	request := httptest.NewRequest(http.MethodGet, "/v1/catalog/discover", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var payload struct {
		Sections []catalogSection `json:"sections"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Sections) != 2 {
		t.Fatalf("sections = %+v", payload.Sections)
	}
	if payload.Sections[0].ID != "popular" || payload.Sections[0].Items[0].Title != "Popular Movie" {
		t.Fatalf("popular = %+v", payload.Sections[0])
	}
	if payload.Sections[1].ID != "top-rated" || payload.Sections[1].Items[0].Title != "Top Rated Movie" {
		t.Fatalf("top rated = %+v", payload.Sections[1])
	}
}

func TestWatchHistoryProgressRoundTrip(t *testing.T) {
	server := &Server{historyStore: history.New(t.TempDir())}
	body := `{"media_id":"tmdb:1","title":"The Movie","year":2001,"poster_url":"https://image.example/poster.jpg","position_seconds":120,"duration_seconds":1000}`
	request := httptest.NewRequest(http.MethodPut, "/v1/watch-history", strings.NewReader(body))
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodGet, "/v1/watch-history?continue=true", nil)
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var payload struct {
		Entries []history.Entry `json:"entries"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Entries) != 1 || payload.Entries[0].ResumePosition() != 120 {
		t.Fatalf("entries = %+v", payload.Entries)
	}
}

func TestRemoveWatchHistoryClearsSavedProgress(t *testing.T) {
	store := history.New(t.TempDir())
	entry, err := store.RecordProgress(history.Entry{
		MediaID:         "tmdb:1",
		Title:           "The Movie",
		Year:            2001,
		PositionSeconds: 120,
		DurationSeconds: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{historyStore: store}
	request := httptest.NewRequest(http.MethodDelete, "/v1/watch-history/"+entry.ID, nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	entries, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("entries after removal = %+v", entries)
	}
}

func TestWatchHistoryAddsMissingMetadata(t *testing.T) {
	store := history.New(t.TempDir())
	entry, err := store.RecordProgress(history.Entry{
		Title: "The Movie", Year: 2001, PositionSeconds: 120, DurationSeconds: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{historyStore: store, metadataProvider: fakeMetadata{}}
	request := httptest.NewRequest(http.MethodGet, "/v1/watch-history?continue=true", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "poster.jpg") {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	entries, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].MediaID != "tmdb:1" || entries[0].UpdatedAt != entry.UpdatedAt {
		t.Fatalf("entries = %+v", entries)
	}
}

func TestFirstTextSubtitleOptionPrefersLiveSupportedRelease(t *testing.T) {
	manager := &fakeHLSManager{subtitleTracks: map[string][]hls.SubtitleTrack{
		"with-subtitles": {{Index: 3, Language: "en"}},
	}}
	server := &Server{
		hlsManager: manager,
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	options := []playbackOption{
		{session: &torrentstream.Session{ID: "without-subtitles"}, peers: 12},
		{session: &torrentstream.Session{ID: "with-subtitles"}, peers: 5},
	}
	if index := server.firstTextSubtitleOption(t.Context(), options); index != 1 {
		t.Fatalf("subtitle option = %d, want 1", index)
	}
}

func TestHLSAssetsAndCleanup(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.m3u8"), []byte("#EXTM3U\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "segment-000000.m4s"), []byte("segment"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "subtitle-6.vtt"), []byte("WEBVTT\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	manager := &fakeHLSManager{dir: dir}
	server := &Server{hlsManager: manager}

	request := httptest.NewRequest(http.MethodPost, "/v1/playbacks/abc/hls/subtitles/6", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || manager.subtitlePlayback != "abc" || manager.subtitleIndex != 6 {
		t.Fatalf("subtitle start status = %d, playback = %q, index = %d", response.Code, manager.subtitlePlayback, manager.subtitleIndex)
	}

	request = httptest.NewRequest(http.MethodGet, "/v1/playbacks/abc/hls/index.m3u8", nil)
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "application/vnd.apple.mpegurl" {
		t.Fatalf("status = %d, content type = %q", response.Code, response.Header().Get("Content-Type"))
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("playlist cache control = %q", response.Header().Get("Cache-Control"))
	}

	request = httptest.NewRequest(http.MethodGet, "/v1/playbacks/abc/hls/segment-000000.m4s", nil)
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("segment status = %d, cache control = %q", response.Code, response.Header().Get("Cache-Control"))
	}

	request = httptest.NewRequest(http.MethodGet, "/v1/playbacks/abc/hls/subtitle-6.vtt", nil)
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "text/vtt; charset=utf-8" {
		t.Fatalf("subtitle status = %d, content type = %q", response.Code, response.Header().Get("Content-Type"))
	}

	request = httptest.NewRequest(http.MethodDelete, "/v1/playbacks/abc/hls", nil)
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || manager.stopped != "abc" {
		t.Fatalf("status = %d, stopped = %q", response.Code, manager.stopped)
	}
}

func TestPublicRankedCandidateRemovesPrivateTorrentSources(t *testing.T) {
	candidate := publicRankedCandidate(catalog.RankedCandidate{Candidate: catalog.Candidate{
		Name:       "The Movie",
		MagnetURI:  "magnet:?xt=private",
		TorrentURL: "https://indexer.example/download?apikey=secret",
	}})
	if candidate.Candidate.MagnetURI != "" || candidate.Candidate.TorrentURL != "" {
		t.Fatalf("public candidate contains private sources: %+v", candidate.Candidate)
	}
	if candidate.Candidate.Name != "The Movie" {
		t.Fatalf("candidate name = %q", candidate.Candidate.Name)
	}
}

func TestRequestSchemeUsesForwardedProtocol(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("X-Forwarded-Proto", "https")
	if got := requestScheme(request); got != "https" {
		t.Fatalf("scheme = %q", got)
	}
}

func TestReloadIndexerConfiguration(t *testing.T) {
	called := false
	server := &Server{reloadIndexers: func() error {
		called = true
		return nil
	}}
	request := httptest.NewRequest(http.MethodPost, "/v1/indexers/reload", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if !called {
		t.Fatal("reload callback was not called")
	}
}
