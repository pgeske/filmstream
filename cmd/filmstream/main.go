package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/pgeske/filmstream/internal/api"
	"github.com/pgeske/filmstream/internal/catalog"
	"github.com/pgeske/filmstream/internal/config"
	"github.com/pgeske/filmstream/internal/history"
	"github.com/pgeske/filmstream/internal/hls"
	"github.com/pgeske/filmstream/internal/indexer"
	"github.com/pgeske/filmstream/internal/metadata"
	"github.com/pgeske/filmstream/internal/playbackcache"
	"github.com/pgeske/filmstream/internal/resolver"
	"github.com/pgeske/filmstream/internal/torrentstream"
	"github.com/pgeske/filmstream/internal/usenetstream"
)

const version = "0.5.7"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "filmstream:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return runTUI(nil)
	}
	if len(args) > 0 {
		switch args[0] {
		case "serve":
			return runServer(args[1:])
		case "play":
			return runPlay(args[1:])
		case "status":
			return runStatus(args[1:])
		case "indexer":
			return runIndexer(args[1:])
		case "resolver":
			return runResolver(args[1:])
		case "resolve":
			return runResolve(args[1:])
		case "tui":
			return runTUI(args[1:])
		case "version", "--version", "-version":
			fmt.Println("filmstream", version)
			return nil
		case "help", "--help", "-h":
			printUsage()
			return nil
		}
	}
	return runPlay(args)
}

func runServer(args []string) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	configPath := flags.String("config", os.Getenv("FILMSTREAM_CONFIG"), "path to config file")
	torrentListenPort := flags.Int("torrent-listen-port", 0, "BitTorrent peer listen port (0 chooses an available port)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return err
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	registry, err := indexer.NewRegistry(cfg.Indexers)
	if err != nil {
		return err
	}
	engine, err := torrentstream.New(torrentstream.Config{
		DataDir:         cfg.DataDir,
		ListenPort:      *torrentListenPort,
		MaxTorrentBytes: cfg.MaxCandidateBytes(),
		ReadaheadBytes:  cfg.ReadaheadBytes(),
		MetadataTimeout: time.Duration(cfg.MetadataTimeoutSecs) * time.Second,
		SeedRatioTarget: cfg.SeedRatioTarget,
		CacheLimitBytes: cfg.CacheLimitBytes(),
		MaxSeedSessions: cfg.MaxSeedSessions,
		IdleGrace:       time.Duration(cfg.IdleGraceSeconds) * time.Second,
		SeedMaxAge:      time.Duration(cfg.SeedMaxHours) * time.Hour,
		CleanOnStart:    true,
		CleanOnClose:    true,
		Logger:          logger,
	})
	if err != nil {
		return err
	}
	defer engine.Close()

	usenetEngine, err := usenetstream.FromConfig(
		cfg.Usenet,
		time.Duration(cfg.IdleGraceSeconds)*time.Second,
		logger,
	)
	if err != nil {
		return err
	}
	if usenetEngine != nil {
		defer usenetEngine.Close()
	}

	sourceBaseURL, err := loopbackBaseURL(cfg.Listen)
	if err != nil {
		return err
	}
	hlsManager, hlsErr := hls.New(hls.Config{
		DataDir:        cfg.HLSDir,
		FFmpegPath:     cfg.FFmpegPath,
		FFprobePath:    cfg.FFprobePath,
		SourceBaseURL:  sourceBaseURL,
		StartupTimeout: time.Duration(cfg.HLSStartupSeconds) * time.Second,
		BufferSeconds:  cfg.HLSBufferSeconds,
		ReadRate:       cfg.HLSReadRate,
		SegmentSeconds: cfg.HLSSegmentSeconds,
		Logger:         logger,
	})
	if hlsErr != nil {
		logger.Warn("native HLS playback disabled", "error", hlsErr)
	} else {
		defer hlsManager.Close()
	}

	defaults := catalog.Preferences{
		Resolution:   cfg.PreferredResolution,
		Codecs:       []string{"h264", "h265"},
		Languages:    cfg.PreferredLanguages,
		MaxSizeBytes: cfg.MaxCandidateBytes(),
	}
	movieResolver, err := resolver.FromConfig(cfg.Resolver, cfg.DataDir)
	if err != nil {
		return err
	}
	metadataProvider, err := metadata.FromConfig(cfg.Metadata)
	if err != nil {
		return err
	}
	ratingsProvider, err := metadata.RatingsFromEnvironment()
	if err != nil {
		return err
	}
	apiServer := api.New(registry, engine, defaults, logger)
	apiServer.SetPlaybackSourceMode(cfg.PlaybackSourceMode)
	apiServer.SetUsenetEngine(usenetEngine)
	apiServer.SetMovieResolver(movieResolver)
	apiServer.SetMetadataProvider(metadataProvider)
	apiServer.SetRatingsProvider(ratingsProvider)
	apiServer.SetHistoryStore(history.New(cfg.StateDir))
	apiServer.SetPlaybackCache(playbackcache.New(cfg.StateDir))
	if hlsManager != nil {
		apiServer.SetHLSManager(hlsManager)
	}
	apiServer.SetResolverReloader(func() error {
		updated, err := config.Load(*configPath)
		if err != nil {
			return err
		}
		updatedResolver, err := resolver.FromConfig(updated.Resolver, updated.DataDir)
		if err != nil {
			return err
		}
		apiServer.SetMovieResolver(updatedResolver)
		return nil
	})
	apiServer.SetIndexerReloader(func() error {
		updated, err := config.Load(*configPath)
		if err != nil {
			return err
		}
		return registry.Replace(updated.Indexers)
	})
	server := &http.Server{
		Addr:              cfg.Listen,
		Handler:           apiServer.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	apiServer.StartPlaybackPrewarmer(ctx, sourceBaseURL)
	go func() {
		<-ctx.Done()
		shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownContext)
	}()

	logger.Info("filmstream server listening",
		"address", cfg.Listen,
		"data", cfg.DataDir,
		"torrent_listen_port", engine.ListenPort(),
		"usenet_enabled", usenetEngine != nil,
		"playback_source_mode", cfg.PlaybackSourceMode,
	)
	err = server.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func loopbackBaseURL(listen string) (string, error) {
	_, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "", fmt.Errorf("parse listen address for HLS: %w", err)
	}
	return "http://" + net.JoinHostPort("127.0.0.1", port), nil
}

