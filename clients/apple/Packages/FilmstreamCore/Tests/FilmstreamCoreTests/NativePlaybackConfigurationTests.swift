import AVFoundation
import Testing
@testable import FilmstreamCore

@Test func nativePlaybackConfigurationKeepsBoundedBufferingPolicy() {
    let item = AVPlayerItem(url: URL(string: "https://filmstream.test/index.m3u8")!)
    NativePlaybackConfiguration.configure(item)

    #expect(item.preferredForwardBufferDuration == 30)
    #expect(NativePlaybackConfiguration.recoveryDelay == .seconds(12))
    #expect(NativePlaybackConfiguration.stallTimeout == .seconds(45))
}
