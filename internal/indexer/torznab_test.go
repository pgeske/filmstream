package indexer

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pgeske/filmstream/internal/catalog"
)

func TestTorznabCapabilitiesSearchAndResolve(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("apikey") != "secret" {
			http.Error(w, "missing API key", http.StatusUnauthorized)
			return
		}
		switch r.URL.Query().Get("t") {
		case "caps":
			fmt.Fprint(w, `<?xml version="1.0"?><caps><searching><search available="yes" supportedParams="q"/><movie-search available="yes" supportedParams="q,year"/></searching></caps>`)
		case "movie":
			if r.URL.Query().Get("q") != "Sintel" || r.URL.Query().Get("year") != "2010" {
				t.Errorf("unexpected search query: %s", r.URL.RawQuery)
			}
			fmt.Fprint(w, `<?xml version="1.0"?><rss xmlns:torznab="http://torznab.com/schemas/2015/feed"><channel><item><title>Sintel.2010.1080p.x265</title><guid>release-1</guid><enclosure url="/download/1" length="2147483648" type="application/x-bittorrent"/><torznab:attr name="seeders" value="25"/><torznab:attr name="peers" value="31"/><torznab:attr name="downloadvolumefactor" value="0"/><torznab:attr name="uploadvolumefactor" value="2"/><torznab:attr name="language" value="en"/></item></channel></rss>`)
		default:
			http.Error(w, "unsupported", http.StatusBadRequest)
		}
	}))
	defer server.Close()

	configured, err := NewTorznab("test", server.URL+"/1/api", "secret", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	candidates, err := configured.Search(context.Background(), catalog.SearchRequest{Query: "Sintel", Year: 2010})
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 {
		t.Fatalf("candidates = %+v", candidates)
	}
	candidate := candidates[0]
	if candidate.Seeders == nil || *candidate.Seeders != 25 || candidate.Leechers == nil || *candidate.Leechers != 6 {
		t.Fatalf("swarm values = %+v", candidate)
	}
	if candidate.DownloadVolumeFactor == nil || *candidate.DownloadVolumeFactor != 0 {
		t.Fatalf("download factor = %+v", candidate.DownloadVolumeFactor)
	}
	if candidate.TorrentURL != server.URL+"/download/1" {
		t.Fatalf("torrent URL = %q", candidate.TorrentURL)
	}
	source, err := configured.Resolve(context.Background(), candidate)
	if err != nil {
		t.Fatal(err)
	}
	if source.TorrentURL != candidate.TorrentURL {
		t.Fatalf("source = %+v", source)
	}
}

func TestTorznabNormalizesPunctuationInSearchQueries(t *testing.T) {
	if got := torznabQuery("The Good, the Bad and the Ugly"); got != "The Good the Bad and the Ugly" {
		t.Fatalf("normalized query = %q", got)
	}
	if got := torznabQuery("Harry Potter and the Philosopher's Stone"); got != "Harry Potter and the Philosophers Stone" {
		t.Fatalf("normalized possessive query = %q", got)
	}
	if got := broadMovieQuery("The Good, the Bad and the Ugly", 1966); got != "The Good the 1966" {
		t.Fatalf("broad query = %q", got)
	}
}

func TestTorznabAugmentsSparseMovieResultsWithBasicSearch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("t") {
		case "caps":
			fmt.Fprint(w, `<?xml version="1.0"?><caps><searching><search available="yes" supportedParams="q"/><movie-search available="yes" supportedParams="q,year"/></searching></caps>`)
		case "movie":
			if r.URL.Query().Get("q") != "Movie Part One" {
				t.Errorf("movie query = %q", r.URL.Query().Get("q"))
			}
			fmt.Fprint(w, `<?xml version="1.0"?><rss><channel><item><title>Movie.2001.1080p.x264-OLD</title><guid>old</guid><enclosure url="/old" length="1000" type="application/x-nzb"/></item></channel></rss>`)
		case "search":
			if r.URL.Query().Get("q") != "Movie Part One 2001" {
				t.Errorf("broad query = %q", r.URL.Query().Get("q"))
			}
			fmt.Fprint(w, `<?xml version="1.0"?><rss><channel><item><title>Movie.2001.1080p.x265-NEW</title><guid>new</guid><pubDate>Tue, 30 Sep 2025 12:00:00 +0000</pubDate><enclosure url="/new" length="1000" type="application/x-nzb"/></item></channel></rss>`)
		default:
			http.Error(w, "unsupported", http.StatusBadRequest)
		}
	}))
	defer server.Close()

	configured, err := NewTorznab("test", server.URL+"/api", "", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	candidates, err := configured.Search(t.Context(), catalog.SearchRequest{Query: "Movie, Part One", Year: 2001})
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 2 || candidates[0].ID != "old" || candidates[1].ID != "new" || candidates[1].PublishedUnix == 0 {
		t.Fatalf("candidates = %+v", candidates)
	}
}

