package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pgeske/filmstream/internal/history"
)

type mpvProgressTracker struct {
	options []string
	read    func() (float64, float64, error)
	close   func()
}

func runTrackedMPV(path, streamURL string, entry history.Entry, store *history.Store) error {
	tracker, err := newMPVProgressTracker(path)
	if err != nil {
		return err
	}
	defer tracker.close()

	title := entry.Title
	if entry.Year > 0 {
		title += fmt.Sprintf(" (%d)", entry.Year)
	}
	position := entry.ResumePosition()
	args := trackedMPVOptions(path, tracker.options, title, position)
	if position > 0 {
		fmt.Fprintf(os.Stderr, "Resuming at %s.\n", formatDuration(position))
	}
	args = append(args, streamURL)
	command := exec.Command(path, args...)
	command.Stdin = os.Stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		return err
	}

	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	var lastPosition, lastDuration float64
	var saveErr error
	for {
		select {
		case err := <-done:
			if lastPosition > 0 {
				if updateErr := store.UpdateProgress(entry.ID, lastPosition, lastDuration); updateErr != nil {
					saveErr = updateErr
				}
			}
			if saveErr != nil {
				fmt.Fprintln(os.Stderr, "Warning: could not save watch progress:", saveErr)
			}
			return err
		case <-ticker.C:
			position, duration, err := tracker.read()
			if err != nil {
				continue
			}
			lastPosition, lastDuration = position, duration
			if err := store.UpdateProgress(entry.ID, position, duration); err != nil {
				saveErr = err
			}
		}
	}
}

func trackedMPVOptions(path string, progressOptions []string, title string, position float64) []string {
	options := append([]string(nil), mpvStreamingOptions(path)...)
	options = append(options, "--resume-playback=no", "--force-media-title="+title)
	options = append(options, progressOptions...)
	if position > 0 {
		options = append(options, "--start="+strconv.FormatFloat(position, 'f', 1, 64))
	}
	return options
}

func newMPVProgressTracker(path string) (*mpvProgressTracker, error) {
	if isWindowsMPV(path) {
		return newWindowsMPVProgressTracker()
	}

	temporary, err := os.CreateTemp("", "filmstream-mpv-*.sock")
	if err != nil {
		return nil, err
	}
	socketPath := temporary.Name()
	if err := temporary.Close(); err != nil {
		return nil, err
	}
	_ = os.Remove(socketPath)
	return &mpvProgressTracker{
		options: []string{"--input-ipc-server=" + socketPath},
		read: func() (float64, float64, error) {
			return readMPVProgress(socketPath)
		},
		close: func() { _ = os.Remove(socketPath) },
	}, nil
}

func newWindowsMPVProgressTracker() (*mpvProgressTracker, error) {
	windowsTemp, err := exec.Command("cmd.exe", "/d", "/c", "echo", "%TEMP%").Output()
	if err != nil {
		return nil, fmt.Errorf("find Windows temporary directory: %w", err)
	}
	tempDir, err := exec.Command("wslpath", "-u", strings.TrimSpace(string(windowsTemp))).Output()
	if err != nil {
		return nil, fmt.Errorf("translate Windows temporary directory: %w", err)
	}

	script, err := os.CreateTemp(strings.TrimSpace(string(tempDir)), "filmstream-mpv-*.lua")
	if err != nil {
		return nil, err
	}
	scriptPath := script.Name()
	progressPath := strings.TrimSuffix(scriptPath, ".lua") + ".json"
	windowsProgressPath, err := windowsPath(progressPath)
	if err != nil {
		_ = script.Close()
		_ = os.Remove(scriptPath)
		return nil, err
	}
	contents := `local utils = require 'mp.utils'
local progress_path = ` + strconv.Quote(windowsProgressPath) + `

local function save_progress()
    local position = mp.get_property_number('time-pos')
    local duration = mp.get_property_number('duration')
    if not position or not duration or duration <= 0 then return end
    local temporary_path = progress_path .. '.tmp'
    local file = io.open(temporary_path, 'w')
    if not file then return end
    file:write(utils.format_json({position = position, duration = duration}))
    file:close()
    os.remove(progress_path)
    os.rename(temporary_path, progress_path)
end

mp.add_periodic_timer(3, save_progress)
mp.register_event('shutdown', save_progress)
`
	if _, err := script.WriteString(contents); err != nil {
		_ = script.Close()
		_ = os.Remove(scriptPath)
		return nil, err
	}
	if err := script.Close(); err != nil {
		_ = os.Remove(scriptPath)
		return nil, err
	}
	windowsScriptPath, err := windowsPath(scriptPath)
	if err != nil {
		_ = os.Remove(scriptPath)
		return nil, err
	}

	return &mpvProgressTracker{
		options: []string{"--script=" + windowsScriptPath},
		read: func() (float64, float64, error) {
			return readWindowsMPVProgress(progressPath)
		},
		close: func() {
			_ = os.Remove(scriptPath)
			_ = os.Remove(progressPath)
			_ = os.Remove(progressPath + ".tmp")
		},
	}, nil
}

