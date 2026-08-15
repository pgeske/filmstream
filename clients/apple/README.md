# TeaStream for Apple TV

TeaStream is Filmstream's native tvOS experience, with a cozy matcha-inspired interface while retaining the existing Filmstream backend, bundle identifier, and repository structure. The shared `FilmstreamCore` package supports tvOS, iOS, and macOS so future app targets can reuse API models, networking, metadata, and watch progress. Playback uses AVPlayer with server-generated fragmented HLS; the app does not embed VLC or retain indexer, TMDB, or resolver credentials.

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

The corresponding repository shortcut is `make tvos-build`.

## Install on a physical Apple TV

Before the first installation, pair the Apple TV in Xcode and sign in under **Xcode > Settings > Apple Accounts**. The Apple TV and Mac must be able to reach each other on the local network. If TeaStream uses a tailnet-only backend, authenticate the Apple TV in Tailscale as well.

From the repository root, build, sign, install, and launch TeaStream with:

```bash
make tvos-install
```

The command regenerates the Xcode project, discovers a paired physical Apple TV, detects a single Apple Development team from the login keychain, lets Xcode update automatic provisioning, and installs the signed app with `devicectl`. Use explicit values when more than one device or development team is available:

```bash
make tvos-install \
  TVOS_DEVICE_ID=<apple-tv-udid> \
  TVOS_DEVELOPMENT_TEAM=<team-id>
```

Set `TVOS_DERIVED_DATA_PATH` to override the default incremental build directory at `clients/apple/.derivedData/device`. Personal Team provisioning expires periodically, so rerun the command when tvOS reports that the app is no longer available.

TeaStream defaults to `http://filmstream.home.alyoshukai.com`. `FilmstreamTV/Info.plist` contains a temporary App Transport Security exception for that private hostname. Replace it with HTTPS before distributing the app.

## Server capabilities

The home screen pairs Continue Watching with TMDB-powered Popular Now and Top Rated discovery rails. Discovery requires an existing digital, physical, or TV release, so upcoming and theater-only titles stay off the shelf. Search starts automatically after two title characters with a short debounce; selecting any movie opens its details and playback action. The tvOS client uses:

- `GET /v1/catalog/search` for movie metadata and artwork;
- `GET /v1/catalog/discover` for home-screen discovery sections;
- `POST /v1/playbacks` for streaming-optimized torrent selection;
- `POST /v1/playbacks/{id}/hls` for incremental AVPlayer-compatible HLS;
- `DELETE /v1/playbacks/{id}/hls` to stop packaging and remove temporary segments;
- `GET /v1/watch-history?continue=true` for the home screen; and
- `PUT /v1/watch-history` to sync playback progress.

The HLS backend requires FFmpeg and FFprobe. It copies compatible H.264/H.265 video without re-encoding, converts the first audio track to AAC, and paces packaging close to playback speed. It prepares an initial media buffer before opening the player and preserves audio pre-roll when seeking so copied video and transcoded audio stay synchronized. Known Dolby Vision releases are skipped initially in favor of compatible SDR or HDR10 alternatives. Streaming selection prioritizes popular releases regardless of size, penalizes remuxes, and checks ranked candidates progressively so the first strong live swarm can start without waiting for unnecessary alternatives. The server privately caches the selected torrent metadata for Continue Watching and falls back to a fresh search if the cached swarm becomes unavailable.

During playback, Center or Play/Pause toggles playback and left/right seeks 30 seconds. Up or down opens the subtitle panel, where Off and every embedded text subtitle track are available; the selected language is remembered across movies and seeks. Filmstream converts only the selected SRT, ASS/SSA, WebVTT, or other text track into a growing WebVTT sidecar in a separate paced process, so subtitle-heavy releases do not delay video startup. Resume packaging aligns subtitles to the actual preceding video keyframe. Image-based PGS/VobSub tracks require OCR and are not offered.

The timeline always represents the full movie rather than the currently generated HLS window. Seeking outside that window asks the server to resume HLS packaging at the target timestamp, which can briefly buffer while the new window starts.

Configure the server's optional `metadata` provider with a TMDB API read token to enable posters, backdrops, release details, and descriptions. Filmstream keeps the token on the server; it is never included in the app.

Movie data and images are provided by TMDB. This product uses the TMDB API but is not endorsed or certified by TMDB.
