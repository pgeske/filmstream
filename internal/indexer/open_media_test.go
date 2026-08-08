package indexer

import (
	"context"
	"testing"

	"github.com/pgeske/filmstream/internal/catalog"
)

func TestOpenMediaProvidesLicensedFixture(t *testing.T) {
	indexer := NewOpenMedia("open", "https://example.test/torrents")
	candidates, err := indexer.Search(context.Background(), catalog.SearchRequest{Query: "Sintel"})
	if err != nil {
		t.Fatal(err)
	}
	ranked := catalog.Rank(catalog.SearchRequest{Query: "Sintel"}, candidates)
	if len(ranked) != 1 {
		t.Fatalf("ranked candidates = %+v", ranked)
	}
	candidate := ranked[0].Candidate
	if !candidate.Trusted || candidate.TorrentURL != "https://example.test/torrents/sintel.torrent" {
		t.Fatalf("candidate = %+v", candidate)
	}
}
