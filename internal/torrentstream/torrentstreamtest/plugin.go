// Package torrentstreamtest fakes the TeaStream Deluge plugin API so torrent
// playback can be tested without Deluge or a swarm.
package torrentstreamtest

import (
	"bytes"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// File is one file of a fake torrent. Path is the path Deluge reports: the
// torrent name for a single-file torrent, "<name>/<path>" otherwise.
type File struct {
	Path string
	Data []byte
}

// Torrent describes a fake torrent and its payload.
type Torrent struct {
	Name        string
	PieceLength int64
	Private     bool
	Files       []File
}

// Stats are swarm and transfer figures the fake reports for a torrent.
type Stats struct {
	NumPeers        int
	NumSeeds        int
	ListPeers       int
	DownloadRate    int64
	UploadRate      int64
	AllTimeUpload   int64
	SeedingSeconds  int64
	FinishedSeconds int64
	TrackerMessage  string
}

// Window is a stream window set through the API.
type Window struct {
	File          int   `json:"file"`
	Offset        int64 `json:"offset"`
	Length        int64 `json:"length"`
	DeadlineBytes int64 `json:"deadline_bytes"`
	TTLMillis     int64 `json:"ttl_ms"`
}

// Plugin is a running fake plugin API.
type Plugin struct {
	URL          string
	TokenFile    string
	DownloadsDir string

	token  string
	server *httptest.Server

	mu              sync.Mutex
	changed         chan struct{}
	known           map[string]*known
	torrents        map[string]*torrent
	listenPort      int
	autoComplete    bool
	delayMetadata   bool
	windowRequests  int
	removedWithData map[string]bool
}

type known struct {
	def      Torrent
	hash     string
	metainfo []byte
}

type torrent struct {
	*known
	savePath    string
	hasMetadata bool
	pending     any
	priorities  []int
	have        []bool
	windows     map[string]Window
	stats       Stats
	downloaded  int64
}

// NewPlugin starts a fake plugin with a token file and downloads directory
// beneath tb's temporary directory.
func NewPlugin(tb testing.TB) *Plugin {
	tb.Helper()
	dir := tb.TempDir()
	p := &Plugin{
		TokenFile:       filepath.Join(dir, "token"),
		DownloadsDir:    filepath.Join(dir, "downloads"),
		token:           "test-token",
		changed:         make(chan struct{}),
		known:           make(map[string]*known),
		torrents:        make(map[string]*torrent),
		removedWithData: make(map[string]bool),
	}
	if err := os.WriteFile(p.TokenFile, []byte(p.token+"\n"), 0o600); err != nil {
		tb.Fatal(err)
	}
	p.server = httptest.NewServer(http.HandlerFunc(p.serve))
	p.URL = p.server.URL
	tb.Cleanup(p.server.Close)
	return p
}

// Register makes a torrent addable by its metainfo or magnet URI.
func (p *Plugin) Register(def Torrent) string {
	metainfo, hash := def.encode()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.known[hash] = &known{def: def, hash: hash, metainfo: metainfo}
	return hash
}

// SetAutoComplete makes every wanted or windowed piece complete at once.
func (p *Plugin) SetAutoComplete(enabled bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.autoComplete = enabled
	for _, t := range p.torrents {
		p.autoCompleteLocked(t)
	}
	p.notifyLocked()
}

// SetMetadataDelayed keeps magnets without metadata until ProvideMetadata.
func (p *Plugin) SetMetadataDelayed(delayed bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.delayMetadata = delayed
}

// ProvideMetadata delivers a delayed magnet's metadata.
func (p *Plugin) ProvideMetadata(hash string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if t := p.torrents[hash]; t != nil {
		p.receiveMetadataLocked(t)
	}
}

// Complete verifies pieces first..last and writes their bytes to disk.
func (p *Plugin) Complete(hash string, first, last int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	t := p.torrents[hash]
	if t == nil {
		return
	}
	for piece := max(first, 0); piece <= last && piece < len(t.have); piece++ {
		p.completeLocked(t, piece)
	}
	p.notifyLocked()
}

// CompleteAll verifies every piece.
func (p *Plugin) CompleteAll(hash string) {
	p.Complete(hash, 0, 1<<30)
}

// Have reports whether a piece is verified.
func (p *Plugin) Have(hash string, piece int) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	t := p.torrents[hash]
	return t != nil && piece >= 0 && piece < len(t.have) && t.have[piece]
}

