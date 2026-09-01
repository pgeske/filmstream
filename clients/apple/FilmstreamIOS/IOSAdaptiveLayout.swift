import CoreGraphics

/// Width-driven breakpoints keep iPad split views useful even when the size class stays regular.
struct IOSAdaptiveLayout {
    let width: CGFloat
    let height: CGFloat

    init(width: CGFloat, height: CGFloat) {
        self.width = width
        self.height = height
    }

    var isWide: Bool {
        width >= 700
    }

    var usesCinematicDetail: Bool {
        width >= 760 && width >= height * 1.02
    }

    var usesEpisodeSidebar: Bool {
        width >= 680
    }

}
