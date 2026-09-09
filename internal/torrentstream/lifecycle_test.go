package torrentstream

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFailedPlaybackDoesNotPoisonAnotherConsumerOfSameTorrent(t *testing.T) {
	dataDir := t.TempDir()
	torrentPath, _, contents := createTestTorrent(t, dataDir)
	engine := newTestEngine(t, dataDir, Config{})
	defer engine.Close()
	failed, err := engine.Create(t.Context(), Source{TorrentPath: torrentPath})
	if err != nil {
		t.Fatal(err)
	}
	replay, err := engine.Create(t.Context(), Source{TorrentPath: torrentPath})
	if err != nil {
		t.Fatal(err)
	}
	if failed.ID == replay.ID || failed.torrent != replay.torrent {
		t.Fatal("fixture must have independent playback IDs sharing one torrent")
	}
	if err := engine.MarkSourceUnavailable(failed.ID, errors.New("producer stopped advancing")); !errors.Is(err, ErrSourceUnavailable) {
		t.Fatalf("source failure = %v", err)
	}
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/stream", nil).WithContext(t.Context())
	if err := engine.ServeHTTP(response, request, replay.ID); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(response.Body.Bytes(), contents) {
		t.Fatal("another playback lost access to the shared torrent's cached pieces")
	}
	if status, ok := engine.Status(replay.ID); !ok || status.SourceUnavailable {
		t.Fatalf("other consumer status = %+v, exists=%v", status, ok)
	}
	if _, ok := engine.Get(failed.ID); !ok {
		t.Fatal("failure detection removed the retained torrent instead of preserving its seeding lifecycle")
	}
}
