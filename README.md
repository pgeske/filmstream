# filmstream

`filmstream` is a local Go service that streams authorized torrent media to MPV over HTTP. It can search pluggable indexers, rank releases deterministically, or bypass discovery and play a supplied magnet URI or `.torrent` file.

Use it only with media you are authorized to download and share.

## Current MVP

- Local backend bound to `127.0.0.1:8943`
- One-command CLI that starts the backend when needed and launches MPV
- A small, trusted open-movie catalog plus an Internet Archive reference indexer
- External HTTP indexer contract
- Release ranking by title, year, resolution, language, codec, size, seeders, and leechers
- Direct magnet and `.torrent` playback
- HTTP Range streaming with seeking and 32 MiB read-ahead
- Head-on-demand and background tail prefetch for container metadata
- Background completion and seeding with a visible 1.0 ratio target
- Largest-video selection for multi-file torrents

## Install

Go 1.24 or newer and MPV are required. On Ubuntu:

```bash
sudo apt install mpv
make install
```

This installs `filmstream` to `~/.local/bin` by default.

## Use

The normal command searches configured indexers, selects a release, starts the local server if necessary, and opens MPV:

```bash
filmstream --year 2010 Sintel
filmstream "Night of the Living Dead"
```

The title may be passed as separate shell arguments. Years are only interpreted when supplied with `--year`, so a title such as `2001 A Space Odyssey` remains intact.

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
filmstream --print-url Sintel      # prepare a stream without opening a player
filmstream --player ffplay Sintel  # temporary fallback if MPV is unavailable
filmstream status PLAYBACK_ID
```

An automatically started server logs to `~/.cache/filmstream/server.log`.

## Configuration

The optional configuration file is `~/.config/filmstream/config.json`:

```json
{
  "listen": "127.0.0.1:8943",
  "data_dir": "~/.cache/filmstream",
  "max_candidate_gib": 60,
  "readahead_mib": 32,
  "metadata_timeout_seconds": 120,
  "seed_ratio_target": 1.0,
  "preferred_resolution": "1080p",
  "preferred_languages": ["en", "english"],
  "player": "mpv",
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

Downloaded data is stored beneath `<data_dir>/torrents`. The MVP does not delete cached torrents automatically.

## External indexers

External indexers are small HTTP services. Register one with:

```json
{
  "name": "my-authorized-catalog",
  "type": "http",
  "endpoint": "http://127.0.0.1:9000/search",
  "headers": {
    "Authorization": "Bearer example"
  }
}
```

Filmstream sends:

```json
{
  "query": "Sintel",
  "year": 2010,
  "preferences": {
    "resolution": "1080p",
    "codecs": ["h264", "h265"],
    "languages": ["en"],
    "max_size_bytes": 64424509440
  }
}
```

The indexer returns normalized candidates:

```json
{
  "candidates": [
    {
      "id": "release-id",
      "name": "Sintel.2010.1080p.x265",
      "year": 2010,
      "size_bytes": 2147483648,
      "seeders": 25,
      "leechers": 4,
      "resolution": "1080p",
      "codec": "h265",
      "languages": ["en"],
      "release_group": "example",
      "magnet_uri": "magnet:?xt=urn:btih:..."
    }
  ]
}
```

A candidate must contain either `magnet_uri` or `torrent_url`. Seeders favor quick startup; active leechers receive a smaller bonus because they may improve upload opportunities. Codec, resolution, and language fields can be omitted when unknown. Filmstream infers common resolution and codec tags from release names.

Many torrent indexers expose Torznab, usually through software such as Prowlarr or Jackett, but tracker-native APIs are not universal. A future Torznab adapter can translate that standard into this contract. Site-specific authentication and tracker rules remain the adapter operator's responsibility.

## Streaming and ratio behavior

The selected video starts a responsive torrent reader. MPV's HTTP Range requests move the read head, and the torrent engine reprioritizes pieces around the new offset. The rest of the selected video downloads in the background.

The torrent client uploads while running and reports:

```bash
filmstream status PLAYBACK_ID
```

A 1.0 target means one uploaded byte per downloaded byte. It is a target, not a guarantee: the swarm must contain peers that need pieces from this client. Filmstream does not bypass private-tracker accounting or requirements. The current process continues seeding after the player exits, but sessions are not yet restored automatically after a backend restart.

## API

- `GET /v1/health`
- `POST /v1/playbacks`
- `GET /v1/playbacks/{id}`
- `GET|HEAD /v1/playbacks/{id}/stream`

## Development

```bash
make test
make build
```

The end-to-end torrent test builds a local `.torrent`, loads already-authorized test bytes through the torrent engine, and verifies HTTP byte-range seeking without contacting a public swarm.

## Next steps

- Persist and restore torrents for long-lived seeding
- Cache quota enforcement after ratio targets are met
- Generic Torznab adapter
- Optional TMDB title normalization and disambiguation
- Multiple-file selection in the CLI
- Docker image and remote authentication
