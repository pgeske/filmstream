import Foundation

public enum MediaType: String, Codable, Hashable, Sendable {
    case movie
    case show

    public init(from decoder: Decoder) throws {
        let value = try decoder.singleValueContainer().decode(String.self)
        self = MediaType(rawValue: value) ?? .movie
    }

    public func encode(to encoder: Encoder) throws {
        var container = encoder.singleValueContainer()
        try container.encode(rawValue)
    }
}

public struct Movie: Codable, Hashable, Identifiable, Sendable {
    public let id: String
    public let mediaType: MediaType?
    public let title: String
    public let originalTitle: String?
    public let originalLanguage: String?
    public let year: Int?
    public let releaseDate: String?
    public let overview: String?
    public let posterURL: URL?
    public let backdropURL: URL?
    public let voteAverage: Double?
    public let genres: [String]?
    public let numberOfSeasons: Int?
    public let seriesID: String?
    public let seriesTitle: String?
    public let seasonNumber: Int?
    public let episodeNumber: Int?
    public let episodeTitle: String?

    public init(
        id: String,
        mediaType: MediaType? = nil,
        title: String,
        originalTitle: String? = nil,
        originalLanguage: String? = nil,
        year: Int? = nil,
        releaseDate: String? = nil,
        overview: String? = nil,
        posterURL: URL? = nil,
        backdropURL: URL? = nil,
        voteAverage: Double? = nil,
        genres: [String]? = nil,
        numberOfSeasons: Int? = nil,
        seriesID: String? = nil,
        seriesTitle: String? = nil,
        seasonNumber: Int? = nil,
        episodeNumber: Int? = nil,
        episodeTitle: String? = nil
    ) {
        self.id = id
        self.mediaType = mediaType
        self.title = title
        self.originalTitle = originalTitle
        self.originalLanguage = originalLanguage
        self.year = year
        self.releaseDate = releaseDate
        self.overview = overview
        self.posterURL = posterURL
        self.backdropURL = backdropURL
        self.voteAverage = voteAverage
        self.genres = genres
        self.numberOfSeasons = numberOfSeasons
        self.seriesID = seriesID
        self.seriesTitle = seriesTitle
        self.seasonNumber = seasonNumber
        self.episodeNumber = episodeNumber
        self.episodeTitle = episodeTitle
    }

    public var isShow: Bool { mediaType == .show }

    public var episodeLabel: String? {
        guard let seasonNumber, let episodeNumber else { return nil }
        return "S\(seasonNumber) E\(episodeNumber)"
    }

    public var mediaTypeLabel: String { isShow ? "Show" : "Movie" }

    public var seasonCountLabel: String? {
        guard isShow, let numberOfSeasons, numberOfSeasons > 0 else { return nil }
        return numberOfSeasons == 1 ? "1 Season" : "\(numberOfSeasons) Seasons"
    }

    public var catalogMetadata: String {
        var values = [mediaTypeLabel]
        if let primaryGenre { values.append(primaryGenre) }
        if let year { values.append(String(year)) }
        if let seasonCountLabel { values.append(seasonCountLabel) }
        return values.joined(separator: " • ")
    }

    public var primaryGenre: String? {
        genres?.first { !$0.isEmpty }
    }

    public var genreSummary: String? {
        guard let genres else { return nil }
        let values = genres.filter { !$0.isEmpty }.prefix(2)
        return values.isEmpty ? nil : values.joined(separator: " • ")
    }

    private enum CodingKeys: String, CodingKey {
        case id, title, year, overview, genres
        case mediaType = "media_type"
        case originalTitle = "original_title"
        case originalLanguage = "original_language"
        case releaseDate = "release_date"
        case posterURL = "poster_url"
        case backdropURL = "backdrop_url"
        case voteAverage = "vote_average"
        case numberOfSeasons = "number_of_seasons"
        case seriesID = "series_id"
        case seriesTitle = "series_title"
        case seasonNumber = "season_number"
        case episodeNumber = "episode_number"
        case episodeTitle = "episode_title"
    }
}

