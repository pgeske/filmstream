package indexer

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pgeske/filmstream/internal/catalog"
)

func TestInternetArchiveSearchAndResolve(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/advancedsearch.php":
			if got := r.URL.Query().Get("q"); got == "" {
				t.Error("search query is empty")
			}
			fmt.Fprint(w, `{"response":{"docs":[{"identifier":"sintel","title":"Sintel","year":2010,"language":"English","downloads":1234}]}}`)
		case "/metadata/sintel":
			fmt.Fprint(w, `{"files":[{"name":"sintel_archive.torrent","format":"Archive BitTorrent"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	indexer := NewInternetArchive("ia", server.URL, server.Client())
	candidates, err := indexer.Search(context.Background(), catalog.SearchRequest{Query: "Sintel", Year: 2010})
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].Name != "Sintel" {
		t.Fatalf("candidates = %+v", candidates)
	}
	source, err := indexer.Resolve(context.Background(), candidates[0])
	if err != nil {
		t.Fatal(err)
	}
	want := server.URL + "/download/sintel/sintel_archive.torrent"
	if source.TorrentURL != want {
		t.Fatalf("torrent URL = %q, want %q", source.TorrentURL, want)
	}
}
