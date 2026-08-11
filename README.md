# filmstream

`filmstream` is a local Go service that streams authorized torrent media to MPV over HTTP. It can search pluggable indexers, rank releases deterministically, or bypass discovery and play a supplied magnet URI or `.torrent` file.

Use it only with media you are authorized to download and share.

## Current MVP

- Local backend bound to `127.0.0.1:8943`
- Bubble Tea terminal UI with search, continue watching, progress, and history controls
- Native tvOS client with poster search, movie details, AVPlayer HLS playback, and shared progress
- One-command CLI that starts the backend when needed and launches MPV
- A small, trusted open-movie catalog plus an Internet Archive reference indexer
- Standard Torznab indexers, including Prowlarr and Jackett endpoints
- Optional natural-language movie resolution through OpenAI-compatible models
- Release ranking by title, year, resolution, language, codec, size, seeders, and leechers
- Direct magnet and `.torrent` playback
- HTTP Range streaming with seeking and 32 MiB read-ahead
- Demand-driven smart sampling plus a small tail prefetch for container metadata
- Automatic best-effort seeding toward a visible 1.0 ratio target
- Automatic retirement, a 20 GiB cache safety limit, and ephemeral shutdown cleanup
- Largest-video selection for multi-file torrents

## Install

Go 1.24 or newer and MPV are required. On Ubuntu:

```bash
sudo apt install mpv
make install
```

This installs `filmstream` to `~/.local/bin` by default. Ubuntu 22.04 ships MPV 0.34.1; it works through Filmstream's compatibility path. Under WSL, a current native Windows MPV can instead be selected with its mounted path, such as `/mnt/c/Users/you/mpv/mpv.com`; Filmstream preserves watch-progress tracking through a temporary MPV script.

## Use

Open the terminal UI with no arguments:

```bash
filmstream
```

The home screen separates resumable movies from watch history. Use:

- `/` or `s` to search by title, typo, or description
- `Enter` to play or resume the selected movie
- `u` to mark the selected movie unwatched
- `d` to remove it from tracking
- `q` to quit

Filmstream records MPV position through its local IPC socket. Continue Watching starts after 30 seconds, and a movie is marked watched at 90%. History is stored privately in `~/.local/state/filmstream/history.json`; torrent data remains temporary and is not needed to remember progress. Resuming searches for an appropriate release again, then starts the new stream at the saved timestamp.

The direct CLI searches configured indexers, selects a release, starts the local server if necessary, and opens MPV:

```bash
filmstream --year 2010 Sintel
filmstream "Night of the Living Dead"
```

The title may be passed as separate shell arguments. Years are only interpreted when supplied with `--year`, so a title such as `2001 A Space Odyssey` remains intact. When a model resolver is configured, the query can also contain typos, shorthand, franchise ordinals, remembered scenes, or a plot description:

```bash
filmstream "the first lotr movie"
filmstream "animated movie about an old man who flies away in his house"
```

Filmstream shows the canonical title and confidence before searching indexers. Use `--no-ai` to search the original text unchanged.

Structured overrides are optional:

```bash
filmstream \
  --resolution 1080p \
  --language en \
  --codecs h264,h265 \
  --max-size-gib 60 \
  "movie title"
```

H.264 and H.265 are soft preferences, not hard filters. A good release is not discarded solely because its codec is unknown or different.

Direct sources skip search and ranking:

```bash
filmstream --magnet 'magnet:?xt=urn:btih:...'
filmstream --torrent ./authorized-movie.torrent
```

Useful development commands:

```bash
filmstream serve                   # run the backend in the foreground
filmstream serve --torrent-listen-port 51413  # use a VPN-forwarded peer port
filmstream --print-url Sintel      # prepare a stream without opening a player
filmstream --player ffplay Sintel  # temporary fallback if MPV is unavailable
filmstream status PLAYBACK_ID
```

An automatically started server logs to `~/.cache/filmstream/server.log`.

## Apple TV client

The SwiftUI client lives in `clients/apple`. Its shared `FilmstreamCore` package supports future iOS and macOS targets, while `FilmstreamTV` uses AVPlayer for native Apple TV playback and remote controls. The backend copies compatible H.264/H.265 video into fragmented HLS and converts the selected audio track to AAC; the raw HTTP stream remains available to MPV.