public struct MovieRatings: Codable, Hashable, Sendable {
    public let imdb: Double?
    public let rottenTomatoes: Int?
    public let contentRating: String?

    public init(
        imdb: Double? = nil,
        rottenTomatoes: Int? = nil,
        contentRating: String? = nil
    ) {
        self.imdb = imdb
        self.rottenTomatoes = rottenTomatoes
        self.contentRating = contentRating
    }

    public var isEmpty: Bool {
        imdb == nil && rottenTomatoes == nil && contentRating == nil
    }

    private enum CodingKeys: String, CodingKey {
        case imdb
        case rottenTomatoes = "rotten_tomatoes"
        case contentRating = "content_rating"
    }
}

public struct Recommendations: Codable, Hashable, Sendable {
    public let generatedAt: Date?
    public let prompt: String
    public let refreshing: Bool
    public let items: [Movie]

    public init(
        generatedAt: Date? = nil,
        prompt: String,
        refreshing: Bool,
        items: [Movie]
    ) {
        self.generatedAt = generatedAt
        self.prompt = prompt
        self.refreshing = refreshing
        self.items = items
    }

    public init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        prompt = try container.decode(String.self, forKey: .prompt)
        refreshing = try container.decode(Bool.self, forKey: .refreshing)
        items = try container.decode([Movie].self, forKey: .items)

        guard let value = try container.decodeIfPresent(String.self, forKey: .generatedAt) else {
            generatedAt = nil
            return
        }
        let formatter = ISO8601DateFormatter()
        formatter.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        if let date = formatter.date(from: value) {
            generatedAt = date
            return
        }
        formatter.formatOptions = [.withInternetDateTime]
        guard let date = formatter.date(from: value) else {
            throw DecodingError.dataCorruptedError(
                forKey: .generatedAt,
                in: container,
                debugDescription: "Expected an RFC3339 timestamp."
            )
        }
        generatedAt = date
    }

    public func encode(to encoder: Encoder) throws {
        var container = encoder.container(keyedBy: CodingKeys.self)
        if let generatedAt {
            let formatter = ISO8601DateFormatter()
            formatter.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
            try container.encode(formatter.string(from: generatedAt), forKey: .generatedAt)
        }
        try container.encode(prompt, forKey: .prompt)
        try container.encode(refreshing, forKey: .refreshing)
        try container.encode(items, forKey: .items)
    }

    private enum CodingKeys: String, CodingKey {
        case generatedAt = "generated_at"
        case prompt, refreshing, items
    }
}

public enum RecommendationUpdateSource: Sendable {
    case promptSave
    case refresh
}

public extension Recommendations {
    /// Items with missing or unrecognized media types stay visible in the movie shelf,
    /// matching Movie's existing non-show fallback behavior.
    var recommendedShows: [Movie] {
        items.filter { $0.mediaType == .show }
    }

    var recommendedMovies: [Movie] {
        items.filter { $0.mediaType != .show }
    }

    func isOlderGeneration(than other: Recommendations) -> Bool {
        guard let generatedAt, let otherGeneratedAt = other.generatedAt else { return false }
        return generatedAt < otherGeneratedAt
    }

