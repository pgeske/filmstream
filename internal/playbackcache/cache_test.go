package playbackcache

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pgeske/filmstream/internal/catalog"
)

func TestSaveAndLookupCachedPlayback(t *testing.T) {
	store := New(t.TempDir())
	selected := catalog.RankedCandidate{
		Candidate: catalog.Candidate{
			Name:       "The Movie 2001 1080p x264",
			MagnetURI:  "magnet:?xt=private",
			TorrentURL: "https://indexer.example/download?apikey=secret",
		},
		Score: 900,
	}
	entry, err := store.Save("tmdb:1", "The Movie", 2001, selected, []byte("torrent metainfo"))
	if err != nil {
		t.Fatal(err)
	}
	if entry.Selected.Candidate.MagnetURI != "" || entry.Selected.Candidate.TorrentURL != "" {
		t.Fatal("cached selection retained a private source URL")
	}
	for _, path := range []string{store.path, entry.TorrentPath} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("%s mode = %o, want 600", path, got)
		}
	}

	cached, ok, err := store.Lookup("tmdb:1", "Different Title", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || cached.ID != entry.ID || cached.Selected.Candidate.Name != selected.Candidate.Name {
		t.Fatalf("cached entry = %+v, found = %v", cached, ok)
	}
	contents, err := os.ReadFile(cached.TorrentPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "torrent metainfo" {
		t.Fatalf("torrent contents = %q", contents)
	}
	cacheJSON, err := os.ReadFile(store.path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(cacheJSON), "magnet:?xt=private") || strings.Contains(string(cacheJSON), "apikey=secret") {
		t.Fatal("playback cache JSON contains a private torrent source")
	}
}

func TestSaveAndLookupCachedUsenetPlayback(t *testing.T) {
	store := New(t.TempDir())
	selected := catalog.RankedCandidate{
		Candidate: catalog.Candidate{
			ID:       "release-1",
			Indexer:  "usenet",
			Name:     "The Movie 2001 1080p x264",
			Protocol: catalog.ProtocolUsenet,
			NZBURL:   "https://indexer.example/download?apikey=secret",
		},
		Score: 1200,
	}
	nzb := []byte(`<?xml version="1.0"?><nzb/>`)
	entry, err := store.SaveUsenet("tmdb:1", "The Movie", 2001, selected, nzb)
	if err != nil {
		t.Fatal(err)
	}
	if entry.Selected.Candidate.NZBURL != "" {
		t.Fatal("cached Usenet selection retained a private source URL")
	}
	for _, path := range []string{store.usenetPath, entry.NZBPath} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("%s mode = %o, want 600", path, got)
		}
	}

	cached, ok, err := store.LookupUsenet("tmdb:1", "Different Title", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || cached.ID != entry.ID || cached.Selected.Candidate.Name != selected.Candidate.Name {
		t.Fatalf("cached entry = %+v, found = %v", cached, ok)
	}
	contents, err := os.ReadFile(cached.NZBPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != string(nzb) {
		t.Fatalf("NZB contents = %q", contents)
	}
	cacheJSON, err := os.ReadFile(store.usenetPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(cacheJSON), "apikey=secret") {
		t.Fatal("Usenet playback cache JSON contains a private source URL")
	}
}

func TestSaveAndLoadUsenetFailures(t *testing.T) {
	store := New(t.TempDir())
	future := time.Now().Add(time.Hour).UTC()
	if err := store.SaveUsenetFailures(map[string]time.Time{
		"usenet:active":  future,
		"usenet:expired": time.Now().Add(-time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	failures, err := store.LoadUsenetFailures()
	if err != nil {
		t.Fatal(err)
	}
	if len(failures) != 1 || failures["usenet:active"] != future {
		t.Fatalf("failures = %+v", failures)
	}
	info, err := os.Stat(store.usenetFailuresPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("failure cache mode = %o", info.Mode().Perm())
	}
}

func TestLookupFallsBackToTitleAndYear(t *testing.T) {
	store := New(t.TempDir())
	if _, err := store.Save("", "The Movie", 2001, catalog.RankedCandidate{}, []byte("torrent")); err != nil {
		t.Fatal(err)
	}
	cached, ok, err := store.Lookup("tmdb:later", "the movie", 2001)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || cached.Title != "The Movie" {
		t.Fatalf("cached entry = %+v, found = %v", cached, ok)
	}
}

func TestRemoveDeletesCachedSelectionAndTorrent(t *testing.T) {
	store := New(t.TempDir())
	entry, err := store.Save("tmdb:1", "The Movie", 2001, catalog.RankedCandidate{}, []byte("torrent"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Remove("tmdb:1", "The Movie", 2001); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(entry.TorrentPath); !os.IsNotExist(err) {
		t.Fatalf("cached torrent still exists: %v", err)
	}
	if _, ok, err := store.Lookup("tmdb:1", "The Movie", 2001); err != nil || ok {
		t.Fatalf("found = %v, error = %v", ok, err)
	}
}

func TestRemoveDeletesCachedUsenetSelectionAndNZB(t *testing.T) {
	store := New(t.TempDir())
	entry, err := store.SaveUsenet(
		"tmdb:1",
		"The Movie",
		2001,
		catalog.RankedCandidate{Candidate: catalog.Candidate{Protocol: catalog.ProtocolUsenet}},
		[]byte("nzb"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RemoveUsenet("tmdb:1", "The Movie", 2001); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(entry.NZBPath); !os.IsNotExist(err) {
		t.Fatalf("cached NZB still exists: %v", err)
	}
	if _, ok, err := store.LookupUsenet("tmdb:1", "The Movie", 2001); err != nil || ok {
		t.Fatalf("found = %v, error = %v", ok, err)
	}
}

func TestLookupIgnoresMissingTorrentFile(t *testing.T) {
	store := New(t.TempDir())
	entry, err := store.Save("tmdb:1", "The Movie", 2001, catalog.RankedCandidate{}, []byte("torrent"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(entry.TorrentPath); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.Lookup("tmdb:1", "The Movie", 2001); err != nil || ok {
		t.Fatalf("found = %v, error = %v", ok, err)
	}
}
