# TeaStream for iPhone and Apple TV

TeaStream is Filmstream's native SwiftUI experience for iPhone and Apple TV. The iPhone target provides touch-first home and search tabs, compact movie details, ratings, Continue Watching management, and custom playback controls. The Apple TV target retains its existing bundle identifier and remote-focused interface. Both reuse `FilmstreamCore` for API models, networking, metadata, HLS playback, subtitles, and watch progress; neither embeds VLC or retains server credentials.

## Generate and build

Install XcodeGen once, generate the project, and build either target:

```bash
brew install xcodegen
cd clients/apple
xcodegen generate

xcodebuild -project FilmstreamApple.xcodeproj \
  -scheme FilmstreamIOS \
  -destination 'platform=iOS Simulator,name=iPhone 17 Pro' \
  build

xcodebuild -project FilmstreamApple.xcodeproj \
  -scheme FilmstreamTV \
  -destination 'platform=tvOS Simulator,name=Apple TV 4K (3rd generation)' \
  build
```

The corresponding repository shortcuts are `make ios-build` and `make tvos-build`.

## Install on a physical iPhone

Connect and trust the iPhone, sign in under **Xcode > Settings > Apple Accounts**, and enable Developer Mode if iOS requests it. From the repository root, build, sign, install, and launch TeaStream with:

```bash
make ios-install
```

The command discovers one paired physical iPhone and one Apple Development team without persisting either identifier. When discovery or signing is ambiguous, pass invocation-only values:

```bash
make ios-install \
  IOS_DEVICE_ID=<iphone-udid> \
  IOS_DEVELOPMENT_TEAM=<team-id>
```

Set `IOS_DERIVED_DATA_PATH` to override `clients/apple/.derivedData/ios-device`.

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

TeaStream defaults to `http://filmstream.home.alyoshukai.com`; each target contains a temporary App Transport Security exception for that private hostname. Replace it with HTTPS before distribution.

## Server capabilities

The home screens pair Continue Watching with TMDB-powered Popular Now and Top Rated discovery rails. Discovery requires an existing digital, physical, or TV release, so upcoming and theater-only titles stay off the shelf. Search starts automatically after two title characters with a short debounce; selecting any movie opens its details and playback action. Both clients use:

- `GET /v1/catalog/search` for movie metadata and artwork;
- `GET /v1/catalog/discover` for home-screen discovery sections;
- `POST /v1/playbacks` for streaming-optimized torrent selection;
- `POST /v1/playbacks/{id}/hls` for incremental AVPlayer-compatible HLS;
- `DELETE /v1/playbacks/{id}/hls` to stop packaging and remove temporary segments;
- `GET /v1/watch-history?continue=true` for the home screen; and
- `PUT /v1/watch-history` to sync playback progress.

The HLS backend requires FFmpeg and FFprobe. It copies compatible H.264/H.265 video without re-encoding, converts the first audio track to AAC, and paces packaging close to playback speed. It prepares an initial media buffer before opening the player and preserves audio pre-roll when seeking so copied video and transcoded audio stay synchronized. Known Dolby Vision releases are skipped initially in favor of compatible SDR or HDR10 alternatives. Streaming selection prioritizes popular releases regardless of size, penalizes remuxes, and checks ranked candidates progressively so the first strong live swarm can start without waiting for unnecessary alternatives. The server privately caches the selected torrent metadata for Continue Watching and falls back to a fresh search if the cached swarm becomes unavailable.

During tvOS playback, Center or Play/Pause toggles playback and left/right seeks 30 seconds; iPhone provides equivalent touch controls and timeline scrubbing. Both clients expose embedded text subtitle tracks and remember the selected language across movies and seeks. Filmstream converts only the selected SRT, ASS/SSA, WebVTT, or other text track into a growing WebVTT sidecar in a separate paced process, so subtitle-heavy releases do not delay video startup. Resume packaging aligns subtitles to the actual preceding video keyframe. Image-based PGS/VobSub tracks require OCR and are not offered.

The timeline always represents the full movie rather than the currently generated HLS window. Seeking outside that window asks the server to resume HLS packaging at the target timestamp, which can briefly buffer while the new window starts.

Configure the server's optional `metadata` provider with a TMDB API read token to enable posters, backdrops, release details, and descriptions. Filmstream keeps the token on the server; it is never included in the app.

Movie data and images are provided by TMDB. This product uses the TMDB API but is not endorsed or certified by TMDB.
