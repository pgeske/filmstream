# Filmstream for Apple Platforms

Filmstream's native Apple client starts with tvOS. The shared `FilmstreamCore` package supports tvOS, iOS, and macOS so future app targets can reuse API models, networking, metadata, and watch progress. Playback uses AVPlayer with server-generated fragmented HLS; the app does not embed VLC or retain indexer, TMDB, or resolver credentials.

## Generate and build

Install XcodeGen once, generate the project, and build the tvOS target:

```bash
brew install xcodegen
cd clients/apple
xcodegen generate
xcodebuild -project FilmstreamApple.xcodeproj \
  -scheme FilmstreamTV \
  -destination 'platform=tvOS Simulator,name=Apple TV 4K (3rd generation)' \
  build
```

The app defaults to `http://filmstream.home.alyoshukai.com`. `FilmstreamTV/Info.plist` contains a temporary App Transport Security exception for that private hostname. Replace it with HTTPS before distributing the app.

## Server capabilities

Search starts automatically after two title characters with a short debounce; selecting a TMDB result opens its movie details and playback action. The tvOS client uses:

- `GET /v1/catalog/search` for movie metadata and artwork;
- `POST /v1/playbacks` for streaming-optimized torrent selection;
- `POST /v1/playbacks/{id}/hls` for incremental AVPlayer-compatible HLS;
- `DELETE /v1/playbacks/{id}/hls` to stop packaging and remove temporary segments;
- `GET /v1/watch-history?continue=true` for the home screen; and
- `PUT /v1/watch-history` to sync playback progress.

The HLS backend requires FFmpeg and FFprobe. It copies compatible H.264/H.265 video without re-encoding, converts the first audio track to AAC, and paces packaging close to playback speed. It prepares an initial media buffer before opening the player and preserves audio pre-roll when seeking so copied video and transcoded audio stay synchronized. Known Dolby Vision releases are skipped initially in favor of compatible SDR or HDR10 alternatives.

During playback, Center or Play/Pause toggles playback and left/right seeks 30 seconds. Up or down opens the subtitle panel, where Off and every embedded text subtitle track are available; the selected language is remembered across movies and seeks. Filmstream converts SRT, ASS/SSA, WebVTT, and other text tracks into growing WebVTT sidecars while the same paced FFmpeg process packages video. Image-based PGS/VobSub tracks require OCR and are not offered.

The timeline always represents the full movie rather than the currently generated HLS window. Seeking outside that window asks the server to resume HLS packaging at the target timestamp, which can briefly buffer while the new window starts.

Configure the server's optional `metadata` provider with a TMDB API read token to enable posters, backdrops, release details, and descriptions. Filmstream keeps the token on the server; it is never included in the app.

Movie data and images are provided by TMDB. This product uses the TMDB API but is not endorsed or certified by TMDB.
