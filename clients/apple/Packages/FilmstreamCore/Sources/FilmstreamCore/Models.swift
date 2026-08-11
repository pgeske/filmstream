import Foundation

public struct Movie: Codable, Hashable, Identifiable, Sendable {
    public let id: String
    public let title: String
    public let originalTitle: String?
    public let year: Int?
    public let releaseDate: String?
    public let overview: String?
    public let posterURL: URL?
    public let backdropURL: URL?
    public let voteAverage: Double?

    public init(
        id: String,
        title: String,
        originalTitle: String? = nil,
        year: Int? = nil,
        releaseDate: String? = nil,
        overview: String? = nil,
        posterURL: URL? = nil,
        backdropURL: URL? = nil,
        voteAverage: Double? = nil
    ) {
        self.id = id
        self.title = title
        self.originalTitle = originalTitle
        self.year = year
        self.releaseDate = releaseDate
        self.overview = overview
        self.posterURL = posterURL
        self.backdropURL = backdropURL
        self.voteAverage = voteAverage
    }

    private enum CodingKeys: String, CodingKey {
        case id, title, year, overview
        case originalTitle = "original_title"
        case releaseDate = "release_date"
        case posterURL = "poster_url"
        case backdropURL = "backdrop_url"
        case voteAverage = "vote_average"
    }
}

public struct WatchHistoryEntry: Codable, Hashable, Identifiable, Sendable {
    public let id: String
    public let mediaID: String?
    public let title: String
    public let year: Int?
    public let overview: String?
    public let posterURL: URL?
    public let backdropURL: URL?
    public let positionSeconds: Double
    public let durationSeconds: Double?
    public let completed: Bool
    public let updatedAt: String

    public var progress: Double {
        guard let durationSeconds, durationSeconds > 0 else { return 0 }
        return max(0, min(1, positionSeconds / durationSeconds))
    }

    public var movie: Movie {
        Movie(
            id: mediaID ?? "filmstream-history:\(id)",
            title: title,
            year: year,
            overview: overview,
            posterURL: posterURL,
            backdropURL: backdropURL
        )
    }

    private enum CodingKeys: String, CodingKey {
        case id, title, year, overview, completed
        case mediaID = "media_id"
        case posterURL = "poster_url"
        case backdropURL = "backdrop_url"
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
    public let title: String
    public let year: Int?
    public let overview: String?
    public let posterURL: URL?
    public let backdropURL: URL?
    public let positionSeconds: Double
    public let durationSeconds: Double

    public init(movie: Movie, positionSeconds: Double, durationSeconds: Double) {
        mediaID = movie.id
        title = movie.title
        year = movie.year
        overview = movie.overview
        posterURL = movie.posterURL
        backdropURL = movie.backdropURL
        self.positionSeconds = positionSeconds
        self.durationSeconds = durationSeconds
    }

    private enum CodingKeys: String, CodingKey {
        case title, year, overview
        case mediaID = "media_id"
        case posterURL = "poster_url"
        case backdropURL = "backdrop_url"
        case positionSeconds = "position_seconds"
        case durationSeconds = "duration_seconds"
    }
}