    func merged(
        with current: Recommendations?,
        source: RecommendationUpdateSource
    ) -> Recommendations {
        guard let current else { return self }

        if source == .promptSave {
            let shouldKeepCurrentItems = refreshing || items.isEmpty || isOlderGeneration(than: current)
            return Recommendations(
                generatedAt: latestGeneratedAt(comparedWith: current),
                prompt: prompt,
                refreshing: refreshing,
                items: shouldKeepCurrentItems && !current.items.isEmpty ? current.items : items
            )
        }

        if isOlderGeneration(than: current) {
            return Recommendations(
                generatedAt: current.generatedAt,
                prompt: current.prompt,
                refreshing: current.refreshing && refreshing,
                items: current.items
            )
        }

        if refreshing, !current.items.isEmpty {
            return Recommendations(
                generatedAt: latestGeneratedAt(comparedWith: current),
                prompt: items.isEmpty ? current.prompt : prompt,
                refreshing: true,
                items: current.items
            )
        }

        if items.isEmpty {
            return Recommendations(
                generatedAt: current.generatedAt,
                prompt: current.prompt,
                refreshing: refreshing,
                items: current.items
            )
        }

        return self
    }

    func settingRefreshing(_ refreshing: Bool) -> Recommendations {
        Recommendations(
            generatedAt: generatedAt,
            prompt: prompt,
            refreshing: refreshing,
            items: items
        )
    }

    private func latestGeneratedAt(comparedWith other: Recommendations) -> Date? {
        switch (generatedAt, other.generatedAt) {
        case let (generatedAt?, otherGeneratedAt?):
            max(generatedAt, otherGeneratedAt)
        case let (generatedAt?, nil):
            generatedAt
        case let (nil, otherGeneratedAt?):
            otherGeneratedAt
        case (nil, nil):
            nil
        }
    }
}

public struct RecommendationPromptUpdate: Codable, Hashable, Sendable {
    public let prompt: String

    public init(prompt: String) {
        self.prompt = prompt
    }
}

public struct DiscoverySection: Codable, Hashable, Identifiable, Sendable {
    public let id: String
    public let title: String
    public let subtitle: String
    public let items: [Movie]

    public init(id: String, title: String, subtitle: String, items: [Movie]) {
        self.id = id
        self.title = title
        self.subtitle = subtitle
        self.items = items
    }
}

public struct SeasonSummary: Codable, Hashable, Identifiable, Sendable {
    public var id: Int { number }
    public let number: Int
    public let name: String
    public let episodeCount: Int
    public let posterURL: URL?
    public let airDate: String?

    private enum CodingKeys: String, CodingKey {
        case number, name
        case episodeCount = "episode_count"
        case posterURL = "poster_url"
        case airDate = "air_date"
    }
}

public struct SeriesDetails: Codable, Hashable, Sendable {
    public let show: Movie
    public let seasons: [SeasonSummary]
}

public struct Episode: Codable, Hashable, Identifiable, Sendable {
    public let id: String
    public let seriesID: String
    public let seriesTitle: String
    public let seasonNumber: Int
    public let episodeNumber: Int
    public let title: String
    public let overview: String?
    public let airDate: String?
    public let stillURL: URL?
    public let voteAverage: Double?
    public let runtimeMinutes: Int?

    public func playbackMovie(in show: Movie) -> Movie {
        Movie(
            id: id,
            mediaType: .show,
            title: show.title,
            originalTitle: show.originalTitle,
            originalLanguage: show.originalLanguage,
            year: show.year,
            releaseDate: airDate,
            overview: overview,
            posterURL: show.posterURL,
            backdropURL: stillURL ?? show.backdropURL,
            voteAverage: voteAverage,
            genres: show.genres,
            numberOfSeasons: show.numberOfSeasons,
            seriesID: seriesID,
            seriesTitle: seriesTitle,
            seasonNumber: seasonNumber,
            episodeNumber: episodeNumber,
            episodeTitle: title
        )
    }

    public var label: String { "S\(seasonNumber) E\(episodeNumber)" }

    private enum CodingKeys: String, CodingKey {
        case id, title, overview
        case seriesID = "series_id"
        case seriesTitle = "series_title"
        case seasonNumber = "season_number"
        case episodeNumber = "episode_number"
        case airDate = "air_date"
        case stillURL = "still_url"
        case voteAverage = "vote_average"
        case runtimeMinutes = "runtime_minutes"
    }
}

