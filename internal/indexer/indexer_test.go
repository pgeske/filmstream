package indexer

import (
	"context"
	"testing"
	"time"

	"github.com/pgeske/filmstream/internal/catalog"
)

type protocolTestIndexer struct {
	name       string
	candidates []catalog.Candidate
	delay      time.Duration
	wait       bool
	canceled   chan struct{}
}

func (f protocolTestIndexer) Name() string { return f.name }

func (f protocolTestIndexer) Search(ctx context.Context, _ catalog.SearchRequest) ([]catalog.Candidate, error) {
	if f.delay > 0 {
		timer := time.NewTimer(f.delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			if f.canceled != nil {
				close(f.canceled)
			}
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	if !f.wait {
		return f.candidates, nil
	}
	<-ctx.Done()
	close(f.canceled)
	return nil, ctx.Err()
}

func (f protocolTestIndexer) Resolve(context.Context, catalog.Candidate) (Source, error) {
	return Source{}, nil
}

func TestSearchProtocolUntilSkipsFastIrrelevantMovieResults(t *testing.T) {
	seeders := 50
	request := catalog.SearchRequest{
		Query: "Dune: Part Two", Year: 2024, MediaType: "movie",
		Preferences: catalog.Preferences{
			Resolution: "1080p", Codecs: []string{"h264", "h265"}, StreamingOptimized: true,
		},
	}
	registry := &Registry{
		indexers: make(map[string]Indexer),
		ordered: []Indexer{
			protocolTestIndexer{name: "fast-fixture", candidates: []catalog.Candidate{
				{Name: "Sintel", Year: 2010, Protocol: catalog.ProtocolTorrent, Trusted: true},
				{Name: "Big Buck Bunny", Year: 2008, Protocol: catalog.ProtocolTorrent, Trusted: true},
				{Name: "Tears of Steel", Year: 2012, Protocol: catalog.ProtocolTorrent, Trusted: true},
				{Name: "Cosmos Laundromat", Year: 2015, Protocol: catalog.ProtocolTorrent, Trusted: true},
			}},
			protocolTestIndexer{name: "movie-indexer", delay: 10 * time.Millisecond, candidates: []catalog.Candidate{
				{Name: "Dune Part Two 2024 1080p WEB-DL H264-A", Protocol: catalog.ProtocolTorrent, Seeders: &seeders},
				{Name: "Dune Part Two 2024 1080p BluRay x265-B", Protocol: catalog.ProtocolTorrent, Seeders: &seeders},
			}},
		},
	}
	candidates, err := registry.SearchProtocolUntil(t.Context(), request, catalog.ProtocolTorrent, func(candidates []catalog.Candidate) bool {
		return len(catalog.Rank(request, candidates)) >= 2
	})
	if err != nil {
		t.Fatal(err)
	}
	ranked, diagnostics := catalog.RankWithDiagnostics(request, candidates)
	if len(candidates) != 6 || len(ranked) != 2 || diagnostics.RejectionReasons["year_mismatch"] != 4 {
		t.Fatalf("candidates = %+v, ranked = %+v, diagnostics = %+v", candidates, ranked, diagnostics)
	}
}

func TestSearchProtocolUntilDoesNotWaitForSlowIndexersAfterEnoughMatches(t *testing.T) {
	slowCanceled := make(chan struct{})
	registry := &Registry{
		indexers: make(map[string]Indexer),
		ordered: []Indexer{
			protocolTestIndexer{name: "primary", candidates: []catalog.Candidate{
				{Name: "Movie 2024 1080p H264-A", Protocol: catalog.ProtocolTorrent},
				{Name: "Movie 2024 1080p H264-B", Protocol: catalog.ProtocolTorrent},
			}},
			protocolTestIndexer{name: "slow", wait: true, canceled: slowCanceled},
		},
	}
	started := time.Now()
	candidates, err := registry.SearchProtocolUntil(
		t.Context(), catalog.SearchRequest{Query: "Movie", Year: 2024}, catalog.ProtocolTorrent,
		func(candidates []catalog.Candidate) bool { return len(candidates) >= 2 },
	)
	if err != nil || len(candidates) != 2 {
		t.Fatalf("candidates = %+v, error = %v", candidates, err)
	}
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("primary search waited for slow indexer: %s", elapsed)
	}
	select {
	case <-slowCanceled:
	case <-time.After(time.Second):
		t.Fatal("slower indexer was not canceled")
	}
}

func TestSearchFirstProtocolReturnsWithoutWaitingForOtherIndexers(t *testing.T) {
	slowCanceled := make(chan struct{})
	registry := &Registry{
		indexers: make(map[string]Indexer),
		ordered: []Indexer{
			protocolTestIndexer{name: "slow-torrent", wait: true, canceled: slowCanceled},
			protocolTestIndexer{name: "fast-usenet", candidates: []catalog.Candidate{{
				Name: "Movie.1080p", Protocol: catalog.ProtocolUsenet,
			}}},
		},
	}

	candidates, err := registry.SearchFirstProtocol(t.Context(), catalog.SearchRequest{Query: "Movie"}, catalog.ProtocolUsenet)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].Indexer != "fast-usenet" {
		t.Fatalf("candidates = %+v", candidates)
	}
	select {
	case <-slowCanceled:
	case <-time.After(time.Second):
		t.Fatal("slower indexer was not canceled")
	}
}
