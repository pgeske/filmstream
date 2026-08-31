package api

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pgeske/filmstream/internal/catalog"
	"github.com/pgeske/filmstream/internal/config"
	"github.com/pgeske/filmstream/internal/indexer"
)

func TestMovieAndShowReleaseSearchBothSkipIrrelevantFastIndexer(t *testing.T) {
	var movieQueries []string
	indexerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("t") {
		case "caps":
			fmt.Fprint(w, `<?xml version="1.0"?><caps><searching><search available="yes" supportedParams="q"/><movie-search available="yes" supportedParams="q"/><tv-search available="yes" supportedParams="q,season,ep"/></searching></caps>`)
		case "movie":
			movieQueries = append(movieQueries, r.URL.Query().Get("q"))
			fmt.Fprint(w, `<?xml version="1.0"?><rss><channel><item><title>Dune.Part.Two.2024.1080p.WEB-DL.H264-A</title><guid>movie-a</guid><enclosure url="/movie-a" length="7000000000" type="application/x-bittorrent"/></item><item><title>Dune.Part.Two.2024.1080p.BluRay.x265-B</title><guid>movie-b</guid><enclosure url="/movie-b" length="8000000000" type="application/x-bittorrent"/></item></channel></rss>`)
		case "tvsearch":
			fmt.Fprint(w, `<?xml version="1.0"?><rss><channel><item><title>Dune.Prophecy.S01.Complete.1080p.WEB.H264-PACK</title><guid>show-pack</guid><enclosure url="/show-pack" length="9000000000" type="application/x-bittorrent"/></item></channel></rss>`)
		case "search":
			query := r.URL.Query().Get("q")
			if strings.HasSuffix(query, "S01E01") {
				fmt.Fprint(w, `<?xml version="1.0"?><rss><channel><item><title>Dune.Prophecy.S01E01.1080p.WEB.H264-EPISODE</title><guid>show-episode</guid><enclosure url="/show-episode" length="1000000000" type="application/x-bittorrent"/></item></channel></rss>`)
				return
			}
			fmt.Fprint(w, `<?xml version="1.0"?><rss><channel><item><title>Dune.Prophecy.S01.Complete.1080p.WEB.H264-PACK</title><guid>show-pack</guid><enclosure url="/show-pack" length="9000000000" type="application/x-bittorrent"/></item></channel></rss>`)
		default:
			http.Error(w, "unsupported", http.StatusBadRequest)
		}
	}))
	defer indexerServer.Close()

	registry, err := indexer.NewRegistry([]config.Indexer{
		{Name: "fast-fixtures", Type: "open_media", Endpoint: "https://example.test/torrents"},
		{Name: "release-indexer", Type: "torznab", Endpoint: indexerServer.URL},
	})
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	server := &Server{indexers: registry, logger: slog.New(slog.NewTextHandler(&logs, nil))}
	preferences := catalog.Preferences{
		Resolution: "1080p", Codecs: []string{"h264", "h265"},
		MaxSizeBytes: 60 << 30, StreamingOptimized: true,
	}

	movies, err := server.searchAndRank(t.Context(), catalog.SearchRequest{
		Query: "Dune: Part Two", Year: 2024, MediaType: "movie", Preferences: preferences,
	}, "Dune Part II", catalog.ProtocolTorrent)
	if err != nil {
		t.Fatal(err)
	}
	shows, err := server.searchAndRank(t.Context(), catalog.SearchRequest{
		Query: "Dune Prophecy", MediaType: "show", SeasonNumber: 1, EpisodeNumber: 1,
		Preferences: preferences,
	}, "", catalog.ProtocolTorrent)
	if err != nil {
		t.Fatal(err)
	}
	if len(movies) != 2 || !strings.HasPrefix(movies[0].Candidate.Name, "Dune.Part.Two.2024") {
		t.Fatalf("movie ranking = %+v", movies)
	}
	if len(shows) != 2 || !strings.HasPrefix(shows[0].Candidate.Name, "Dune.Prophecy") {
		t.Fatalf("show ranking = %+v", shows)
	}
	if !slices.Equal(movieQueries, []string{"Dune Part Two"}) {
		t.Fatalf("movie queries = %q; the original-title fallback should stop after a valid catalog-title result", movieQueries)
	}
	if output := logs.String(); !strings.Contains(output, "raw_candidates_by_indexer") ||
		!strings.Contains(output, "rejection_reasons") || !strings.Contains(output, "year_mismatch:4") {
		t.Fatalf("release diagnostics = %q", output)
	}
}

