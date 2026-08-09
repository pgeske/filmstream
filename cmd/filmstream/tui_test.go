package main

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

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
	if line := model.renderRow(model.rows[0], true, model.rowTitleWidths(100)[model.rows[0].section]); strings.Contains(line, "\n") {
		t.Fatalf("selected row wrapped unexpectedly: %q", line)
	}
	for _, expected := range []string{"FILMSTREAM", "Continue Watching", "Sintel", "Watch History", "Cosmos Laundromat"} {
		if !strings.Contains(view, expected) {
			t.Fatalf("view does not contain %q", expected)
		}
	}
}

func TestTUIAlignsProgressBarsWithinSection(t *testing.T) {
	entries := []history.Entry{
		{Title: "Lawrence of Arabia", Year: 1962, PositionSeconds: 4200, DurationSeconds: 13600},
		{Title: "Raiders of the Lost Ark", Year: 1981, PositionSeconds: 2900, DurationSeconds: 6900},
	}
	model := newTUIModel(history.New(t.TempDir()), entries, "")
	model.width, model.height = 100, 30
	lines := strings.Split(ansi.Strip(model.View()), "\n")
	positions := make([]int, 0, 2)
	matchingLines := make([]string, 0, 2)
	for _, line := range lines {
		if strings.Contains(line, "Lawrence of Arabia") || strings.Contains(line, "Raiders of the Lost Ark") {
			bar := strings.Index(line, "━")
			if bar < 0 {
				positions = append(positions, -1)
			} else {
				positions = append(positions, ansi.StringWidth(line[:bar]))
			}
			matchingLines = append(matchingLines, line)
		}
	}
	if len(positions) != 2 || positions[0] < 0 || positions[0] != positions[1] {
		t.Fatalf("progress bar positions = %v in %q", positions, matchingLines)
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