public struct EpisodePlaybackSelection: Hashable, Sendable {
    public let episode: Episode
    public let startSeconds: Double

    public init(episode: Episode, startSeconds: Double) {
        self.episode = episode
        self.startSeconds = startSeconds
    }

    public var isResume: Bool { startSeconds > 0 }
}

public struct ShowSeason: Codable, Hashable, Sendable {
    public let seriesID: String
    public let seriesTitle: String
    public let number: Int
    public let name: String
    public let overview: String?
    public let posterURL: URL?
    public let episodes: [Episode]

    private enum CodingKeys: String, CodingKey {
        case number, name, overview, episodes
        case seriesID = "series_id"
        case seriesTitle = "series_title"
        case posterURL = "poster_url"
    }
}

public struct WatchHistoryEntry: Codable, Hashable, Identifiable, Sendable {
    public let id: String
    public let mediaID: String?
    public let mediaType: MediaType?
    public let title: String
    public let year: Int?
    public let overview: String?
    public let posterURL: URL?
    public let backdropURL: URL?
    public let genres: [String]?
    public let numberOfSeasons: Int?
    public let seriesID: String?
    public let seriesTitle: String?
    public let seasonNumber: Int?
    public let episodeNumber: Int?
    public let episodeTitle: String?
    public let positionSeconds: Double
    public let durationSeconds: Double?
    public let completed: Bool
    public let updatedAt: String

    public var progress: Double {
        guard let durationSeconds, durationSeconds > 0 else { return 0 }
        return max(0, min(1, positionSeconds / durationSeconds))
    }

    public var episodeLabel: String? {
        guard let seasonNumber, let episodeNumber else { return nil }
        return "S\(seasonNumber) E\(episodeNumber)"
    }

    public var movie: Movie {
        let isSeries = seriesID != nil || mediaType == .show
        return Movie(
            id: seriesID ?? mediaID ?? "filmstream-history:\(id)",
            mediaType: isSeries ? .show : mediaType,
            title: seriesTitle ?? title,
            year: year,
            overview: overview,
            posterURL: posterURL,
            backdropURL: backdropURL,
            genres: genres,
            numberOfSeasons: numberOfSeasons,
            seriesID: seriesID,
            seriesTitle: seriesTitle,
            seasonNumber: seasonNumber,
            episodeNumber: episodeNumber,
            episodeTitle: episodeTitle
        )
    }

    public var playbackMovie: Movie {
        let item = movie
        return Movie(
            id: mediaID ?? item.id,
            mediaType: item.mediaType,
            title: item.title,
            originalTitle: item.originalTitle,
            originalLanguage: item.originalLanguage,
            year: item.year,
            releaseDate: item.releaseDate,
            overview: item.overview,
            posterURL: item.posterURL,
            backdropURL: item.backdropURL,
            voteAverage: item.voteAverage,
            genres: item.genres,
            numberOfSeasons: item.numberOfSeasons,
            seriesID: item.seriesID,
            seriesTitle: item.seriesTitle,
            seasonNumber: item.seasonNumber,
            episodeNumber: item.episodeNumber,
            episodeTitle: item.episodeTitle
        )
    }

    private enum CodingKeys: String, CodingKey {
        case id, title, year, overview, genres, completed
        case mediaID = "media_id"
        case mediaType = "media_type"
        case posterURL = "poster_url"
        case backdropURL = "backdrop_url"
        case numberOfSeasons = "number_of_seasons"
        case seriesID = "series_id"
        case seriesTitle = "series_title"
        case seasonNumber = "season_number"
        case episodeNumber = "episode_number"
        case episodeTitle = "episode_title"
        case positionSeconds = "position_seconds"
        case durationSeconds = "duration_seconds"
        case updatedAt = "updated_at"
    }
}

public struct Playback: Codable, Hashable, Identifiable, Sendable {
    public let id: String
    public let name: String
    public let fileName: String
    public let fileSize: Int64
    public let streamURL: URL

