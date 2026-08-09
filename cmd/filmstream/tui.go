package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/pgeske/filmstream/internal/config"
	"github.com/pgeske/filmstream/internal/history"
)

const (
	colorBase     = "#303446"
	colorText     = "#c6d0f5"
	colorSubtext  = "#a5adce"
	colorSurface  = "#414559"
	colorOverlay  = "#737994"
	colorBlue     = "#8caaee"
	colorLavender = "#babbf1"
	colorGreen    = "#a6d189"
	colorYellow   = "#e5c890"
	colorRed      = "#e78284"
)

type tuiMode int

const (
	tuiBrowse tuiMode = iota
	tuiSearch
)

type tuiRow struct {
	section string
	entry   history.Entry
}

type playbackFinishedMsg struct {
	err error
}

type tuiModel struct {
	store      *history.Store
	configPath string
	entries    []history.Entry
	rows       []tuiRow
	cursor     int
	width      int
	height     int
	mode       tuiMode
	search     textinput.Model
	status     string
}

var (
	titleStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(colorLavender))
	sectionStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(colorBlue)).MarginTop(1)
	selectedStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(colorText)).Background(lipgloss.Color(colorSurface))
	normalStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color(colorText))
	detailStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color(colorSubtext))
	helpStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color(colorOverlay))
	statusStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color(colorGreen))
	errorStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color(colorRed))
)

func runTUI(args []string) error {
	flags := flag.NewFlagSet("tui", flag.ContinueOnError)
	configPath := flags.String("config", os.Getenv("FILMSTREAM_CONFIG"), "path to config file")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if len(flags.Args()) != 0 {
		return errors.New("usage: filmstream tui [--config PATH]")
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	store := history.New(cfg.StateDir)
	entries, err := store.List()
	if err != nil {
		return err
	}
	model := newTUIModel(store, entries, *configPath)
	_, err = tea.NewProgram(model, tea.WithAltScreen()).Run()
	return err
}

func newTUIModel(store *history.Store, entries []history.Entry, configPath string) tuiModel {
	input := textinput.New()
	input.Prompt = "Search › "
	input.Placeholder = "title, typo, or movie description"
	input.CharLimit = 300
	input.PromptStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(colorBlue))
	input.TextStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(colorText))
	input.PlaceholderStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(colorOverlay))
	model := tuiModel{store: store, entries: entries, configPath: configPath, search: input}
	model.rebuildRows()
	return model
}

func (m tuiModel) Init() tea.Cmd {
	return nil
}

func (m tuiModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch message := message.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = message.Width, message.Height
		m.search.Width = max(20, min(70, message.Width-14))
		return m, nil
	case playbackFinishedMsg:
		if message.err != nil {
			m.status = "Playback failed: " + message.err.Error()
		} else {
			m.status = "Playback ended. Progress saved."
		}
		m.reloadEntries()
		return m, nil
	case tea.KeyMsg:
		if message.Type == tea.KeyCtrlC {
			return m, tea.Quit
		}
		if m.mode == tuiSearch {
			switch message.String() {
			case "esc":
				m.mode = tuiBrowse
				m.search.Blur()
				m.search.Reset()
				m.status = ""
				return m, nil
			case "enter":
				query := strings.TrimSpace(m.search.Value())
				if query == "" {
					m.status = "Enter a movie title or description."
					return m, nil
				}
				m.mode = tuiBrowse
				m.search.Blur()
				m.search.Reset()
				m.status = "Opening " + query + "…"
				return m, m.playCommand(query, 0, false)
			}
			var command tea.Cmd
			m.search, command = m.search.Update(message)
			return m, command
		}

		switch message.String() {
		case "q":
			return m, tea.Quit
		case "/", "s":
			m.mode = tuiSearch
			m.status = ""
			return m, m.search.Focus()
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.cursor < len(m.rows)-1 {
				m.cursor++
			}
		case "enter":
			if row, ok := m.selectedRow(); ok {
				m.status = "Opening " + row.entry.Title + "…"
				return m, m.playCommand(row.entry.Title, row.entry.Year, true)
			}
		case "u":
			if row, ok := m.selectedRow(); ok {
				if err := m.store.MarkUnwatched(row.entry.ID); err != nil {
					m.status = "Could not update history: " + err.Error()
				} else {
					m.status = "Marked " + row.entry.Title + " unwatched."
					m.reloadEntries()
				}
			}
		case "d", "x":
			if row, ok := m.selectedRow(); ok {
				if err := m.store.Remove(row.entry.ID); err != nil {
					m.status = "Could not remove history: " + err.Error()
				} else {
					m.status = "Removed " + row.entry.Title + " from tracking."
					m.reloadEntries()
				}
			}
		}
	}
	return m, nil
}

