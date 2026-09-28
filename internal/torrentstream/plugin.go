package torrentstream

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// pluginCallTimeout bounds every plugin request except long polls. The plugin
// runs on loopback inside Deluge's reactor, so a slower answer means Deluge is
// wedged rather than busy.
const pluginCallTimeout = 15 * time.Second

// errTorrentNotFound reports a torrent Deluge does not have.
var errTorrentNotFound = errors.New("torrent not found in Deluge")

// errNoMetadata reports a magnet whose metadata has not arrived yet.
var errNoMetadata = errors.New("torrent metadata is not available yet")

// pluginClient speaks the TeaStream Deluge plugin's loopback JSON API; see
// deluge/teastream/deluge_teastream/core.py for the endpoint reference.
type pluginClient struct {
	baseURL   string
	tokenFile string
	http      *http.Client

	tokenMu sync.Mutex
	token   string
}

type pluginAddRequest struct {
	Torrent     string `json:"torrent,omitempty"`
	Magnet      string `json:"magnet,omitempty"`
	SaveRoot    string `json:"save_root,omitempty"`
	WantedFiles any    `json:"wanted_files"`
}

type pluginAddResponse struct {
	InfoHash string `json:"info_hash"`
	Added    bool   `json:"added"`
	SavePath string `json:"save_path"`
}

type pluginFile struct {
	Index    int    `json:"index"`
	Path     string `json:"path"`
	Size     int64  `json:"size"`
	Offset   int64  `json:"offset"`
	Pad      bool   `json:"pad"`
	Priority int    `json:"priority"`
	Done     int64  `json:"done"`
}

// pluginTorrent is the plugin's torrent detail. Transfer totals come from
// libtorrent's all-time counters, which Deluge persists across restarts.
type pluginTorrent struct {
	InfoHash          string       `json:"info_hash"`
	Name              string       `json:"name"`
	State             string       `json:"state"`
	HasMetadata       bool         `json:"has_metadata"`
	Private           bool         `json:"private"`
	TotalSize         int64        `json:"total_size"`
	TotalDone         int64        `json:"total_done"`
	TotalWanted       int64        `json:"total_wanted"`
	TotalWantedDone   int64        `json:"total_wanted_done"`
	Progress          float64      `json:"progress"`
	DownloadRate      int64        `json:"download_rate"`
	UploadRate        int64        `json:"upload_rate"`
	NumPeers          int          `json:"num_peers"`
	NumSeeds          int          `json:"num_seeds"`
	ListPeers         int          `json:"list_peers"`
	ListSeeds         int          `json:"list_seeds"`
	ConnectCandidates int          `json:"connect_candidates"`
	AllTimeDownload   int64        `json:"all_time_download"`
	AllTimeUpload     int64        `json:"all_time_upload"`
	SeedingSeconds    int64        `json:"seeding_seconds"`
	FinishedSeconds   int64        `json:"finished_seconds"`
	ActiveSeconds     int64        `json:"active_seconds"`
	IsFinished        bool         `json:"is_finished"`
	IsSeeding         bool         `json:"is_seeding"`
	Paused            bool         `json:"paused"`
	SavePath          string       `json:"save_path"`
	TrackerStatus     string       `json:"tracker_status"`
	TrackerMessage    string       `json:"tracker_message"`
	Error             string       `json:"error"`
	FileProgress      []int64      `json:"file_progress"`
	PieceLength       int64        `json:"piece_length"`
	NumPieces         int          `json:"num_pieces"`
	Trackers          []string     `json:"trackers"`
	Files             []pluginFile `json:"files"`
}

type pluginPieces struct {
	First    int    `json:"first"`
	Last     int    `json:"last"`
	Bitfield []byte `json:"bitfield"`
	Complete bool   `json:"complete"`
}

// has reports whether piece is present according to the bitfield.
func (p pluginPieces) has(piece int) bool {
	if piece < p.First || piece > p.Last {
		return false
	}
	bit := piece - p.First
	return bit>>3 < len(p.Bitfield) && p.Bitfield[bit>>3]&(0x80>>(bit&7)) != 0
}

type pluginWindow struct {
	File          int   `json:"file"`
	Offset        int64 `json:"offset"`
	Length        int64 `json:"length"`
	DeadlineBytes int64 `json:"deadline_bytes,omitempty"`
	TTLMillis     int64 `json:"ttl_ms,omitempty"`
}

type pluginHealth struct {
	OK         bool   `json:"ok"`
	Deluge     string `json:"deluge"`
	Libtorrent string `json:"libtorrent"`
	ListenPort int    `json:"listen_port"`
	DHT        bool   `json:"dht"`
	LSD        bool   `json:"lsd"`
	UPnP       bool   `json:"upnp"`
	NATPMP     bool   `json:"natpmp"`
	UTPEX      bool   `json:"utpex"`
	PEXLoaded  bool   `json:"pex_extension_loaded"`
	Token      bool   `json:"token_configured"`
}

type pluginError struct {
	status  int
	message string
}

func (e *pluginError) Error() string {
	return fmt.Sprintf("deluge plugin: %s (HTTP %d)", e.message, e.status)
}

func newPluginClient(baseURL, tokenFile string) *pluginClient {
	return &pluginClient{
		baseURL:   strings.TrimRight(baseURL, "/"),
		tokenFile: tokenFile,
		// Per-call contexts bound requests; the transport only needs to keep
		// loopback connections alive for the frequent window and wait calls.
		http: &http.Client{Transport: &http.Transport{
			MaxIdleConnsPerHost: 16,
			IdleConnTimeout:     90 * time.Second,
		}},
	}
}

func (c *pluginClient) health(ctx context.Context) (pluginHealth, error) {
	var health pluginHealth
	err := c.call(ctx, http.MethodGet, "/v1/health", nil, &health)
	return health, err
}