// SetStats edits a torrent's reported figures.
func (p *Plugin) SetStats(hash string, edit func(*Stats)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if t := p.torrents[hash]; t != nil {
		edit(&t.stats)
	}
	p.notifyLocked()
}

// Windows returns the torrent's current stream windows by stream ID.
func (p *Plugin) Windows(hash string) map[string]Window {
	p.mu.Lock()
	defer p.mu.Unlock()
	windows := make(map[string]Window)
	if t := p.torrents[hash]; t != nil {
		for stream, window := range t.windows {
			windows[stream] = window
		}
	}
	return windows
}

// WindowRequests counts window updates received.
func (p *Plugin) WindowRequests() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.windowRequests
}

// Priorities returns the torrent's file priorities.
func (p *Plugin) Priorities(hash string) []int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if t := p.torrents[hash]; t != nil {
		return append([]int(nil), t.priorities...)
	}
	return nil
}

// Has reports whether the torrent is in the fake session.
func (p *Plugin) Has(hash string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.torrents[hash] != nil
}

// RemovedWithData reports whether the API removed the torrent with its data.
func (p *Plugin) RemovedWithData(hash string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.removedWithData[hash]
}

// Forget drops a torrent as if Deluge lost it.
func (p *Plugin) Forget(hash string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.torrents, hash)
	p.notifyLocked()
}

// ListenPort returns the last port set through the API.
func (p *Plugin) ListenPort() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.listenPort
}

func (p *Plugin) notifyLocked() {
	close(p.changed)
	p.changed = make(chan struct{})
}

func (p *Plugin) serve(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) == 0 || parts[0] != "v1" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	parts = parts[1:]
	if len(parts) == 1 && parts[0] == "health" {
		p.mu.Lock()
		port := p.listenPort
		p.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deluge": "fake", "libtorrent": "fake", "listen_port": port, "token_configured": true})
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+p.token {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid or missing bearer token"})
		return
	}
	var body map[string]json.RawMessage
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	status, payload := p.route(r, parts, body)
	if status == 0 {
		return
	}
	writeJSON(w, status, payload)
}

func (p *Plugin) route(r *http.Request, parts []string, body map[string]json.RawMessage) (int, any) {
	method := r.Method
	switch {
	case len(parts) == 1 && parts[0] == "torrents" && method == http.MethodGet:
		p.mu.Lock()
		defer p.mu.Unlock()
		summaries := make([]map[string]any, 0, len(p.torrents))
		hashes := make([]string, 0, len(p.torrents))
		for hash := range p.torrents {
			hashes = append(hashes, hash)
		}
		sort.Strings(hashes)
		for _, hash := range hashes {
			summaries = append(summaries, p.summaryLocked(p.torrents[hash]))
		}
		return http.StatusOK, map[string]any{"torrents": summaries}
	case len(parts) == 1 && parts[0] == "torrents" && method == http.MethodPost:
		return p.add(body)
	case len(parts) == 1 && parts[0] == "listen-port" && method == http.MethodPut:
		var port int
		_ = json.Unmarshal(body["port"], &port)
		p.mu.Lock()
		p.listenPort = port
		p.mu.Unlock()
		return http.StatusOK, map[string]int{"port": port}
	case len(parts) >= 2 && parts[0] == "torrents":
		return p.routeTorrent(r, parts[1], parts[2:], body)
	}
	return http.StatusNotFound, map[string]string{"error": "no such endpoint"}
}

