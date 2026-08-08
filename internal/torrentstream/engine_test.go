package torrentstream

import (
	"bytes"
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
)

func TestEngineServesRangesFromTorrentStorage(t *testing.T) {
	dataDir := t.TempDir()
	torrentDataDir := filepath.Join(dataDir, "torrents")
	if err := os.MkdirAll(torrentDataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	contents := bytes.Repeat([]byte("filmstream-range-test-"), 2048)
	videoPath := filepath.Join(torrentDataDir, "sample.mp4")
	if err := os.WriteFile(videoPath, contents, 0o644); err != nil {
		t.Fatal(err)
	}

	meta := metainfo.MetaInfo{}
	meta.SetDefaults()
	info := metainfo.Info{PieceLength: 16 << 10}
	if err := info.BuildFromFilePath(videoPath); err != nil {
		t.Fatal(err)
	}
	var err error
	meta.InfoBytes, err = bencode.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	torrentPath := filepath.Join(dataDir, "sample.torrent")
	file, err := os.Create(torrentPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := meta.Write(file); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	engine, err := New(Config{
		DataDir:         dataDir,
		MaxTorrentBytes: 1 << 30,
		ReadaheadBytes:  1 << 20,
		MetadataTimeout: 5 * time.Second,
		SeedRatioTarget: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()

	session, err := engine.Create(context.Background(), Source{TorrentPath: torrentPath})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	request := httptest.NewRequest("GET", "/stream", nil).WithContext(ctx)
	request.Header.Set("Range", "bytes=10-99")
	response := httptest.NewRecorder()
	if err := engine.ServeHTTP(response, request, session.ID); err != nil {
		t.Fatal(err)
	}

	if response.Code != 206 {
		t.Fatalf("status = %d, body = %q", response.Code, response.Body.String())
	}
	if got, want := response.Body.Bytes(), contents[10:100]; !bytes.Equal(got, want) {
		t.Fatalf("range body mismatch: got %d bytes, want %d", len(got), len(want))
	}
}
