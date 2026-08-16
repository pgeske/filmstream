# TeaStream for Apple platforms

TeaStream is Filmstream's native SwiftUI experience for iPhone, Apple TV, and Mac. The iPhone target provides touch-first home and search tabs, compact movie details, and custom playback controls. Apple TV retains its existing bundle identifier and remote-focused interface, while Mac uses a desktop-native sidebar and pointer-friendly layouts. All three apps reuse `FilmstreamCore` for API models, networking, metadata, HLS playback, subtitles, and watch progress; none embeds VLC or retains server credentials.

## Generate and build

Install XcodeGen once, generate the project, and build any target:

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

xcodebuild -project FilmstreamApple.xcodeproj \
  -scheme FilmstreamMac \
  -destination 'platform=macOS,arch=arm64' \
  build
```

The corresponding repository shortcuts are `make ios-build`, `make tvos-build`, and `make macos-build`.

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

The home screens pair Continue Watching with TMDB-powered Popular Now and Top Rated discovery rails. On tvOS, focused titles expand into cinematic landscape cards, move to the leading shelf position, and reveal their genre, year, content rating, and synopsis; iPhone and Mac use the same landscape-first metadata. Discovery requires an existing digital, physical, or TV release, so upcoming and theater-only titles stay off the shelf. Search starts automatically after two title characters with a short debounce; selecting any movie opens its details and playback actions. All clients use:

- `GET /v1/catalog/search` for movie metadata and artwork;
- `GET /v1/catalog/discover` for home-screen discovery sections;
- `GET /v1/catalog/ratings` for optional IMDb, Rotten Tomatoes, and content ratings;
- `POST /v1/playbacks` for streaming-optimized Usenet-or-torrent selection;
- `POST /v1/playbacks/{id}/hls` for incremental AVPlayer-compatible HLS;
- `DELETE /v1/playbacks/{id}/hls` to stop packaging and remove temporary segments;
- `GET /v1/watch-history?continue=true` for the home screen;
- `PUT /v1/watch-history` to sync playback progress; and
- `DELETE /v1/watch-history/{id}` to clear a saved resume point.

The HLS backend requires FFmpeg and FFprobe. It copies compatible H.264/H.265 video without re-encoding, converts the first audio track to AAC, and paces packaging close to playback speed. It prepares an initial media buffer before opening the player and preserves audio pre-roll when seeking so copied video and transcoded audio stay synchronized. Known Dolby Vision releases are skipped initially in favor of compatible SDR or HDR10 alternatives. Streaming selection strongly prefers healthy Usenet releases, then automatically falls back to cached or newly ranked torrents. Torrent fallback prioritizes popular releases regardless of size, penalizes remuxes, and checks candidates progressively so the first strong live swarm can start without unnecessary delay. The server privately caches selected torrent metadata for Continue Watching and falls back to a fresh search if that cached swarm becomes unavailable.

During tvOS playback, Center or Play/Pause toggles playback and left/right seeks 30 seconds; iPhone and Mac provide equivalent native controls and timeline scrubbing. All clients expose embedded text subtitle tracks and remember the selected language across movies and seeks. Apple playback requests prefer a live release with a usable text track, falling back to the best available release when none is available. Filmstream converts only the selected SRT, ASS/SSA, WebVTT, or other text track into a growing WebVTT sidecar in a separate paced process, so subtitle-heavy releases do not delay video startup. Resume packaging aligns subtitles to the actual preceding video keyframe. Image-based PGS/VobSub tracks require OCR and are not offered.

The timeline always represents the full movie rather than the currently generated HLS window. Seeking outside that window asks the server to resume HLS packaging at the target timestamp, which can briefly buffer while the new window starts.

Configure the server's optional `metadata` provider with a TMDB API read token to enable posters, backdrops, genres, release details, and descriptions. Set the server-only `OMDB_API_KEY` environment variable to add IMDb, Rotten Tomatoes, and content ratings. TeaStream requests external ratings as cards become visible or focused, and the server resolves TMDB IDs to canonical IMDb IDs before querying OMDb. Neither credential is included in the app.

A movie with saved progress presents explicit Resume, Play from Beginning, and Remove from Continue Watching actions on its detail screen. Removing a title clears its saved resume point; playing it again starts fresh and resumes normal progress tracking.

Movie data and images are provided by TMDB. This product uses the TMDB API but is not endorsed or certified by TMDB. External rating data is provided by OMDb.
