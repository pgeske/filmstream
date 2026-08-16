import Foundation

public enum MediaType: String, Codable, Hashable, Sendable {
    case movie
    case show
}

public struct Movie: Codable, Hashable, Identifiable, Sendable {
    public let id: String
    public let mediaType: MediaType?
    public let title: String
    public let originalTitle: String?
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

    private enum CodingKeys: String, CodingKey {
        case index, language, title
        case isDefault = "default"
        case isForced = "forced"
    }
}

public struct HLSPlayback: Codable, Hashable, Identifiable, Sendable {
    public var id: String { playbackID }
    public let playbackID: String
    public let playlistURL: URL
    public let startSeconds: Double
    public let durationSeconds: Double?
    public let videoCodec: String
    public let subtitles: [HLSSubtitleTrack]?

    private enum CodingKeys: String, CodingKey {
        case playbackID = "playback_id"
        case playlistURL = "playlist_url"
        case startSeconds = "start_seconds"
        case durationSeconds = "duration_seconds"
        case videoCodec = "video_codec"
        case subtitles
    }
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

    public init(movie: Movie, positionSeconds: Double, durationSeconds: Double) {
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
    }
}