    private enum CodingKeys: String, CodingKey {
        case id, name
        case fileName = "file_name"
        case fileSize = "file_size"
        case streamURL = "stream_url"
    }
}

public struct HLSSubtitleTrack: Codable, Hashable, Identifiable, Sendable {
    public var id: Int { index }
    public let index: Int
    public let language: String?
    public let title: String?
    public let isDefault: Bool?
    public let isForced: Bool?
    public let codec: String?
    public let kind: String?

    public var isBitmap: Bool { kind == "bitmap" }

    public static func savedPreference(in tracks: [HLSSubtitleTrack]) -> HLSSubtitleTrack? {
        let defaults = UserDefaults.standard
        if defaults.object(forKey: "filmstream.subtitles.enabled") == nil {
            return tracks.first(where: { $0.isForced == true })
        }
        guard defaults.bool(forKey: "filmstream.subtitles.enabled") else { return nil }
        let language = defaults.string(forKey: "filmstream.subtitles.language")
        let title = defaults.string(forKey: "filmstream.subtitles.title")
        let kind = defaults.string(forKey: "filmstream.subtitles.kind")
        let selected: HLSSubtitleTrack?
        if let kind {
            selected = tracks.first(where: {
                $0.kind == kind && $0.language == language && $0.title == title
            }) ?? tracks.first(where: { $0.kind == kind && $0.language == language })
                ?? tracks.first(where: { $0.language == language && $0.title == title })
                ?? tracks.first(where: { $0.language == language })
        } else {
            selected = tracks.first(where: { $0.language == language && $0.title == title })
                ?? tracks.first(where: { $0.language == language })
            defaults.set(selected?.kind, forKey: "filmstream.subtitles.kind")
        }
        return selected
    }

    public static func savePreference(_ track: HLSSubtitleTrack?) {
        let defaults = UserDefaults.standard
        defaults.set(track != nil, forKey: "filmstream.subtitles.enabled")
        defaults.set(track?.language, forKey: "filmstream.subtitles.language")
        defaults.set(track?.title, forKey: "filmstream.subtitles.title")
        defaults.set(track?.kind, forKey: "filmstream.subtitles.kind")
    }

    private enum CodingKeys: String, CodingKey {
        case index, language, title
        case isDefault = "default"
        case isForced = "forced"
        case codec, kind
    }
}

public struct HLSPlayback: Codable, Hashable, Identifiable, Sendable {
    public var id: String { playbackID }
    public var timeline: HLSPlaybackTimeline {
        HLSPlaybackTimeline(
            requestedSeconds: requestedStartSeconds ?? startSeconds,
            originSeconds: startSeconds,
            playerTimeOffsetSeconds: playerTimeOffsetSeconds ?? 0
        )
    }

    public let playbackID: String
    public let playlistURL: URL
    public let requestedStartSeconds: Double?
    public let startSeconds: Double
    public let playerTimeOffsetSeconds: Double?
    public let durationSeconds: Double?
    public let videoCodec: String
    public let subtitles: [HLSSubtitleTrack]?
    public let burnedSubtitleIndex: Int?

    private enum CodingKeys: String, CodingKey {
        case playbackID = "playback_id"
        case playlistURL = "playlist_url"
        case requestedStartSeconds = "requested_start_seconds"
        case startSeconds = "start_seconds"
        case playerTimeOffsetSeconds = "player_time_offset_seconds"
        case durationSeconds = "duration_seconds"
        case videoCodec = "video_codec"
        case subtitles
        case burnedSubtitleIndex = "burned_subtitle_index"
    }
}

public struct HLSPlaybackTimeline: Hashable, Sendable {
    public let requestedSeconds: Double
    public let originSeconds: Double
    public let playerTimeOffsetSeconds: Double

    // AVPlayer reports the first playlist presentation timestamp as time zero.
    public var mediaOriginSeconds: Double {
        originSeconds + playerTimeOffsetSeconds
    }