func runPlay(args []string) error {
	flags := flag.NewFlagSet("play", flag.ContinueOnError)
	configPath := flags.String("config", os.Getenv("FILMSTREAM_CONFIG"), "path to config file")
	serverURL := flags.String("server", os.Getenv("FILMSTREAM_SERVER"), "server URL")
	magnet := flags.String("magnet", "", "stream a magnet URI directly")
	torrentPath := flags.String("torrent", "", "stream a local .torrent file directly")
	year := flags.Int("year", 0, "movie release year")
	resolution := flags.String("resolution", "", "preferred resolution")
	languages := flags.String("language", "", "comma-separated preferred languages")
	codecs := flags.String("codecs", "", "comma-separated accepted codecs")
	maxSize := flags.Int64("max-size-gib", 0, "maximum release size in GiB")
	player := flags.String("player", "", "player executable")
	printURL := flags.Bool("print-url", false, "print the stream URL without launching a player")
	noAI := flags.Bool("no-ai", false, "skip natural-language movie resolution")
	noAutostart := flags.Bool("no-autostart", false, "do not start a missing local server")
	if err := flags.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if *serverURL == "" {
		*serverURL = "http://" + cfg.Listen
	}
	if *player == "" {
		*player = cfg.Player
	}
	if *resolution == "" {
		*resolution = cfg.PreferredResolution
	}
	if *languages == "" {
		*languages = strings.Join(cfg.PreferredLanguages, ",")
	}
	if *codecs == "" {
		*codecs = "h264,h265"
	}
	if *maxSize == 0 {
		*maxSize = cfg.MaxCandidateGiB
	}

	query := strings.TrimSpace(strings.Join(flags.Args(), " "))
	if query == "" && *magnet == "" && *torrentPath == "" {
		return errors.New("provide a movie query, --magnet, or --torrent")
	}
	if *torrentPath != "" {
		absolute, err := filepath.Abs(*torrentPath)
		if err != nil {
			return err
		}
		*torrentPath = absolute
	}

	if err := ensureServer(*serverURL, *configPath, cfg.DataDir, !*noAutostart); err != nil {
		return err
	}
	if query != "" && !*noAI {
		var resolution resolver.Result
		if err := postJSON(*serverURL+"/v1/resolve", map[string]string{"query": query}, &resolution); err != nil {
			return fmt.Errorf("resolve movie: %w", err)
		}
		candidate, err := selectResolvedMovie(resolution)
		if err != nil {
			return err
		}
		if resolution.Provider != "" {
			cacheLabel := ""
			if resolution.Cached {
				cacheLabel = ", cached"
			}
			fmt.Fprintf(os.Stderr, "Resolved: %s%s\n", formatResolvedCandidate(candidate), cacheLabel)
		}
		query = candidate.Title
		if *year == 0 {
			*year = candidate.Year
		}
	}
	request := api.CreatePlaybackRequest{
		Query:       query,
		Year:        *year,
		MagnetURI:   *magnet,
		TorrentPath: *torrentPath,
		Preferences: catalog.Preferences{
			Resolution:   *resolution,
			Codecs:       splitList(*codecs),
			Languages:    splitList(*languages),
			MaxSizeBytes: *maxSize << 30,
		},
	}
	fmt.Fprintln(os.Stderr, "Finding a release and preparing the stream...")
	var response api.CreatePlaybackResponse
	if err := postJSON(*serverURL+"/v1/playbacks", request, &response); err != nil {
		return err
	}

	if response.Selected != nil {
		candidate := response.Selected.Candidate
		fmt.Fprintf(os.Stderr, "Selected: %s [score %.1f]\n", candidate.Name, response.Selected.Score)
	}
	fmt.Fprintf(os.Stderr, "Streaming: %s (%s)\n", response.FileName, formatBytes(response.FileSize))
	fmt.Fprintf(os.Stderr, "Playback: %s\n", response.ID)
	if *printURL {
		fmt.Println(response.StreamURL)
		return nil
	}

	path, err := exec.LookPath(*player)
	if err != nil {
		if *player == "mpv" {
			return fmt.Errorf("mpv is not installed; install it with 'sudo apt install mpv', use --player ffplay, or use --print-url")
		}
		return fmt.Errorf("find player %q: %w", *player, err)
	}
	if isMPV(path) {
		title := query
		if title == "" {
			title = response.Name
		}
		store := history.New(cfg.StateDir)
		entry, err := store.Upsert(title, *year)
		if err == nil {
			if err := runTrackedMPV(path, response.StreamURL, entry, store); err != nil {
				return err
			}
			fmt.Fprintln(os.Stderr, "Playback ended; Filmstream saved your progress and will retire temporary data automatically.")
			return nil
		}
		fmt.Fprintln(os.Stderr, "Warning: watch progress will not be saved:", err)
	}

	commandArgs := []string{response.StreamURL}
	if isMPV(path) {
		commandArgs = append(mpvStreamingOptions(path), response.StreamURL)
	} else if filepath.Base(path) == "ffplay" {
		commandArgs = []string{"-autoexit", response.StreamURL}
	}
	command := exec.Command(path, commandArgs...)
	command.Stdin = os.Stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		return err
	}
	if response.Source == catalog.ProtocolTorrent {
		fmt.Fprintln(os.Stderr, "Playback ended; Filmstream will seed and retire temporary data automatically.")
	} else {
		fmt.Fprintln(os.Stderr, "Playback ended; Filmstream will retire temporary session data automatically.")
	}
	return nil
}

