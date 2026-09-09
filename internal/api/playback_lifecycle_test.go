package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pgeske/filmstream/internal/catalog"
	"github.com/pgeske/filmstream/internal/hls"
	"github.com/pgeske/filmstream/internal/playbackcache"
	"github.com/pgeske/filmstream/internal/torrentstream"
)

func TestCanceledHLSRequestsPreserveSelectedRelease(t *testing.T) {
	for _, source := range []string{catalog.ProtocolTorrent, catalog.ProtocolUsenet} {
		for _, endpoint := range []string{"hls", "hls/subtitles"} {
			for _, interruption := range []string{"cancel", "deadline", "stop"} {
				t.Run(source+"/"+endpoint+"/"+interruption, func(t *testing.T) {
					store := playbackcache.New(t.TempDir())
					candidate := catalog.RankedCandidate{Candidate: catalog.Candidate{
						ID: "release", Name: "Fixture Season", Protocol: source,
					}}
					var err error
					if source == catalog.ProtocolUsenet {
						_, err = store.SaveUsenet("show-season", "Fixture", 2020, candidate, []byte("fixture metadata"))
					} else {
						_, err = store.Save("show-season", "Fixture", 2020, candidate, []byte("fixture metadata"))
					}
					if err != nil {
						t.Fatal(err)
					}
					ctx, cancel := context.WithCancel(t.Context())
					cause := context.Canceled
					if interruption == "deadline" {
						cancel()
						ctx, cancel = context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
						cause = context.DeadlineExceeded
					} else if interruption == "cancel" {
						cancel()
					}
					defer cancel()
					manager := &fakeHLSManager{
						startErrors: map[string]error{"playback": fmt.Errorf("waiter: %w", cause)},
						probeErrors: map[string]error{"playback": fmt.Errorf("waiter: %w", cause)},
					}
					server := &Server{
						engine: &fakeTorrentPlaybackEngine{sessions: map[string]*torrentstream.Session{
							"playback": {ID: "playback"},
						}},
						hlsManager: manager, playbackCache: store,
						playbackCacheKeys: map[string]playbackCacheKey{
							"playback": {mediaID: "show-season", title: "Fixture", year: 2020, source: source},
						},
						selected: map[string]catalog.RankedCandidate{"playback": candidate},
						playbackRequests: map[string]CreatePlaybackRequest{
							"playback": {MediaID: "episode-one"},
						},
						logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
					}
					method := http.MethodPost
					if endpoint == "hls/subtitles" {
						method = http.MethodGet
					}
					request := httptest.NewRequest(method, "/v1/playbacks/playback/"+endpoint, strings.NewReader(`{}`)).WithContext(ctx)
					response := httptest.NewRecorder()
					server.Handler().ServeHTTP(response, request)
					if interruption == "stop" && response.Code != http.StatusConflict {
						t.Fatalf("retired generation status = %d, want 409", response.Code)
					}
					var found bool
					if source == catalog.ProtocolUsenet {
						_, found, err = store.LookupUsenet("show-season", "Fixture", 2020)
					} else {
						_, found, err = store.Lookup("show-season", "Fixture", 2020)
					}
					if err != nil || !found {
						t.Fatalf("another consumer lost the shared release cache: found=%v error=%v", found, err)
					}
					if server.playbackInvalidated["playback"] || len(server.torrentFailures) != 0 || len(server.usenetFailures) != 0 {
						t.Fatal("canceled waiter marked a release as failed")
					}
				})
			}
		}
	}
}

type failedProducerManager struct{ fakeHLSManager }

func (*failedProducerManager) Status(string) (hls.Status, bool) {
	return hls.Status{State: "failed", PackagedSegments: 8, PackagedSeconds: 32, Error: "HLS producer stopped"}, true
}

func (*failedProducerManager) AssetPath(string, string) (string, error) {
	return "", hls.ErrProducerStopped
}

func TestPlaybackStatusDistinguishesProducerFailureFromActiveSourceReader(t *testing.T) {
	server := &Server{
		engine: &fakeTorrentPlaybackEngine{statuses: map[string]torrentstream.Status{
			"playback": {ID: "playback", State: "streaming", ActiveStreams: 1},
		}},
		hlsManager: &failedProducerManager{},
	}
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/playbacks/playback", nil))
	var status struct {
		State string     `json:"state"`
		HLS   hls.Status `json:"hls"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.State != "streaming" || status.HLS.State != "failed" || status.HLS.PackagedSeconds != 32 {
		t.Fatalf("status = %+v", status)
	}
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/playbacks/playback/hls/index.m3u8", nil))
	if response.Code != http.StatusBadGateway {
		t.Fatalf("failed live playlist status = %d, want 502", response.Code)
	}
}
