package main

import (
	"os"
	"path/filepath"
	"strings"
)

func mpvStreamingOptions() []string {
	return mpvStreamingOptionsFor(detectMPVPlatform())
}

type mpvPlatform struct {
	UseWSLNVDEC bool
}

func mpvStreamingOptionsFor(platform mpvPlatform) []string {
	options := []string{
		"--cache=yes",
		"--cache-secs=30",
		"--cache-pause-initial=yes",
		"--cache-pause-wait=2",
	}
	if platform.UseWSLNVDEC {
		options = append(options, "--profile=sw-fast", "--vo=wlshm", "--hwdec=nvdec-copy")
	}
	return options
}

func detectMPVPlatform() mpvPlatform {
	contents, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil || !strings.Contains(strings.ToLower(string(contents)), "microsoft") {
		return mpvPlatform{}
	}
	if os.Getenv("WAYLAND_DISPLAY") == "" {
		return mpvPlatform{}
	}
	matches, _ := filepath.Glob("/usr/lib/wsl/lib/libnvcuvid.so*")
	return mpvPlatform{UseWSLNVDEC: len(matches) > 0}
}