func TestTorznabTVSearchIncludesSeasonPacksAndEpisodeFallback(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("t") {
		case "caps":
			fmt.Fprint(w, `<?xml version="1.0"?><caps><searching><search available="yes" supportedParams="q"/><tv-search available="yes" supportedParams="q,season,ep"/></searching></caps>`)
		case "tvsearch":
			if r.URL.Query().Get("season") != "1" || r.URL.Query().Get("ep") != "" {
				t.Errorf("TV search query = %q", r.URL.RawQuery)
			}
			fmt.Fprint(w, `<?xml version="1.0"?><rss><channel><item><title>Example.Show.S01.Complete.1080p</title><guid>pack</guid><enclosure url="/pack" length="1000" type="application/x-bittorrent"/></item></channel></rss>`)
		case "search":
			title := "Example.Show.S01E02.1080p"
			guid := "episode"
			if r.URL.Query().Get("q") == "Example Show S01" {
				title = "Example.Show.S01.Complete.1080p"
				guid = "pack"
			}
			fmt.Fprintf(w, `<?xml version="1.0"?><rss><channel><item><title>%s</title><guid>%s</guid><enclosure url="/%s" length="1000" type="application/x-bittorrent"/></item></channel></rss>`, title, guid, guid)
		default:
			http.Error(w, "unsupported", http.StatusBadRequest)
		}
	}))
	defer server.Close()

	configured, err := NewTorznab("test", server.URL+"/api", "", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	candidates, err := configured.Search(t.Context(), catalog.SearchRequest{
		Query: "Example Show", MediaType: "show", SeasonNumber: 1, EpisodeNumber: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 2 || candidates[0].ID != "pack" || candidates[1].ID != "episode" {
		t.Fatalf("candidates = %+v", candidates)
	}
}

func TestTorznabRecognizesUsenetEnclosure(t *testing.T) {
	configured, err := NewTorznab("test", "https://indexer.example/6/api", "secret", http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	item := torznabItem{Title: "Sintel.2010.1080p.WEB.H264-GROUP", GUID: "release-1"}
	item.Enclosure.URL = "/download/1"
	item.Enclosure.Length = "607836000"
	item.Enclosure.Type = "application/x-nzb"
	candidate, ok := configured.candidate(item)
	if !ok {
		t.Fatal("candidate was rejected")
	}
	if candidate.Protocol != catalog.ProtocolUsenet || candidate.NZBURL != "https://indexer.example/download/1" || candidate.TorrentURL != "" {
		t.Fatalf("candidate = %+v", candidate)
	}
	source, err := configured.Resolve(context.Background(), candidate)
	if err != nil {
		t.Fatal(err)
	}
	if source.Protocol != catalog.ProtocolUsenet || source.NZBURL != candidate.NZBURL {
		t.Fatalf("source = %+v", source)
	}
}

func TestTorznabHandlesProtocolError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `<error code="100" description="Incorrect credentials"/>`)
	}))
	defer server.Close()
	configured, err := NewTorznab("test", server.URL, "secret", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := configured.Capabilities(context.Background()); err == nil {
		t.Fatal("expected a protocol error")
	}
}

func TestTorznabRejectsAPIKeyInURL(t *testing.T) {
	_, err := NewTorznab("test", "https://example.test/api?apikey=secret", "", http.DefaultClient)
	if err == nil {
		t.Fatal("expected an error")
	}
}