func (m tuiModel) View() string {
	width := m.width
	if width <= 0 {
		width = 80
	}
	width = min(width, 110)
	var view strings.Builder
	view.WriteString(titleStyle.Render("FILMSTREAM"))
	view.WriteString("  ")
	view.WriteString(detailStyle.Render("stream lightly · resume anywhere"))
	view.WriteString("\n")
	view.WriteString(helpStyle.Render("Press / to search and play a movie"))
	view.WriteString("\n")

	if m.mode == tuiSearch {
		box := lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color(colorBlue)).
			Padding(0, 1).
			MarginTop(1).
			Width(max(24, min(width-4, 76)))
		view.WriteString(box.Render(m.search.View()))
		view.WriteString("\n")
		view.WriteString(helpStyle.Render("enter play  •  esc cancel"))
		view.WriteString("\n")
	} else if len(m.rows) == 0 {
		view.WriteString("\n")
		view.WriteString(sectionStyle.Render("Continue Watching"))
		view.WriteString("\n")
		view.WriteString(detailStyle.Render("Nothing here yet. Press / and find something good."))
		view.WriteString("\n")
	} else {
		start, end := m.visibleRange()
		lastSection := ""
		for index := start; index < end; index++ {
			row := m.rows[index]
			if row.section != lastSection {
				view.WriteString(sectionStyle.Render(row.section))
				view.WriteString("\n")
				lastSection = row.section
			}
			line := m.renderRow(row, index == m.cursor, width)
			view.WriteString(line)
			view.WriteString("\n")
		}
	}

	if m.status != "" {
		view.WriteString("\n")
		style := statusStyle
		if strings.Contains(strings.ToLower(m.status), "failed") || strings.Contains(strings.ToLower(m.status), "could not") {
			style = errorStyle
		}
		view.WriteString(style.Render(ansi.Truncate(m.status, max(20, width-2), "…")))
		view.WriteString("\n")
	}
	view.WriteString("\n")
	view.WriteString(helpStyle.Render("↑/↓ navigate  •  enter watch  •  / search  •  u unwatched  •  d remove  •  q quit"))
	return lipgloss.NewStyle().Background(lipgloss.Color(colorBase)).Padding(1, 2).Width(max(20, width-4)).Render(view.String())
}

func (m tuiModel) renderRow(row tuiRow, selected bool, width int) string {
	marker := "  "
	if selected {
		marker = "› "
	}
	title := row.entry.Title
	if row.entry.Year > 0 {
		title += " (" + strconv.Itoa(row.entry.Year) + ")"
	}
	detail := historyDetail(row.entry)
	available := max(18, width-lipgloss.Width(detail)-9)
	title = ansi.Truncate(title, available, "…")
	line := marker + title + "  " + detail
	if selected {
		return selectedStyle.Padding(0, 1).Width(max(20, width-6)).Render(line)
	}
	return normalStyle.Padding(0, 1).Render(line)
}

func historyDetail(entry history.Entry) string {
	switch {
	case entry.Completed:
		return lipgloss.NewStyle().Foreground(lipgloss.Color(colorGreen)).Render("watched")
	case entry.CanContinue():
		progress := entry.Progress()
		filled := int(progress*10 + 0.5)
		bar := strings.Repeat("━", filled) + strings.Repeat("─", 10-filled)
		return lipgloss.NewStyle().Foreground(lipgloss.Color(colorYellow)).Render(
			fmt.Sprintf("%s %3.0f%% · %s", bar, progress*100, formatDuration(entry.PositionSeconds)),
		)
	case entry.PositionSeconds > 0:
		return detailStyle.Render("started " + formatDuration(entry.PositionSeconds))
	default:
		return detailStyle.Render("unwatched")
	}
}

func (m tuiModel) playCommand(query string, year int, exact bool) tea.Cmd {
	executable, err := os.Executable()
	if err != nil {
		return func() tea.Msg { return playbackFinishedMsg{err: err} }
	}
	arguments := []string{"play"}
	if m.configPath != "" {
		arguments = append(arguments, "--config", m.configPath)
	}
	if exact {
		arguments = append(arguments, "--no-ai")
	}
	if year > 0 {
		arguments = append(arguments, "--year", strconv.Itoa(year))
	}
	arguments = append(arguments, query)
	command := exec.Command(executable, arguments...)
	return tea.ExecProcess(command, func(err error) tea.Msg {
		return playbackFinishedMsg{err: err}
	})
}

func (m *tuiModel) reloadEntries() {
	entries, err := m.store.List()
	if err != nil {
		m.status = "Could not load history: " + err.Error()
		return
	}
	m.entries = entries
	m.rebuildRows()
	if m.cursor >= len(m.rows) {
		m.cursor = max(0, len(m.rows)-1)
	}
}

func (m *tuiModel) rebuildRows() {
	m.rows = m.rows[:0]
	for _, entry := range m.entries {
		if entry.CanContinue() {
			m.rows = append(m.rows, tuiRow{section: "Continue Watching", entry: entry})
		}
	}
	for _, entry := range m.entries {
		if !entry.CanContinue() {
			m.rows = append(m.rows, tuiRow{section: "Watch History", entry: entry})
		}
	}
}

func (m tuiModel) selectedRow() (tuiRow, bool) {
	if m.cursor < 0 || m.cursor >= len(m.rows) {
		return tuiRow{}, false
	}
	return m.rows[m.cursor], true
}

func (m tuiModel) visibleRange() (int, int) {
	maximum := max(3, m.height-11)
	if maximum >= len(m.rows) {
		return 0, len(m.rows)
	}
	start := m.cursor - maximum/2
	start = max(0, min(start, len(m.rows)-maximum))
	return start, start + maximum
}