func TestTorrentMoviePrewarmCachesPopularMovieRankingsWithoutMounting(t *testing.T) {
	var searches atomic.Int32
	indexerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("t") {
		case "caps":
			fmt.Fprint(w, `<?xml version="1.0"?><caps><searching><search available="yes" supportedParams="q"/><movie-search available="yes" supportedParams="q"/></searching></caps>`)
		case "movie":
			searches.Add(1)
			switch r.URL.Query().Get("q") {
			case "Pirates of the Caribbean Dead Men Tell No Tales":
				fmt.Fprint(w, `<?xml version="1.0"?><rss><channel><item><title>Pirates.of.the.Caribbean.Dead.Men.Tell.No.Tales.2017.2160p.DV.HEVC.REMUX</title><guid>pirates-bad</guid><enclosure url="/pirates-bad" length="50000000000" type="application/x-bittorrent"/></item><item><title>Pirates.of.the.Caribbean.Dead.Men.Tell.No.Tales.2017.1080p.BluRay.x265-Ralphy</title><guid>pirates-good</guid><enclosure url="/pirates-good" length="6000000000" type="application/x-bittorrent"/></item></channel></rss>`)
			case "Dune Part Two":
				fmt.Fprint(w, `<?xml version="1.0"?><rss><channel><item><title>Dune.Part.Two.2024.2160p.DV.HEVC.REMUX</title><guid>dune-bad</guid><enclosure url="/dune-bad" length="50000000000" type="application/x-bittorrent"/></item><item><title>Dune.Part.Two.2024.1080p.WEB-DL.H264-GROUP</title><guid>dune-good</guid><enclosure url="/dune-good" length="7000000000" type="application/x-bittorrent"/></item></channel></rss>`)
			default:
				fmt.Fprint(w, `<?xml version="1.0"?><rss><channel></channel></rss>`)
			}
		default:
			http.Error(w, "unexpected fallback", http.StatusBadRequest)
		}
	}))
	defer indexerServer.Close()
	registry, err := indexer.NewRegistry([]config.Indexer{{
		Name: "torrent", Type: "torznab", Endpoint: indexerServer.URL,
	}})
	if err != nil {
		t.Fatal(err)
	}
	preferences := catalog.Preferences{
		Resolution: "1080p", Codecs: []string{"h264", "h265"},
		MaxSizeBytes: 60 << 30, StreamingOptimized: true,
	}
	server := newReleaseOnlyTestServer(registry, preferences)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	server.prewarmContext = ctx

	tests := []CreatePlaybackRequest{
		{MediaID: "tmdb:166426", MediaType: "movie", Query: "Pirates of the Caribbean: Dead Men Tell No Tales", Year: 2017},
		{MediaID: "tmdb:693134", MediaType: "movie", Query: "Dune: Part Two", Year: 2024},
	}
	for _, request := range tests {
		body := fmt.Sprintf(`{"media_id":%q,"media_type":"movie","query":%q,"year":%d,"preferences":{"streaming_optimized":true}}`, request.MediaID, request.Query, request.Year)
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/playbacks/prewarm", strings.NewReader(body)))
		if response.Code != http.StatusAccepted || !strings.Contains(response.Body.String(), "finding_releases") {
			t.Fatalf("prewarm %s: status = %d, body = %s", request.MediaID, response.Code, response.Body.String())
		}
		request.Preferences = preferences
		ranked, found := server.cachedReleaseSearch(t.Context(), request)
		if !found || len(ranked) != 1 || ranked[0].Candidate.Resolution != "1080p" {
			t.Fatalf("prewarm %s: found = %v, ranked = %+v", request.MediaID, found, ranked)
		}
	}
	if searches.Load() != 2 || len(server.prewarmStates) != 0 {
		t.Fatalf("searches = %d, playback prewarms = %d", searches.Load(), len(server.prewarmStates))
	}
}

func TestExplicitPlayJoinsConcurrentMovieReleasePrewarm(t *testing.T) {
	var searches atomic.Int32
	searchStarted := make(chan struct{})
	finishSearch := make(chan struct{})
	indexerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("t") {
		case "caps":
			fmt.Fprint(w, `<?xml version="1.0"?><caps><searching><search available="yes" supportedParams="q"/><movie-search available="yes" supportedParams="q"/></searching></caps>`)
		case "movie":
			searches.Add(1)
			close(searchStarted)
			<-finishSearch
			fmt.Fprint(w, `<?xml version="1.0"?><rss><channel></channel></rss>`)
		case "search":
			searches.Add(1)
			fmt.Fprint(w, `<?xml version="1.0"?><rss><channel></channel></rss>`)
		default:
			http.Error(w, "unsupported", http.StatusBadRequest)
		}
	}))
	defer indexerServer.Close()
	registry, err := indexer.NewRegistry([]config.Indexer{{
		Name: "torrent", Type: "torznab", Endpoint: indexerServer.URL,
	}})
	if err != nil {
		t.Fatal(err)
	}
	preferences := catalog.Preferences{Codecs: []string{"h264", "h265"}, StreamingOptimized: true}
	server := newReleaseOnlyTestServer(registry, preferences)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	server.prewarmContext = ctx
	body := `{"media_id":"tmdb:693134","media_type":"movie","query":"Dune: Part Two","year":2024,"preferences":{"streaming_optimized":true}}`

	prewarm := httptest.NewRecorder()
	server.Handler().ServeHTTP(prewarm, httptest.NewRequest(http.MethodPost, "/v1/playbacks/prewarm", strings.NewReader(body)))
	if prewarm.Code != http.StatusAccepted {
		t.Fatalf("prewarm status = %d, body = %s", prewarm.Code, prewarm.Body.String())
	}
	select {
	case <-searchStarted:
	case <-time.After(time.Second):
		t.Fatal("prewarm release search did not start")
	}

	explicitDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/playbacks", strings.NewReader(body)))
		explicitDone <- response
	}()
	time.Sleep(20 * time.Millisecond)
	if searches.Load() != 1 {
		t.Fatalf("concurrent Play started %d searches instead of joining the prewarm", searches.Load())
	}
	close(finishSearch)
	response := <-explicitDone
	if response.Code != http.StatusNotFound || searches.Load() != 3 {
		t.Fatalf("explicit status = %d, searches = %d, body = %s", response.Code, searches.Load(), response.Body.String())
	}
}

