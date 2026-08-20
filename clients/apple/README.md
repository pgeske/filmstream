# TeaStream for Apple platforms

TeaStream is Filmstream's native SwiftUI experience for iPhone, Apple TV, and Mac. The iPhone target provides touch-first home and search tabs, compact movie and show details, season browsing, and custom playback controls. Apple TV retains its existing bundle identifier and remote-focused interface, while Mac uses a desktop-native sidebar and pointer-friendly layouts. All three apps reuse `FilmstreamCore` for API models, networking, metadata, HLS playback, subtitles, and watch progress; none embeds VLC or retains server credentials.

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

The home screens pair Continue Watching with mixed movie/show TMDB-powered Popular Now and Top Rated rails. Cards identify the media type, genre, year, and season count where applicable. On tvOS, focused titles expand into cinematic landscape cards and move to the leading shelf position; iPhone and Mac use touch- and pointer-appropriate versions of the same metadata. Movie discovery requires an existing digital, physical, or TV release, so upcoming and theater-only movies stay off the shelf. Search starts automatically after two title characters with a short debounce; selecting any result opens its adaptive details and playback actions. All clients use:

- `GET /v1/catalog/search` for mixed movie/show metadata and artwork;
- `GET /v1/catalog/discover` for mixed home-screen discovery sections;
- `GET /v1/catalog/shows/{id}` for show and season summaries;
- `GET /v1/catalog/shows/{id}/seasons/{season}` for episode artwork and metadata;
- `GET /v1/catalog/ratings` for optional IMDb, Rotten Tomatoes, and content ratings;
- `POST /v1/playbacks` for streaming-optimized Usenet-or-torrent selection;
- `POST /v1/playbacks/prewarm` to begin release selection and buffering from a detail screen;
- `POST /v1/playbacks/{id}/hls` for incremental AVPlayer-compatible HLS;
- `DELETE /v1/playbacks/{id}/hls` to stop packaging and remove temporary segments;
- `GET /v1/watch-history?continue=true` for movie and next-episode home progress;
- `GET /v1/watch-history` for first-unwatched episode selection;
- `PUT /v1/watch-history` to sync movie or episode progress; and
- `DELETE /v1/watch-history/{id}` to clear a movie or series resume point.

The HLS backend requires FFmpeg and FFprobe. It copies compatible H.264/H.265 video without re-encoding unless a bitmap subtitle is selected, converts the title's original-language audio track to AAC with English fallback, and paces packaging close to playback speed. PGS and other supported bitmap subtitles are burned into a hardware-accelerated H.264 rendition because AVPlayer HLS cannot carry them directly. It prepares an initial media buffer before opening the player, marks the beginning of each event playlist as the preferred start point, and preserves audio pre-roll when seeking so copied video and transcoded audio stay synchronized. TV browsing prefetches and caches ranked indexer results without mounting a torrent or downloading episode payload. An explicit play request validates the live swarm and subtitle tracks before caching the selected season release. While an episode is actually playing, the backend keeps only the next episode's first 30 seconds ready; it no longer preloads every Continue Watching show or the current episode after exit. This preserves fast transitions without creating unrelated torrent seeding obligations. Play requests join an in-progress warmup rather than restarting it. Before serving a parked buffer, the backend verifies that its source connection is producing new segments and rebuilds stale packaging automatically. Durable torrent sessions restore asynchronously so seeding cannot block the health endpoint after a deployment. The tvOS player also reconnects its current stream after system sleep and recovers prolonged buffering without requiring the viewer to leave playback. Known Dolby Vision releases are skipped initially in favor of compatible SDR or HDR10 alternatives. Streaming selection strongly prefers healthy Usenet releases, then automatically falls back to cached or newly ranked torrents. Torrent fallback rejects AI upscales and 2160p remuxes, prioritizes the requested quality and then swarm popularity, and checks candidates progressively so stale seeder counts cannot strand playback. The server privately caches successful NZBs and season-scoped torrent metadata, reuses the known-good release without searching or probing it again for every episode, and falls back to a fresh search when a cached source becomes unavailable.

During tvOS playback, Center or Play/Pause toggles playback, left/right seeks 30 seconds, and Up opens subtitles. Episodes automatically advance at the end on Apple TV, iPhone, and Mac, with no extra Next Episode control in the playback chrome. TeaStream registers the active tvOS player with the system media command center so AirPods and other play/pause controls operate the show instead of resuming another Apple TV app. iPhone and Mac provide equivalent native controls and timeline scrubbing. Mac playback also includes a Picture in Picture button once the video is ready. All clients expose embedded text and bitmap subtitle tracks and remember the selected language across movies and seeks. Apple playback requests prefer a live release with a supported subtitle track, falling back to the best available release when none is available. Filmstream converts a selected SRT, ASS/SSA, WebVTT, or other text track into a growing WebVTT sidecar in a separate paced process. PGS, VobSub, and other bitmap tracks retain their authored typography and placement by being composited into the video with NVENC; switching one on or off rebuilds the HLS rendition at the current playback position. Resume packaging aligns both paths to the actual preceding video keyframe.

The timeline always represents the full movie or episode rather than the currently generated HLS window. Seeking outside that window asks the server to resume HLS packaging at the target timestamp, which can briefly buffer while the new window starts.

Configure the server's optional `metadata` provider with a TMDB API read token to enable posters, backdrops, genres, release details, and descriptions. Set the server-only `OMDB_API_KEY` environment variable to add IMDb, Rotten Tomatoes, and content ratings. TeaStream requests external ratings as cards become visible or focused, and the server resolves TMDB IDs to canonical IMDb IDs before querying OMDb. Neither credential is included in the app.

Long cold starts distinguish Finding a Release from Buffering Video instead of showing one indefinite preparation state. Detail pages show only IMDb and Rotten Tomatoes scores; TMDB votes remain catalog-ranking metadata rather than user-facing ratings. Top Rated requires substantially more votes so established movies and shows are favored over obscure new entries.

A movie with saved progress presents explicit Resume, Play from Beginning, and Remove from Continue Watching actions. A show resumes its active episode or selects the first unwatched episode, labels the primary action with `Sx Ex`, and adds Episodes & More. The episode browser uses a season sidebar on tvOS and platform-native touch or pointer layouts on iPhone and Mac. Removing a show clears progress for the series; playing it again starts fresh and resumes normal episode tracking.

Media data and images are provided by TMDB. This product uses the TMDB API but is not endorsed or certified by TMDB. External rating data is provided by OMDb.
