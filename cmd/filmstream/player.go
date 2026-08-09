package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"time"

	"github.com/pgeske/filmstream/internal/history"
)

func runTrackedMPV(path, streamURL string, entry history.Entry, store *history.Store) error {
	temporary, err := os.CreateTemp("", "filmstream-mpv-*.sock")
	if err != nil {
		return err
	}
	socketPath := temporary.Name()
	if err := temporary.Close(); err != nil {
		return err
	}
	_ = os.Remove(socketPath)
	defer os.Remove(socketPath)

	title := entry.Title
	if entry.Year > 0 {
		title += fmt.Sprintf(" (%d)", entry.Year)
	}
	position := entry.ResumePosition()
	args := trackedMPVOptions(socketPath, title, position)
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
			position, duration, err := readMPVProgress(socketPath)
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

func trackedMPVOptions(socketPath, title string, position float64) []string {
	options := append([]string(nil), mpvStreamingOptions()...)
	options = append(options,
		"--resume-playback=no",
		"--force-media-title="+title,
		"--input-ipc-server="+socketPath,
	)
	if position > 0 {
		options = append(options, "--start="+strconv.FormatFloat(position, 'f', 1, 64))
	}
	return options
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