func (p *Plugin) routeTorrent(r *http.Request, hash string, rest []string, body map[string]json.RawMessage) (int, any) {
	p.mu.Lock()
	t := p.torrents[hash]
	if t == nil {
		p.mu.Unlock()
		return http.StatusNotFound, map[string]string{"error": "torrent " + hash + " not found"}
	}
	method := r.Method
	switch {
	case len(rest) == 0 && method == http.MethodGet:
		defer p.mu.Unlock()
		return http.StatusOK, p.detailLocked(t)
	case len(rest) == 0 && method == http.MethodDelete:
		defer p.mu.Unlock()
		removeData := r.URL.Query().Get("remove_data") == "1"
		delete(p.torrents, hash)
		p.removedWithData[hash] = removeData
		if removeData {
			_ = os.RemoveAll(t.savePath)
		}
		p.notifyLocked()
		return http.StatusOK, map[string]bool{"removed": true}
	case len(rest) == 1 && rest[0] == "metainfo" && method == http.MethodGet:
		defer p.mu.Unlock()
		return http.StatusOK, map[string]string{"torrent": base64.StdEncoding.EncodeToString(t.metainfo)}
	}
	if !t.hasMetadata {
		p.mu.Unlock()
		return http.StatusConflict, map[string]string{"error": "torrent metadata is not available yet"}
	}
	switch {
	case len(rest) == 1 && rest[0] == "files" && method == http.MethodPut:
		defer p.mu.Unlock()
		var wanted any
		_ = json.Unmarshal(body["wanted"], &wanted)
		t.pending = nil
		t.priorities = priorities(len(t.def.Files), wanted)
		p.autoCompleteLocked(t)
		p.notifyLocked()
		return http.StatusOK, map[string]any{"priorities": t.priorities}
	case len(rest) == 2 && rest[0] == "windows" && method == http.MethodPut:
		defer p.mu.Unlock()
		var window Window
		encoded, _ := json.Marshal(body)
		_ = json.Unmarshal(encoded, &window)
		stream, _ := url.PathUnescape(rest[1])
		if window.File < 0 || window.File >= len(t.def.Files) || window.Offset < 0 || window.Offset >= int64(len(t.def.Files[window.File].Data)) {
			return http.StatusBadRequest, map[string]string{"error": "window outside file"}
		}
		t.windows[stream] = window
		p.windowRequests++
		p.autoCompleteLocked(t)
		p.notifyLocked()
		first, last := t.windowPieces(window)
		return http.StatusOK, map[string]int{"first_piece": first, "last_piece": last, "deadline_last_piece": last}
	case len(rest) == 2 && rest[0] == "windows" && method == http.MethodDelete:
		defer p.mu.Unlock()
		stream, _ := url.PathUnescape(rest[1])
		_, removed := t.windows[stream]
		delete(t.windows, stream)
		return http.StatusOK, map[string]bool{"removed": removed}
	case len(rest) == 1 && rest[0] == "pieces" && method == http.MethodGet:
		defer p.mu.Unlock()
		first, _ := strconv.Atoi(r.URL.Query().Get("first"))
		last := len(t.have) - 1
		if value := r.URL.Query().Get("last"); value != "" {
			last, _ = strconv.Atoi(value)
		}
		return http.StatusOK, piecesResponse(t.have, first, last, allHave(t.have, first, last))
	case len(rest) == 1 && rest[0] == "wait" && method == http.MethodPost:
		var piece, through, timeoutMillis int
		_ = json.Unmarshal(body["piece"], &piece)
		through = piece
		_ = json.Unmarshal(body["through"], &through)
		timeoutMillis = 5000
		_ = json.Unmarshal(body["timeout_ms"], &timeoutMillis)
		if piece < 0 || piece >= len(t.have) || through < piece || through >= len(t.have) {
			p.mu.Unlock()
			return http.StatusBadRequest, map[string]string{"error": "piece out of range"}
		}
		deadline := time.NewTimer(time.Duration(timeoutMillis) * time.Millisecond)
		defer deadline.Stop()
		for {
			current := p.torrents[hash]
			if current == nil {
				p.mu.Unlock()
				return http.StatusNotFound, map[string]string{"error": "torrent removed"}
			}
			if current.have[piece] {
				defer p.mu.Unlock()
				return http.StatusOK, piecesResponse(current.have, piece, through, true)
			}
			changed := p.changed
			p.mu.Unlock()
			select {
			case <-changed:
			case <-deadline.C:
				p.mu.Lock()
				defer p.mu.Unlock()
				if current := p.torrents[hash]; current != nil {
					return http.StatusOK, piecesResponse(current.have, piece, through, current.have[piece])
				}
				return http.StatusNotFound, map[string]string{"error": "torrent removed"}
			case <-r.Context().Done():
				return 0, nil
			}
			p.mu.Lock()
		}
	}
	p.mu.Unlock()
	return http.StatusNotFound, map[string]string{"error": "no such endpoint"}
}