```bash
make apple-test
make tvos-build
```

The app uses the server for catalog search, torrent preparation, and durable watch progress. See [`clients/apple/README.md`](clients/apple/README.md) for project generation, simulator, TMDB, and networking details.

## Configuration

The optional configuration file is `~/.config/filmstream/config.json`:

```json
{
  "listen": "127.0.0.1:8943",
  "data_dir": "~/.cache/filmstream",
  "state_dir": "~/.local/state/filmstream",
  "hls_dir": "~/.cache/filmstream/hls",
  "ffmpeg_path": "ffmpeg",
  "ffprobe_path": "ffprobe",
  "hls_startup_seconds": 90,
  "hls_startup_buffer_seconds": 16,
  "hls_segment_seconds": 4,
  "max_candidate_gib": 60,
  "readahead_mib": 32,
  "metadata_timeout_seconds": 120,
  "seed_ratio_target": 1.0,
  "cache_limit_gib": 20,
  "max_seed_sessions": 20,
  "seed_max_hours": 24,
  "idle_grace_seconds": 120,
  "preferred_resolution": "1080p",
  "preferred_languages": ["en", "english"],
  "player": "mpv",
  "resolver": {
    "provider": "openai-compatible",
    "base_url": "https://api.openai.com/v1",
    "model": "gpt-5-nano",
    "api_key_file": "~/.config/filmstream/openai-api-key",
    "timeout_seconds": 60
  },
  "metadata": {
    "provider": "tmdb",
    "base_url": "https://api.themoviedb.org/3",
    "language": "en-US",
    "api_key_file": "~/.config/filmstream/tmdb-api-key",
    "timeout_seconds": 30
  },
  "indexers": [
    {
      "name": "open-media",
      "type": "open_media",
      "endpoint": "https://webtorrent.io/torrents"
    },
    {
      "name": "internet-archive",
      "type": "internet_archive",
      "endpoint": "https://archive.org"
    }
  ]
}
```

Set `FILMSTREAM_CONFIG` to use another path or `FILMSTREAM_SERVER` to use an already-running backend.

Temporary torrent data is stored beneath `<data_dir>/torrents`, and temporary native-player segments are stored beneath `<hls_dir>`. Filmstream owns and clears both locations on clean startup and shutdown; do not place unrelated files there. Durable watch progress is stored separately beneath `<state_dir>`.

## Natural-language movie resolution

Filmstream uses the common OpenAI-compatible chat-completions API, so the same implementation works with OpenAI and self-hosted servers such as Ollama, vLLM, llama.cpp, LM Studio, and LocalAI.

Configure OpenAI with a key file:

```bash
filmstream resolver configure \
  --base-url https://api.openai.com/v1 \
  --model gpt-5-nano \
  --api-key-file ~/.config/filmstream/openai-api-key
```

Configure an unauthenticated local Ollama endpoint:

```bash
filmstream resolver configure \
  --base-url http://localhost:11434/v1 \
  --model your-local-model \
  --no-api-key
```

Inspect behavior without starting playback:

```bash
filmstream resolve "the first lotr movie"
filmstream resolver test "movie where dreams have dreams inside them"
filmstream resolver disable
```

The model returns up to five structured title/year hypotheses. Filmstream validates the JSON locally, asks the user when results are ambiguous, and then sends the selected canonical title and year through the normal Torznab and deterministic-ranking pipeline. The model never chooses a torrent. Successful resolutions are cached in `<data_dir>/resolver-cache.json` with private file permissions.

API keys may come from a private file, a named environment variable, or a secure interactive prompt. They are never accepted as command-line values.

## Torznab indexers

Every user-registered indexer must expose a standard Torznab endpoint. The endpoint may come directly from an indexer or from software such as Prowlarr or Jackett.

Register an endpoint:

```bash
filmstream indexer add \
  --name movies \
  http://prowlarr.example.internal/1/api
```

