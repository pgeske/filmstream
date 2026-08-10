package history

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStoreTracksResumeCompletionAndRemoval(t *testing.T) {
	stateDir := t.TempDir()
	store := New(stateDir)
	entry, err := store.Upsert("Sintel", 2010)
	if err != nil {
		t.Fatal(err)
	}
	if entry.ResumePosition() != 0 {
		t.Fatalf("new entry resumes at %f", entry.ResumePosition())
	}
	if err := store.UpdateProgress(entry.ID, 300, 1000); err != nil {
		t.Fatal(err)
	}
	entries, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].ResumePosition() != 300 || !entries[0].CanContinue() {
		t.Fatalf("entries = %+v", entries)
	}
	if err := store.UpdateProgress(entry.ID, 950, 1000); err != nil {
		t.Fatal(err)
	}
	entries, _ = store.List()
	if !entries[0].Completed || entries[0].ResumePosition() != 0 {
		t.Fatalf("completed entry = %+v", entries[0])
	}
	if err := store.MarkUnwatched(entry.ID); err != nil {
		t.Fatal(err)
	}
	entries, _ = store.List()
	if entries[0].Completed || entries[0].PositionSeconds != 0 {
		t.Fatalf("unwatched entry = %+v", entries[0])
	}
	if err := store.Remove(entry.ID); err != nil {
		t.Fatal(err)
	}
	entries, _ = store.List()
	if len(entries) != 0 {
		t.Fatalf("entries after removal = %+v", entries)
	}
}

func TestStoreRecordsSharedProgressMetadata(t *testing.T) {
	store := New(t.TempDir())
	entry, err := store.RecordProgress(Entry{
		MediaID:         "tmdb:335984",
		Title:           "Blade Runner 2049",
		Year:            2017,
		Overview:        "A young blade runner uncovers a secret.",
		PosterURL:       "https://image.example/poster.jpg",
		BackdropURL:     "https://image.example/backdrop.jpg",
		PositionSeconds: 600,
		DurationSeconds: 1800,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !entry.CanContinue() || entry.MediaID != "tmdb:335984" {
		t.Fatalf("entry = %+v", entry)
	}
	enriched, err := store.UpdateMetadata(entry.ID, Entry{BackdropURL: "https://image.example/new-backdrop.jpg"})
	if err != nil {
		t.Fatal(err)
	}
	if enriched.UpdatedAt != entry.UpdatedAt || enriched.BackdropURL != "https://image.example/new-backdrop.jpg" {
		t.Fatalf("enriched entry = %+v", enriched)
	}
	updated, err := store.RecordProgress(Entry{
		MediaID:         entry.MediaID,
		Title:           entry.Title,
		Year:            entry.Year,
		PositionSeconds: 700,
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.ID != entry.ID || updated.DurationSeconds != 1800 {
		t.Fatalf("updated entry = %+v", updated)
	}
}

func TestStoreUsesPrivatePermissions(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	store := New(stateDir)
	if _, err := store.Upsert("Cosmos Laundromat", 2015); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(stateDir, "history.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("history permissions = %o", info.Mode().Perm())
	}
}