func (p *Plugin) add(body map[string]json.RawMessage) (int, any) {
	var torrentB64, magnet, saveRoot string
	var wanted any
	_ = json.Unmarshal(body["torrent"], &torrentB64)
	_ = json.Unmarshal(body["magnet"], &magnet)
	_ = json.Unmarshal(body["save_root"], &saveRoot)
	_ = json.Unmarshal(body["wanted_files"], &wanted)
	if saveRoot == "" {
		saveRoot = p.DownloadsDir
	}
	var hash string
	fromMagnet := false
	switch {
	case torrentB64 != "":
		contents, err := base64.StdEncoding.DecodeString(torrentB64)
		if err != nil {
			return http.StatusBadRequest, map[string]string{"error": "invalid torrent file"}
		}
		hash = hashOfMetainfo(contents)
	case magnet != "":
		fromMagnet = true
		parsed, err := url.Parse(magnet)
		if err != nil {
			return http.StatusBadRequest, map[string]string{"error": "invalid magnet URI"}
		}
		hash = strings.ToLower(strings.TrimPrefix(parsed.Query().Get("xt"), "urn:btih:"))
	default:
		return http.StatusBadRequest, map[string]string{"error": "provide exactly one of torrent or magnet"}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if existing := p.torrents[hash]; existing != nil {
		return http.StatusOK, map[string]any{"info_hash": hash, "added": false, "save_path": existing.savePath}
	}
	k := p.known[hash]
	if k == nil {
		return http.StatusBadRequest, map[string]string{"error": "unknown torrent " + hash}
	}
	t := &torrent{known: k, savePath: filepath.Join(saveRoot, hash), windows: make(map[string]Window)}
	p.torrents[hash] = t
	if fromMagnet {
		t.pending = wanted
		if !p.delayMetadata {
			p.receiveMetadataLocked(t)
		}
	} else {
		t.hasMetadata = true
		t.initPieces()
		t.priorities = priorities(len(k.def.Files), wanted)
		p.autoCompleteLocked(t)
	}
	p.notifyLocked()
	return http.StatusOK, map[string]any{"info_hash": hash, "added": true, "save_path": t.savePath}
}

func (p *Plugin) receiveMetadataLocked(t *torrent) {
	if t.hasMetadata {
		return
	}
	t.hasMetadata = true
	t.initPieces()
	t.priorities = priorities(len(t.def.Files), t.pending)
	t.pending = nil
	p.autoCompleteLocked(t)
	p.notifyLocked()
}

func (t *torrent) initPieces() {
	t.have = make([]bool, t.def.numPieces())
}

func (p *Plugin) autoCompleteLocked(t *torrent) {
	if !p.autoComplete || !t.hasMetadata {
		return
	}
	for index, priority := range t.priorities {
		if priority > 0 {
			first, last := t.fileSpan(index, 0, int64(len(t.def.Files[index].Data)))
			for piece := first; piece <= last; piece++ {
				p.completeLocked(t, piece)
			}
		}
	}
	for _, window := range t.windows {
		first, last := t.windowPieces(window)
		for piece := first; piece <= last; piece++ {
			p.completeLocked(t, piece)
		}
	}
}

// completeLocked marks a piece verified and writes its bytes into the files
// it overlaps, like libtorrent writing a verified piece.
func (p *Plugin) completeLocked(t *torrent, piece int) {
	if t.have[piece] {
		return
	}
	t.have[piece] = true
	length := t.def.PieceLength
	start := int64(piece) * length
	end := start + length
	offset := int64(0)
	for _, file := range t.def.Files {
		size := int64(len(file.Data))
		fileStart, fileEnd := offset, offset+size
		offset = fileEnd
		from, to := max(start, fileStart), min(end, fileEnd)
		if from >= to {
			continue
		}
		path := filepath.Join(t.savePath, filepath.FromSlash(file.Path))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			panic(err)
		}
		handle, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
		if err != nil {
			panic(err)
		}
		if err := handle.Truncate(size); err != nil {
			panic(err)
		}
		if _, err := handle.WriteAt(file.Data[from-fileStart:to-fileStart], from-fileStart); err != nil {
			panic(err)
		}
		_ = handle.Close()
		t.downloaded += to - from
	}
}

