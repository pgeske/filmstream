package main

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/pgeske/filmstream/internal/history"
)

func TestTUIBuildsContinueAndHistorySections(t *testing.T) {
	store := history.New(t.TempDir())
	continuing, err := store.Upsert("Sintel", 2010)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateProgress(continuing.ID, 300, 1000); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Upsert("Cosmos Laundromat", 2015); err != nil {
		t.Fatal(err)
	}
	entries, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	model := newTUIModel(store, entries, "")
	model.width, model.height = 100, 30
	if len(model.rows) != 2 || model.rows[0].section != "Continue Watching" || model.rows[1].section != "Watch History" {
		t.Fatalf("rows = %+v", model.rows)
	}
	view := model.View()
	if strings.Contains(view, "\x1b[48;") {
		t.Fatal("view contains a background color that can bleed into adjacent lines")
	}
	if line := model.renderRow(model.rows[0], true, 100); strings.Contains(line, "\n") {
		t.Fatalf("selected row wrapped unexpectedly: %q", line)
	}
	for _, expected := range []string{"FILMSTREAM", "Continue Watching", "Sintel", "Watch History", "Cosmos Laundromat"} {
		if !strings.Contains(view, expected) {
			t.Fatalf("view does not contain %q", expected)
		}
	}
}

func TestTUIMarksSelectedMovieUnwatchedAndOpensSearch(t *testing.T) {
	store := history.New(t.TempDir())
	entry, err := store.Upsert("Sintel", 2010)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateProgress(entry.ID, 300, 1000); err != nil {
		t.Fatal(err)
	}
	entries, _ := store.List()
	model := newTUIModel(store, entries, "")
	updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'u'}})
	model = updated.(tuiModel)
	if len(model.rows) != 1 || model.rows[0].entry.PositionSeconds != 0 || model.rows[0].section != "Watch History" {
		t.Fatalf("rows after marking unwatched = %+v", model.rows)
	}
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	model = updated.(tuiModel)
	if model.mode != tuiSearch || !model.search.Focused() {
		t.Fatal("search did not receive focus")
	}
}
