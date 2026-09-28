import Foundation

/// `GET /v1/playbacks/{id}`. Every field is optional: torrent, Usenet, and older
/// servers report different subsets.
public struct PlaybackStatus: Decodable, Hashable, Sendable {
    public struct Packaging: Decodable, Hashable, Sendable {
        /// starting, ready, buffering, parked, complete, or failed.
        public let state: String
        public let packagedSeconds: Double?
        public let targetSeconds: Double?
        public let secondsSinceProgress: Double?
        public let complete: Bool?
        /// The packager is deliberately paused far ahead of the player.
        public let throttled: Bool?
        public let error: String?

        public init(
            state: String,
            packagedSeconds: Double? = nil,
            targetSeconds: Double? = nil,
            secondsSinceProgress: Double? = nil,
            complete: Bool? = nil,
            throttled: Bool? = nil,
            error: String? = nil
        ) {
            self.state = state
            self.packagedSeconds = packagedSeconds
            self.targetSeconds = targetSeconds
            self.secondsSinceProgress = secondsSinceProgress
            self.complete = complete
            self.throttled = throttled
            self.error = error
        }

        public var isFailed: Bool { state == "failed" }

        private enum CodingKeys: String, CodingKey {
            case state, complete, throttled, error
            case packagedSeconds = "packaged_seconds"
            case targetSeconds = "target_seconds"
            case secondsSinceProgress = "seconds_since_progress"
        }
    }

    public let state: String?
    public let activePeers: Int?
    public let connectedSeeders: Int?
    public let totalPeers: Int?
    public let downloadRate: Int64?
    public let progress: Double?
    public let sourceUnavailable: Bool?
    public let trackerMessage: String?
    public let hls: Packaging?

    public init(
        state: String? = nil,
        activePeers: Int? = nil,
        connectedSeeders: Int? = nil,
        totalPeers: Int? = nil,
        downloadRate: Int64? = nil,
        progress: Double? = nil,
        sourceUnavailable: Bool? = nil,
        trackerMessage: String? = nil,
        hls: Packaging? = nil
    ) {
        self.state = state
        self.activePeers = activePeers
        self.connectedSeeders = connectedSeeders
        self.totalPeers = totalPeers
        self.downloadRate = downloadRate
        self.progress = progress
        self.sourceUnavailable = sourceUnavailable
        self.trackerMessage = trackerMessage
        self.hls = hls
    }

    /// The server cannot continue this stream without being asked to prepare it again.
    public var needsReprepare: Bool {
        hls?.isFailed == true || sourceUnavailable == true
    }

    /// Short live transfer summary, for example "12 peers · 8.4 MB/s".
    public var transferSummary: String? {
        var parts: [String] = []
        if let activePeers {
            parts.append(activePeers == 1 ? "1 peer" : "\(activePeers) peers")
        }
        if let downloadRate, downloadRate > 0 {
            parts.append(Self.formatRate(downloadRate))
        }
        return parts.isEmpty ? nil : parts.joined(separator: " · ")
    }

    static func formatRate(_ bytesPerSecond: Int64) -> String {
        let value = Double(bytesPerSecond)
        if value >= 1_000_000 {
            return String(format: "%.1f MB/s", value / 1_000_000)
        }
        return "\(max(1, Int((value / 1_000).rounded()))) KB/s"
    }

    private enum CodingKeys: String, CodingKey {
        case state, progress, hls
        case activePeers = "active_peers"
        case connectedSeeders = "connected_seeders"
        case totalPeers = "total_peers"
        case downloadRate = "download_rate"
        case sourceUnavailable = "source_unavailable"
        case trackerMessage = "tracker_message"
    }
}

/// What the viewer sees while a title is being prepared or while playback waits for data.
public struct PlaybackProgressDescription: Hashable, Sendable {
    public let headline: String
    public let detail: String?
    /// Known completion of the startup buffer, 0...1.
    public let fraction: Double?

    public init(headline: String, detail: String?, fraction: Double?) {
        self.headline = headline
        self.detail = detail
        self.fraction = fraction
    }

    public init(stage: PlaybackPreparationStage, status: PlaybackStatus?) {
        guard case .bufferingVideo = stage else {
            self.init(headline: "Finding the best release…", detail: nil, fraction: nil)
            return
        }
        guard let status else {
            self.init(headline: "Starting the stream…", detail: nil, fraction: nil)
            return
        }
        if let packaging = status.hls,
           let packaged = packaging.packagedSeconds, packaged > 0,
           let target = packaging.targetSeconds, target > 0 {
            let fraction = min(1, packaged / target)
            self.init(
                headline: "Buffering \(Int((fraction * 100).rounded()))%",
                detail: status.transferSummary,
                fraction: fraction
            )
            return
        }
        self.init(waitingFor: status)
    }

    /// Why playback is waiting, from the server's point of view.
    public init(waitingFor status: PlaybackStatus) {
        if let activePeers = status.activePeers, activePeers == 0 {
            var detail = status.totalPeers.map { $0 == 1 ? "1 peer found" : "\($0) peers found" }
            if let message = status.trackerMessage, !message.isEmpty {
                detail = "Tracker: \(message)"
            }
            self.init(headline: "Connecting to peers…", detail: detail, fraction: nil)
            return
        }
        if status.activePeers != nil, (status.downloadRate ?? 0) == 0 {
            self.init(
                headline: "Waiting for peers to send data…",
                detail: status.transferSummary,
                fraction: nil
            )
            return
        }
        self.init(headline: "Downloading video…", detail: status.transferSummary, fraction: nil)
    }
}