func (c *pluginClient) add(ctx context.Context, request pluginAddRequest) (pluginAddResponse, error) {
	var response pluginAddResponse
	err := c.call(ctx, http.MethodPost, "/v1/torrents", request, &response)
	return response, err
}

func (c *pluginClient) torrent(ctx context.Context, infoHash string) (pluginTorrent, error) {
	var detail pluginTorrent
	err := c.call(ctx, http.MethodGet, "/v1/torrents/"+infoHash, nil, &detail)
	return detail, err
}

func (c *pluginClient) list(ctx context.Context) ([]pluginTorrent, error) {
	var response struct {
		Torrents []pluginTorrent `json:"torrents"`
	}
	err := c.call(ctx, http.MethodGet, "/v1/torrents", nil, &response)
	return response.Torrents, err
}

func (c *pluginClient) remove(ctx context.Context, infoHash string, removeData bool) error {
	path := "/v1/torrents/" + infoHash
	if removeData {
		path += "?remove_data=1"
	}
	return c.call(ctx, http.MethodDelete, path, nil, nil)
}

// wait long-polls until piece is present or timeout elapses and returns the
// bitfield of piece..through.
func (c *pluginClient) wait(ctx context.Context, infoHash string, piece, through int, timeout time.Duration) (pluginPieces, error) {
	var response pluginPieces
	request := map[string]int64{"piece": int64(piece), "through": int64(through), "timeout_ms": timeout.Milliseconds()}
	ctx, cancel := context.WithTimeout(ctx, timeout+pluginCallTimeout)
	defer cancel()
	err := c.do(ctx, http.MethodPost, "/v1/torrents/"+infoHash+"/wait", request, &response)
	return response, err
}

func (c *pluginClient) setFiles(ctx context.Context, infoHash string, wanted any) error {
	return c.call(ctx, http.MethodPut, "/v1/torrents/"+infoHash+"/files", map[string]any{"wanted": wanted}, nil)
}

func (c *pluginClient) setWindow(ctx context.Context, infoHash, stream string, window pluginWindow) error {
	return c.call(ctx, http.MethodPut, "/v1/torrents/"+infoHash+"/windows/"+url.PathEscape(stream), window, nil)
}

func (c *pluginClient) removeWindow(ctx context.Context, infoHash, stream string) error {
	return c.call(ctx, http.MethodDelete, "/v1/torrents/"+infoHash+"/windows/"+url.PathEscape(stream), nil, nil)
}

func (c *pluginClient) metainfo(ctx context.Context, infoHash string) ([]byte, error) {
	var response struct {
		Torrent []byte `json:"torrent"`
	}
	err := c.call(ctx, http.MethodGet, "/v1/torrents/"+infoHash+"/metainfo", nil, &response)
	return response.Torrent, err
}

func (c *pluginClient) setListenPort(ctx context.Context, port int) error {
	return c.call(ctx, http.MethodPut, "/v1/listen-port", map[string]int{"port": port}, nil)
}

func (c *pluginClient) call(ctx context.Context, method, path string, body, result any) error {
	ctx, cancel := context.WithTimeout(ctx, pluginCallTimeout)
	defer cancel()
	return c.do(ctx, method, path, body, result)
}

func (c *pluginClient) do(ctx context.Context, method, path string, body, result any) error {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return fmt.Errorf("encode deluge plugin request: %w", err)
		}
	}
	token, err := c.bearerToken(false)
	if err != nil {
		return err
	}
	status, contents, err := c.send(ctx, method, path, payload, token)
	if err == nil && status == http.StatusUnauthorized {
		// The token file may have been rotated; re-read it once.
		if token, err = c.bearerToken(true); err != nil {
			return err
		}
		status, contents, err = c.send(ctx, method, path, payload, token)
	}
	if err != nil {
		return fmt.Errorf("deluge plugin %s %s: %w", method, path, err)
	}
	if status < 200 || status > 299 {
		var failure struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(contents, &failure)
		if failure.Error == "" {
			failure.Error = strings.TrimSpace(string(contents))
		}
		switch {
		case status == http.StatusNotFound && strings.Contains(failure.Error, "not found") && strings.Contains(failure.Error, "torrent"):
			return fmt.Errorf("%w: %s", errTorrentNotFound, failure.Error)
		case status == http.StatusConflict:
			return fmt.Errorf("%w: %s", errNoMetadata, failure.Error)
		}
		return &pluginError{status: status, message: failure.Error}
	}
	if result == nil {
		return nil
	}
	if err := json.Unmarshal(contents, result); err != nil {
		return fmt.Errorf("decode deluge plugin %s %s: %w", method, path, err)
	}
	return nil
}

func (c *pluginClient) send(ctx context.Context, method, path string, payload []byte, token string) (int, []byte, error) {
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return 0, nil, err
	}
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	contents, err := io.ReadAll(io.LimitReader(response.Body, 64<<20))
	return response.StatusCode, contents, err
}

func (c *pluginClient) bearerToken(reload bool) (string, error) {
	if c.tokenFile == "" {
		return "", nil
	}
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()
	if c.token != "" && !reload {
		return c.token, nil
	}
	contents, err := os.ReadFile(c.tokenFile)
	if err != nil {
		return "", fmt.Errorf("read deluge plugin token: %w", err)
	}
	c.token = strings.TrimSpace(string(contents))
	return c.token, nil
}

// wantedFiles is the plugin's file selection: "all" or explicit indices.
func wantedFiles(all bool, indices []int) any {
	if all {
		return "all"
	}
	if indices == nil {
		return []int{}
	}
	return indices
}

func encodeTorrent(contents []byte) string {
	return base64.StdEncoding.EncodeToString(contents)
}