func runStatus(args []string) error {
	flags := flag.NewFlagSet("status", flag.ContinueOnError)
	configPath := flags.String("config", os.Getenv("FILMSTREAM_CONFIG"), "path to config file")
	serverURL := flags.String("server", os.Getenv("FILMSTREAM_SERVER"), "server URL")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if len(flags.Args()) != 1 {
		return errors.New("usage: filmstream status PLAYBACK_ID")
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if *serverURL == "" {
		*serverURL = "http://" + cfg.Listen
	}
	response, err := http.Get(*serverURL + "/v1/playbacks/" + flags.Arg(0))
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return responseError(response)
	}
	var status struct {
		torrentstream.Status
		Source string `json:"source"`
	}
	if err := json.NewDecoder(response.Body).Decode(&status); err != nil {
		return err
	}
	if status.Source == catalog.ProtocolUsenet {
		fmt.Printf("%s\nfile: %s\nsource: Usenet\nstate: %s\nstreamed: %s\n",
			status.Name,
			status.FileName,
			status.State,
			formatBytes(status.DownloadedBytes),
		)
		return nil
	}
	fmt.Printf("%s\nfile: %s\nsource: torrent\nstate: %s\nstreamed: %.1f%%\nstored: %s\npeers: %d\nratio: %.3f / %.3f\n",
		status.Name,
		status.FileName,
		status.State,
		percent(status.BytesComplete, status.FileSize),
		formatBytes(status.TorrentComplete),
		status.ActivePeers,
		status.Ratio,
		status.RatioTarget,
	)
	return nil
}