func (t *torrent) fileSpan(index int, offset, length int64) (int, int) {
	fileOffset := int64(0)
	for i := 0; i < index; i++ {
		fileOffset += int64(len(t.def.Files[i].Data))
	}
	size := int64(len(t.def.Files[index].Data))
	length = max(1, min(length, size-offset))
	return int((fileOffset + offset) / t.def.PieceLength), int((fileOffset + offset + length - 1) / t.def.PieceLength)
}

func (t *torrent) windowPieces(window Window) (int, int) {
	return t.fileSpan(window.File, window.Offset, window.Length)
}

func (p *Plugin) summaryLocked(t *torrent) map[string]any {
	total := t.def.totalSize()
	summary := map[string]any{
		"info_hash": t.hash, "name": t.def.Name, "state": "Downloading", "has_metadata": t.hasMetadata,
		"private": t.hasMetadata && t.def.Private, "save_path": t.savePath,
		"num_peers": t.stats.NumPeers, "num_seeds": t.stats.NumSeeds, "list_peers": t.stats.ListPeers,
		"connect_candidates": 0, "download_rate": t.stats.DownloadRate, "upload_rate": t.stats.UploadRate,
		"all_time_download": t.downloaded, "all_time_upload": t.stats.AllTimeUpload,
		"seeding_seconds": t.stats.SeedingSeconds, "finished_seconds": t.stats.FinishedSeconds,
		"tracker_message": t.stats.TrackerMessage, "tracker_status": "Announce OK",
	}
	if !t.hasMetadata {
		summary["file_progress"] = []int64{}
		return summary
	}
	var done int64
	fileProgress := make([]int64, len(t.def.Files))
	for piece, have := range t.have {
		if !have {
			continue
		}
		start := int64(piece) * t.def.PieceLength
		end := min(start+t.def.PieceLength, total)
		done += end - start
		offset := int64(0)
		for index, file := range t.def.Files {
			fileEnd := offset + int64(len(file.Data))
			if from, to := max(start, offset), min(end, fileEnd); from < to {
				fileProgress[index] += to - from
			}
			offset = fileEnd
		}
	}
	seeding := done == total
	summary["total_size"] = total
	summary["total_done"] = done
	summary["progress"] = float64(done) / float64(total)
	summary["is_seeding"] = seeding
	summary["is_finished"] = seeding
	summary["file_progress"] = fileProgress
	return summary
}

func (p *Plugin) detailLocked(t *torrent) map[string]any {
	detail := p.summaryLocked(t)
	detail["trackers"] = []string{"http://tracker.invalid/announce"}
	if !t.hasMetadata {
		detail["piece_length"], detail["num_pieces"], detail["files"] = 0, 0, []any{}
		return detail
	}
	progress := detail["file_progress"].([]int64)
	files := make([]map[string]any, 0, len(t.def.Files))
	offset := int64(0)
	for index, file := range t.def.Files {
		files = append(files, map[string]any{
			"index": index, "path": file.Path, "size": len(file.Data), "offset": offset, "pad": false,
			"priority": t.priorities[index], "done": progress[index],
		})
		offset += int64(len(file.Data))
	}
	detail["piece_length"] = t.def.PieceLength
	detail["num_pieces"] = len(t.have)
	detail["files"] = files
	return detail
}

