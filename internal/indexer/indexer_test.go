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
	wait       bool
	canceled   chan struct{}
}

func (f protocolTestIndexer) Name() string { return f.name }

func (f protocolTestIndexer) Search(ctx context.Context, _ catalog.SearchRequest) ([]catalog.Candidate, error) {
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
