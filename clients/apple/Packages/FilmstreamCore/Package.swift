// swift-tools-version: 6.0
import PackageDescription

let package = Package(
    name: "FilmstreamCore",
    platforms: [
        .iOS(.v18),
        .tvOS(.v18),
        .macOS(.v15),
    ],
    products: [
        .library(name: "FilmstreamCore", targets: ["FilmstreamCore"]),
    ],
    targets: [
        .target(name: "FilmstreamCore"),
        .testTarget(name: "FilmstreamCoreTests", dependencies: ["FilmstreamCore"]),
    ]
)
