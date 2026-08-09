package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func mpvStreamingOptions(path string) []string {
	return mpvStreamingOptionsFor(detectMPVPlatform(path))
}

type mpvPlatform struct {
	UseWindowsD3D11 bool
	UseWSLNVDEC     bool
	UseGPUNext      bool
}

func mpvStreamingOptionsFor(platform mpvPlatform) []string {
	options := []string{
		"--cache=yes",
		"--cache-secs=30",
		"--cache-pause-initial=yes",
		"--cache-pause-wait=2",
	}
	if platform.UseWindowsD3D11 {
		options = append(options, "--hwdec=auto-safe")
	} else if platform.UseWSLNVDEC {
		if platform.UseGPUNext {
			options = append(options, "--vo=gpu-next", "--gpu-context=x11egl", "--hwdec=nvdec-copy")
		} else {
			options = append(options, "--vo=wlshm", "--hwdec=nvdec-copy")
		}
	}
	return options
}

func detectMPVPlatform(path string) mpvPlatform {
	if isWindowsMPV(path) {
		return mpvPlatform{UseWindowsD3D11: true}
	}

	contents, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil || !strings.Contains(strings.ToLower(string(contents)), "microsoft") {
		return mpvPlatform{}
	}
	matches, _ := filepath.Glob("/usr/lib/wsl/lib/libnvcuvid.so*")
	useNVDEC := len(matches) > 0 && (os.Getenv("WAYLAND_DISPLAY") != "" || os.Getenv("DISPLAY") != "")
	if !useNVDEC {
		return mpvPlatform{}
	}

	output, err := exec.Command(path, "--vo=help").CombinedOutput()
	useGPUNext := err == nil && os.Getenv("DISPLAY") != "" && strings.Contains(string(output), "gpu-next")
	return mpvPlatform{UseWSLNVDEC: true, UseGPUNext: useGPUNext}
}