func priorities(count int, wanted any) []int {
	result := make([]int, count)
	switch wanted := wanted.(type) {
	case string:
		if wanted == "all" {
			for i := range result {
				result[i] = 4
			}
		}
	case []any:
		for _, value := range wanted {
			if index, ok := value.(float64); ok && int(index) >= 0 && int(index) < count {
				result[int(index)] = 4
			}
		}
	}
	return result
}

func allHave(have []bool, first, last int) bool {
	for piece := first; piece <= last; piece++ {
		if !have[piece] {
			return false
		}
	}
	return true
}

func piecesResponse(have []bool, first, last int, complete bool) map[string]any {
	packed := make([]byte, (last-first+8)/8)
	for i := 0; i <= last-first; i++ {
		if have[first+i] {
			packed[i>>3] |= 0x80 >> (i & 7)
		}
	}
	return map[string]any{
		"first": first, "last": last, "bitfield": base64.StdEncoding.EncodeToString(packed), "complete": complete,
	}
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func (t Torrent) totalSize() int64 {
	var total int64
	for _, file := range t.Files {
		total += int64(len(file.Data))
	}
	return total
}

func (t Torrent) numPieces() int {
	return int((t.totalSize() + t.PieceLength - 1) / t.PieceLength)
}

// Metainfo returns the torrent's bencoded .torrent file.
func (t Torrent) Metainfo() []byte {
	metainfo, _ := t.encode()
	return metainfo
}

// InfoHash returns the torrent's hex info hash.
func (t Torrent) InfoHash() string {
	_, hash := t.encode()
	return hash
}

// Magnet returns a magnet URI for the torrent.
func (t Torrent) Magnet() string {
	return "magnet:?xt=urn:btih:" + t.InfoHash() + "&dn=" + url.QueryEscape(t.Name)
}

func (t Torrent) encode() ([]byte, string) {
	if t.PieceLength <= 0 {
		panic("torrentstreamtest: PieceLength must be positive")
	}
	var payload bytes.Buffer
	for _, file := range t.Files {
		payload.Write(file.Data)
	}
	var pieces bytes.Buffer
	for start := 0; start < payload.Len(); start += int(t.PieceLength) {
		sum := sha1.Sum(payload.Bytes()[start:min(payload.Len(), start+int(t.PieceLength))])
		pieces.Write(sum[:])
	}
	var info bytes.Buffer
	info.WriteString("d")
	single := len(t.Files) == 1 && t.Files[0].Path == t.Name
	if !single {
		info.WriteString("5:filesl")
		for _, file := range t.Files {
			components := strings.Split(strings.TrimPrefix(file.Path, t.Name+"/"), "/")
			fmt.Fprintf(&info, "d6:lengthi%de4:pathl", len(file.Data))
			for _, component := range components {
				writeString(&info, component)
			}
			info.WriteString("ee")
		}
		info.WriteString("e")
	} else {
		fmt.Fprintf(&info, "6:lengthi%de", len(t.Files[0].Data))
	}
	info.WriteString("4:name")
	writeString(&info, t.Name)
	fmt.Fprintf(&info, "12:piece lengthi%de6:pieces", t.PieceLength)
	writeString(&info, pieces.String())
	if t.Private {
		info.WriteString("7:privatei1e")
	}
	info.WriteString("e")
	sum := sha1.Sum(info.Bytes())
	var metainfo bytes.Buffer
	metainfo.WriteString("d8:announce")
	writeString(&metainfo, "http://tracker.invalid/announce")
	metainfo.WriteString("4:info")
	metainfo.Write(info.Bytes())
	metainfo.WriteString("e")
	return metainfo.Bytes(), hex.EncodeToString(sum[:])
}

func writeString(buffer *bytes.Buffer, value string) {
	fmt.Fprintf(buffer, "%d:%s", len(value), value)
}

// hashOfMetainfo finds the info dictionary of a metainfo produced by encode.
func hashOfMetainfo(contents []byte) string {
	start := bytes.Index(contents, []byte("4:infod"))
	if start < 0 || len(contents) < start+7 {
		return ""
	}
	sum := sha1.Sum(contents[start+6 : len(contents)-1])
	return hex.EncodeToString(sum[:])
}
