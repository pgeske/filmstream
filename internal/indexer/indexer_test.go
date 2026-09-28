package indexer

import (
	"context"
	"strings"
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

func TestSearchSkipsFastIrrelevantMovieResults(t *testing.T) {
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
	candidates, err := registry.Search(t.Context(), request, SearchPolicy{
		Protocol: catalog.ProtocolTorrent, Settle: time.Millisecond,
		Acceptable: func(candidates []catalog.Candidate) bool {
			return len(catalog.Rank(request, candidates)) > 0
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ranked, diagnostics := catalog.RankWithDiagnostics(request, candidates)
	if len(candidates) != 6 || len(ranked) != 2 || diagnostics.RejectionReasons["year_mismatch"] != 4 {
		t.Fatalf("candidates = %+v, ranked = %+v, diagnostics = %+v", candidates, ranked, diagnostics)
	}
}

func TestSearchWaitsForSlowerIndexersOnlyWithinSettleWindow(t *testing.T) {
	hungCanceled := make(chan struct{})
	registry := &Registry{
		indexers: make(map[string]Indexer),
		ordered: []Indexer{
			protocolTestIndexer{name: "fast-public", candidates: []catalog.Candidate{
				{Name: "Movie 2024 1080p H264-A", Protocol: catalog.ProtocolTorrent},
			}},
			protocolTestIndexer{name: "slower-private", delay: 30 * time.Millisecond, candidates: []catalog.Candidate{
				{Name: "Movie 2024 1080p H264-B", Protocol: catalog.ProtocolTorrent},
			}},
			protocolTestIndexer{name: "hung", wait: true, canceled: hungCanceled},
		},
	}
	started := time.Now()
	candidates, err := registry.Search(t.Context(), catalog.SearchRequest{Query: "Movie", Year: 2024}, SearchPolicy{
		Protocol: catalog.ProtocolTorrent, Settle: 200 * time.Millisecond, Deadline: 10 * time.Second,
		Acceptable: func(candidates []catalog.Candidate) bool { return len(candidates) > 0 },
	})
	if err != nil || len(candidates) != 2 {
		t.Fatalf("candidates = %+v, error = %v; the slower indexer inside the settle window must be included", candidates, err)
	}
	if elapsed := time.Since(started); elapsed < 200*time.Millisecond || elapsed > 2*time.Second {
		t.Fatalf("search returned after %s, want the settle window rather than the hung indexer or deadline", elapsed)
	}
	select {
	case <-hungCanceled:
	case <-time.After(time.Second):
		t.Fatal("hung indexer was not canceled")
	}
}

func TestSearchDeadlineReturnsPartialCandidatesWithTimeoutError(t *testing.T) {
	registry := &Registry{
		indexers: make(map[string]Indexer),
		ordered: []Indexer{
			protocolTestIndexer{name: "fast", candidates: []catalog.Candidate{
				{Name: "Unrelated 2024 1080p H264", Protocol: catalog.ProtocolTorrent},
			}},
			protocolTestIndexer{name: "hung", wait: true, canceled: make(chan struct{})},
		},
	}
	candidates, err := registry.Search(t.Context(), catalog.SearchRequest{Query: "Movie"}, SearchPolicy{
		Settle: time.Second, Deadline: 50 * time.Millisecond,
		Acceptable: func([]catalog.Candidate) bool { return false },
	})
	if len(candidates) != 1 || err == nil || !strings.Contains(err.Error(), "hung timed out") {
		t.Fatalf("candidates = %+v, error = %v", candidates, err)
	}
}

func TestSearchStampsIndexerPrivacyAndNormalizedMagnetInfoHash(t *testing.T) {
	const hexHash = "0123456789abcdef0123456789abcdef01234567"
	registry := &Registry{
		indexers: make(map[string]Indexer),
		private:  map[string]bool{"tracker": true},
		ordered: []Indexer{protocolTestIndexer{name: "tracker", candidates: []catalog.Candidate{
			{Name: "Hex", Protocol: catalog.ProtocolTorrent, MagnetURI: "magnet:?xt=urn:btih:" + strings.ToUpper(hexHash)},
			// The same hash in base32, as some magnet links encode it.
			{Name: "Base32", Protocol: catalog.ProtocolTorrent, MagnetURI: "magnet:?dn=x&xt=urn:btih:aerukz4jvpg66ajdivtytk6n54asgrlh"},
		}}},
	}
	candidates, err := registry.Search(t.Context(), catalog.SearchRequest{Query: "Movie"}, SearchPolicy{})
	if err != nil || len(candidates) != 2 {
		t.Fatalf("candidates = %+v, error = %v", candidates, err)
	}
	for _, candidate := range candidates {
		if candidate.Indexer != "tracker" || !candidate.Private || candidate.InfoHash != hexHash {
			t.Fatalf("candidate = %+v", candidate)
		}
	}
}
