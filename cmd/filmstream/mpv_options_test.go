package main

import (
	"reflect"
	"testing"
)

func TestMPVStreamingOptionsPrimeCacheBeforePlayback(t *testing.T) {
	got := mpvStreamingOptionsFor(mpvPlatform{})
	want := []string{
		"--cache=yes",
		"--cache-secs=30",
		"--cache-pause-initial=yes",
		"--cache-pause-wait=2",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("options = %q, want %q", got, want)
	}
}

func TestMPVStreamingOptionsUseWindowsHardwareDecoding(t *testing.T) {
	got := mpvStreamingOptionsFor(mpvPlatform{UseWindowsD3D11: true})
	want := []string{
		"--cache=yes",
		"--cache-secs=30",
		"--cache-pause-initial=yes",
		"--cache-pause-wait=2",
		"--hwdec=auto-safe",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("options = %q, want %q", got, want)
	}
}

func TestMPVStreamingOptionsUseLegacyWSLNVDECPath(t *testing.T) {
	got := mpvStreamingOptionsFor(mpvPlatform{UseWSLNVDEC: true})
	want := []string{
		"--cache=yes",
		"--cache-secs=30",
		"--cache-pause-initial=yes",
		"--cache-pause-wait=2",
		"--vo=wlshm",
		"--hwdec=nvdec-copy",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("options = %q, want %q", got, want)
	}
}

func TestMPVStreamingOptionsUseModernWSLGPUPath(t *testing.T) {
	got := mpvStreamingOptionsFor(mpvPlatform{UseWSLNVDEC: true, UseGPUNext: true})
	want := []string{
		"--cache=yes",
		"--cache-secs=30",
		"--cache-pause-initial=yes",
		"--cache-pause-wait=2",
		"--vo=gpu-next",
		"--gpu-context=x11egl",
		"--hwdec=nvdec-copy",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("options = %q, want %q", got, want)
	}
}