The CLI prompts for its API key without echoing it or placing it in shell history. For a public endpoint that requires no key, pass `--no-api-key`. For non-interactive setup, provide the key through `FILMSTREAM_INDEXER_API_KEY`.

Manage registrations with:

```bash
filmstream indexer list
filmstream indexer test movies
filmstream indexer remove movies
```

Registration calls the Torznab `t=caps` endpoint and refuses endpoints that support neither basic nor movie searches. Each configured endpoint is searched concurrently. Filmstream normalizes standard Torznab fields such as size, seeders, peers, magnet/download URL, language, release group, and upload/download volume factors before ranking candidates.

Prowlarr exposes one Torznab URL per configured indexer, conventionally `http://HOST/INDEXER_ID/api`. Register each desired endpoint separately. Filmstream does not use Prowlarr's proprietary aggregate-search API.

The config file is written with mode `0600` because it contains API keys. The CLI reloads a running local backend after an indexer is added or removed.

## Smart streaming, cleanup, and ratio behavior

Filmstream only requests the pieces needed by MPV's current HTTP Range request, a 32 MiB read-ahead window, and a small tail window used to find container metadata. Native HLS prepares `hls_startup_buffer_seconds` of media before opening the player, then packages at approximately playback speed so it avoids racing through an entire movie in the background. Closing either player closes its readers and removes temporary HLS segments.

MPV waits for a two-second initial cache before playback to avoid startup jitter. Native Windows MPV uses its D3D11 hardware-decoding and GPU-rendering path. Linux MPV on WSL uses `gpu-next` with `nvdec-copy` when supported and retains `wlshm` as a compatibility fallback; other environments keep MPV's portable automatic output and software-decoding fallback. Apple clients request streaming-optimized H.264/H.265 releases, avoid known-incompatible Dolby Vision releases, copy video without quality loss, and transcode only audio for AVPlayer compatibility.

Every verified piece remains available for upload while the session is retained. After playback becomes idle, Filmstream manages the lifecycle automatically:

1. Keep seeding the downloaded pieces toward `seed_ratio_target`.
2. Retire the torrent after the target is met and the two-minute idle grace expires.
3. Retire it after `seed_max_hours` even when peer demand cannot reach the target.
4. Keep at most `max_seed_sessions` retained torrents and retire the oldest inactive ones first.
5. Retire the oldest inactive sessions early if completed pieces exceed `cache_limit_gib`.
6. Clear managed torrent data on server startup and shutdown, so crashes or restarts do not leave an unmanaged cache indefinitely.

Active playback is never evicted, even if it temporarily exceeds the cache limit. A full watch may naturally download the full selected video, but its data is still retired by the same policy.

Inspect an active session with:

```bash
filmstream status PLAYBACK_ID
```

A 1.0 ratio means one uploaded byte per downloaded byte. This is necessarily best effort: Filmstream cannot upload bytes unless another peer requests them, and the hard time/space safety limits take precedence over waiting forever. Filmstream does not bypass or guarantee compliance with tracker-specific minimum-time, completion, or hit-and-run rules.

## API

- `GET /v1/health`
- `POST /v1/indexers/reload`
- `POST /v1/resolver/reload`
- `POST /v1/resolve`
- `GET /v1/catalog/search?query=...`
- `GET /v1/watch-history?continue=true`
- `PUT /v1/watch-history`
- `POST /v1/playbacks`
- `GET /v1/playbacks/{id}`
- `GET|HEAD /v1/playbacks/{id}/stream`
- `POST|DELETE /v1/playbacks/{id}/hls`
- `GET /v1/playbacks/{id}/hls/{asset}`

## Development

```bash
make test
make build
make apple-test
make tvos-build
```

The end-to-end torrent test builds a local `.torrent`, loads already-authorized test bytes through the torrent engine, and verifies HTTP byte-range seeking without contacting a public swarm.

## Next steps

- Discovery rows for popular, classic, and genre-based movies
- Rich metadata and artwork in the terminal UI
- TMDB validation and ID-based Torznab searches for resolved movies
- iOS and macOS app targets sharing `FilmstreamCore`
- Multiple-file selection in the CLI
- Docker image and remote authentication