func windowsPath(path string) (string, error) {
	output, err := exec.Command("wslpath", "-w", path).Output()
	if err != nil {
		return "", fmt.Errorf("translate path for Windows MPV: %w", err)
	}
	return strings.TrimSpace(string(output)), nil
}

func readWindowsMPVProgress(path string) (float64, float64, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, err
	}
	var progress struct {
		Position float64 `json:"position"`
		Duration float64 `json:"duration"`
	}
	if err := json.Unmarshal(contents, &progress); err != nil {
		return 0, 0, err
	}
	if progress.Duration <= 0 {
		return 0, 0, errors.New("mpv returned incomplete progress data")
	}
	return progress.Position, progress.Duration, nil
}

func isMPV(path string) bool {
	name := filepath.Base(path)
	extension := filepath.Ext(name)
	if strings.EqualFold(extension, ".exe") || strings.EqualFold(extension, ".com") {
		name = strings.TrimSuffix(name, extension)
	}
	return strings.EqualFold(name, "mpv")
}

func isWindowsMPV(path string) bool {
	extension := filepath.Ext(filepath.Base(path))
	return isMPV(path) && (strings.EqualFold(extension, ".exe") || strings.EqualFold(extension, ".com"))
}

func readMPVProgress(socketPath string) (float64, float64, error) {
	connection, err := net.DialTimeout("unix", socketPath, 300*time.Millisecond)
	if err != nil {
		return 0, 0, err
	}
	defer connection.Close()
	if err := connection.SetDeadline(time.Now().Add(time.Second)); err != nil {
		return 0, 0, err
	}
	requests := []struct {
		Name string
		ID   int
	}{{"time-pos", 1}, {"duration", 2}}
	for _, request := range requests {
		payload := map[string]any{
			"command":    []any{"get_property", request.Name},
			"request_id": request.ID,
		}
		if err := json.NewEncoder(connection).Encode(payload); err != nil {
			return 0, 0, err
		}
	}

	values := make(map[int]float64)
	scanner := bufio.NewScanner(connection)
	for scanner.Scan() {
		var response struct {
			RequestID int             `json:"request_id"`
			Error     string          `json:"error"`
			Data      json.RawMessage `json:"data"`
		}
		if json.Unmarshal(scanner.Bytes(), &response) != nil || response.RequestID == 0 {
			continue
		}
		if response.Error != "success" {
			return 0, 0, errors.New(response.Error)
		}
		var value float64
		if err := json.Unmarshal(response.Data, &value); err != nil {
			return 0, 0, err
		}
		values[response.RequestID] = value
		if len(values) == 2 {
			return values[1], values[2], nil
		}
	}
	if err := scanner.Err(); err != nil {
		return 0, 0, err
	}
	return 0, 0, errors.New("mpv returned incomplete progress data")
}

func formatDuration(seconds float64) string {
	duration := time.Duration(seconds * float64(time.Second)).Round(time.Second)
	if duration >= time.Hour {
		return fmt.Sprintf("%d:%02d:%02d", int(duration.Hours()), int(duration.Minutes())%60, int(duration.Seconds())%60)
	}
	return fmt.Sprintf("%d:%02d", int(duration.Minutes()), int(duration.Seconds())%60)
}