    public init(
        requestedSeconds: Double,
        originSeconds: Double,
        playerTimeOffsetSeconds: Double = 0
    ) {
        self.requestedSeconds = max(0, requestedSeconds)
        self.originSeconds = max(0, originSeconds)
        self.playerTimeOffsetSeconds = max(0, playerTimeOffsetSeconds)
    }

    public func mediaSeconds(forPlayerSeconds playerSeconds: Double) -> Double {
        max(0, mediaOriginSeconds + max(0, playerSeconds))
    }

    public func playerSeconds(forMediaSeconds mediaSeconds: Double) -> Double {
        mediaSeconds - mediaOriginSeconds
    }
}

public enum PlaybackPreparationStage: Hashable, Sendable {
    case findingRelease
    case bufferingVideo
}

public struct PreparedPlayback: Hashable, Identifiable, Sendable {
    public var id: String { playback.id }
    public let playback: Playback
    public let hls: HLSPlayback

    public init(playback: Playback, hls: HLSPlayback) {
        self.playback = playback
        self.hls = hls
    }
}

public struct WatchProgress: Encodable, Sendable {
    public let mediaID: String
    public let mediaType: MediaType?
    public let title: String
    public let year: Int?
    public let overview: String?
    public let posterURL: URL?
    public let backdropURL: URL?
    public let genres: [String]?
    public let numberOfSeasons: Int?
    public let seriesID: String?
    public let seriesTitle: String?
    public let seasonNumber: Int?
    public let episodeNumber: Int?
    public let episodeTitle: String?
    public let positionSeconds: Double
    public let durationSeconds: Double
    private let subtitleSelection: WatchSubtitleSelection

    public init(
        movie: Movie,
        positionSeconds: Double,
        durationSeconds: Double,
        activeSubtitle: HLSSubtitleTrack? = nil
    ) {
        mediaID = movie.id
        mediaType = movie.mediaType
        title = movie.title
        year = movie.year
        overview = movie.overview
        posterURL = movie.posterURL
        backdropURL = movie.backdropURL
        genres = movie.genres
        numberOfSeasons = movie.numberOfSeasons
        seriesID = movie.seriesID
        seriesTitle = movie.seriesTitle
        seasonNumber = movie.seasonNumber
        episodeNumber = movie.episodeNumber
        episodeTitle = movie.episodeTitle
        self.positionSeconds = positionSeconds
        self.durationSeconds = durationSeconds
        subtitleSelection = WatchSubtitleSelection(track: activeSubtitle)
    }

    private enum CodingKeys: String, CodingKey {
        case title, year, overview, genres
        case mediaID = "media_id"
        case mediaType = "media_type"
        case posterURL = "poster_url"
        case backdropURL = "backdrop_url"
        case numberOfSeasons = "number_of_seasons"
        case seriesID = "series_id"
        case seriesTitle = "series_title"
        case seasonNumber = "season_number"
        case episodeNumber = "episode_number"
        case episodeTitle = "episode_title"
        case positionSeconds = "position_seconds"
        case durationSeconds = "duration_seconds"
        case subtitleSelection = "subtitle_selection"
    }
}

private struct WatchSubtitleSelection: Encodable, Sendable {
    let mode: String
    let index: Int?
    let language: String?
    let title: String?
    let codec: String?
    let isDefault: Bool
    let isForced: Bool

    init(track: HLSSubtitleTrack?) {
        mode = track == nil ? "off" : (track?.isBitmap == true ? "bitmap" : "text")
        index = track?.index
        language = track?.language
        title = track?.title
        codec = track?.codec
        isDefault = track?.isDefault == true
        isForced = track?.isForced == true
    }

    private enum CodingKeys: String, CodingKey {
        case mode, index, language, title, codec
        case isDefault = "default"
        case isForced = "forced"
    }
}