func ensureServer(baseURL, configPath, dataDir string, autostart bool) error {
	if healthy(baseURL) {
		return nil
	}
	if !autostart {
		return errors.New("filmstream server is not running")
	}
	if !strings.HasPrefix(baseURL, "http://127.0.0.1:") && !strings.HasPrefix(baseURL, "http://localhost:") {
		return errors.New("refusing to autostart a non-local server")
	}

	executable, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return err
	}
	logFile, err := os.OpenFile(filepath.Join(dataDir, "server.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	arguments := []string{"serve"}
	if configPath != "" {
		arguments = append(arguments, "--config", configPath)
	}
	command := exec.Command(executable, arguments...)
	command.Stdin = nil
	command.Stdout = logFile
	command.Stderr = logFile
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		logFile.Close()
		return fmt.Errorf("start filmstream server: %w", err)
	}
	_ = logFile.Close()
	_ = command.Process.Release()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if healthy(baseURL) {
			return nil
		}
		time.Sleep(150 * time.Millisecond)
	}
	return fmt.Errorf("server did not become ready; check %s", filepath.Join(dataDir, "server.log"))
}

func healthy(baseURL string) bool {
	client := &http.Client{Timeout: 400 * time.Millisecond}
	response, err := client.Get(baseURL + "/v1/health")
	if err != nil {
		return false
	}
	defer response.Body.Close()
	return response.StatusCode == http.StatusOK
}

func postJSON(endpoint string, request, response any) error {
	body, err := json.Marshal(request)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 3 * time.Minute}
	httpResponse, err := client.Post(endpoint, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer httpResponse.Body.Close()
	if httpResponse.StatusCode < 200 || httpResponse.StatusCode >= 300 {
		return responseError(httpResponse)
	}
	return json.NewDecoder(httpResponse.Body).Decode(response)
}

func responseError(response *http.Response) error {
	contents, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	var payload struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(contents, &payload) == nil && payload.Error != "" {
		return errors.New(payload.Error)
	}
	return fmt.Errorf("server returned %s: %s", response.Status, strings.TrimSpace(string(contents)))
}

func splitList(value string) []string {
	var result []string
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			result = append(result, item)
		}
	}
	return result
}

func formatBytes(value int64) string {
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	size := float64(value)
	unit := 0
	for size >= 1024 && unit < len(units)-1 {
		size /= 1024
		unit++
	}
	return strconv.FormatFloat(size, 'f', 1, 64) + " " + units[unit]
}

func percent(part, total int64) float64 {
	if total <= 0 {
		return 0
	}
	return float64(part) / float64(total) * 100
}

func printUsage() {
	fmt.Print(`filmstream streams authorized Usenet or torrent media to a local player.

Usage:
  filmstream                         # open the TUI
  filmstream [play] [options] MOVIE QUERY
  filmstream play --magnet 'magnet:?...'
  filmstream play --torrent ./movie.torrent
  filmstream serve
  filmstream status PLAYBACK_ID
  filmstream indexer add --name NAME URL
  filmstream indexer list
  filmstream resolve MOVIE DESCRIPTION
  filmstream resolver configure [options]
  filmstream tui [--config PATH]

Examples:
  filmstream --year 2010 Sintel
  filmstream --resolution 1080p --language en "public domain movie"
  filmstream --magnet 'magnet:?...'
`)
}