func TestExhaustiveEmptyMoviePrewarmIsShortLivedAndExplicitPlayDoesNotRepeatIt(t *testing.T) {
	var searches atomic.Int32
	indexerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("t") {
		case "caps":
			fmt.Fprint(w, `<?xml version="1.0"?><caps><searching><search available="yes" supportedParams="q"/><movie-search available="yes" supportedParams="q"/></searching></caps>`)
		case "movie", "search":
			searches.Add(1)
			fmt.Fprint(w, `<?xml version="1.0"?><rss><channel></channel></rss>`)
		default:
			http.Error(w, "unsupported", http.StatusBadRequest)
		}
	}))
	defer indexerServer.Close()
	registry, err := indexer.NewRegistry([]config.Indexer{{
		Name: "torrent", Type: "torznab", Endpoint: indexerServer.URL,
	}})
	if err != nil {
		t.Fatal(err)
	}
	preferences := catalog.Preferences{Codecs: []string{"h264", "h265"}, StreamingOptimized: true}
	server := newReleaseOnlyTestServer(registry, preferences)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	server.prewarmContext = ctx
	body := `{"media_id":"tmdb:693134","media_type":"movie","query":"Dune: Part Two","year":2024,"preferences":{"streaming_optimized":true}}`

	prewarm := func() {
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/playbacks/prewarm", strings.NewReader(body)))
		if response.Code != http.StatusAccepted {
			t.Fatalf("prewarm status = %d, body = %s", response.Code, response.Body.String())
		}
	}
	prewarm()
	request := CreatePlaybackRequest{
		MediaID: "tmdb:693134", MediaType: "movie", Query: "Dune: Part Two", Year: 2024,
		Preferences: preferences,
	}
	ranked, found := server.cachedReleaseSearch(t.Context(), request)
	if !found || len(ranked) != 0 || searches.Load() != 3 {
		t.Fatalf("found = %v, ranked = %+v, exhaustive searches = %d", found, ranked, searches.Load())
	}
	server.releaseSearchMu.Lock()
	expiresIn := time.Until(server.releaseSearches[releaseSearchKey(request)].expiresAt)
	server.releaseSearchMu.Unlock()
	if expiresIn <= 0 || expiresIn > releaseSearchNegativeTTL+time.Second {
		t.Fatalf("negative cache expires in %s", expiresIn)
	}

	prewarm()
	explicit := httptest.NewRecorder()
	server.Handler().ServeHTTP(explicit, httptest.NewRequest(http.MethodPost, "/v1/playbacks", strings.NewReader(body)))
	if explicit.Code != http.StatusNotFound || !strings.Contains(explicit.Body.String(), "no matching streaming candidates found") {
		t.Fatalf("explicit status = %d, body = %s", explicit.Code, explicit.Body.String())
	}
	if searches.Load() != 3 {
		t.Fatalf("cached exhaustive miss repeated %d indexer requests", searches.Load())
	}
}

func newReleaseOnlyTestServer(registry *indexer.Registry, preferences catalog.Preferences) *Server {
	return &Server{
		indexers: registry, playbackSourceMode: config.PlaybackSourceTorrentOnly,
		defaults: preferences, logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		prewarmStates:    make(map[string]*playbackPrewarmState),
		claimedPlaybacks: make(map[string]claimedPlayback), releaseSearches: make(map[string]*releaseSearchState),
	}
}

func TestReleaseSearchKeyIncludesMovieMetadataAndCanonicalPreferences(t *testing.T) {
	first := CreatePlaybackRequest{
		MediaID: "tmdb:1", MediaType: "movie", Query: "Catalog Title", OriginalTitle: "Original Title", Year: 2024,
		Preferences: catalog.Preferences{Codecs: []string{"h265", "H264"}, Languages: []string{"English", "en"}},
	}
	reordered := first
	reordered.Preferences.Codecs = []string{"h264", "H265"}
	reordered.Preferences.Languages = []string{"en", "english"}
	if releaseSearchKey(first) != releaseSearchKey(reordered) {
		t.Fatal("equivalent preference order produced a different release-search key")
	}
	differentYear := first
	differentYear.Year++
	if releaseSearchKey(first) == releaseSearchKey(differentYear) {
		t.Fatal("movie year was omitted from the release-search key")
	}
	differentOriginalTitle := first
	differentOriginalTitle.OriginalTitle = "Another Alias"
	if releaseSearchKey(first) == releaseSearchKey(differentOriginalTitle) {
		t.Fatal("original title was omitted from the release-search key")
	}
}
