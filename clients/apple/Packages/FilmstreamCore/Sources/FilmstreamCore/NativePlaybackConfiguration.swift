import AVFoundation

public enum NativePlaybackConfiguration {
    public static let preferredForwardBufferDuration: TimeInterval = 30
    public static let recoveryDelay: Duration = .seconds(12)
    public static let stallTimeout: Duration = .seconds(45)

    public static func configure(_ item: AVPlayerItem) {
        item.preferredForwardBufferDuration = preferredForwardBufferDuration
    }
}
