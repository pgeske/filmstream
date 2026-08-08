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
