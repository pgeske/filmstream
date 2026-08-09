package main

import (
	"bufio"
	"encoding/json"
	"net"
	"path/filepath"
	"slices"
	"testing"
)

func TestTrackedMPVOptionsPreserveStreamingStartupOptions(t *testing.T) {
	streaming := mpvStreamingOptions("mpv")
	options := trackedMPVOptions("mpv", "/tmp/mpv.sock", "Sintel (2010)", 65.5)
	if !slices.Equal(options[:len(streaming)], streaming) {
		t.Fatalf("streaming prefix = %q, want %q", options[:len(streaming)], streaming)
	}
	if options[len(options)-1] != "--start=65.5" {
		t.Fatalf("resume option = %q", options[len(options)-1])
	}
}

func TestReadMPVProgress(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mpv.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		defer connection.Close()
		scanner := bufio.NewScanner(connection)
		encoder := json.NewEncoder(connection)
		for scanner.Scan() {
			var request struct {
				RequestID int `json:"request_id"`
			}
			if json.Unmarshal(scanner.Bytes(), &request) != nil {
				continue
			}
			value := 7200.0
			if request.RequestID == 1 {
				value = 321.5
			}
			_ = encoder.Encode(map[string]any{"request_id": request.RequestID, "error": "success", "data": value})
		}
	}()

	position, duration, err := readMPVProgress(path)
	if err != nil {
		t.Fatal(err)
	}
	if position != 321.5 || duration != 7200 {
		t.Fatalf("progress = %f / %f", position, duration)
	}
}

func TestFormatDuration(t *testing.T) {
	if got := formatDuration(3723); got != "1:02:03" {
		t.Fatalf("duration = %q", got)
	}
}
