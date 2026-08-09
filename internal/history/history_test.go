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
