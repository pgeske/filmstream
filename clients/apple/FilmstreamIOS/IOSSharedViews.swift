import FilmstreamCore
import Foundation
import SwiftUI

extension Color {
    static let mobileTeaBackground = Color(red: 0.027, green: 0.067, blue: 0.047)
    static let mobileTeaBackgroundTop = Color(red: 0.063, green: 0.145, blue: 0.102)
    static let mobileTeaPanel = Color(red: 0.071, green: 0.153, blue: 0.110)
    static let mobileTeaPanelElevated = Color(red: 0.106, green: 0.212, blue: 0.153)
    static let mobileTeaAccent = Color(red: 0.624, green: 0.769, blue: 0.482)
    static let mobileTeaAccentLight = Color(red: 0.784, green: 0.867, blue: 0.663)
    static let mobileTeaCream = Color(red: 0.953, green: 0.922, blue: 0.867)
    static let mobileTeaMuted = Color(red: 0.682, green: 0.725, blue: 0.659)
    static let mobileTeaAmber = Color(red: 0.839, green: 0.631, blue: 0.373)
    static let mobileTeaHoney = Color(red: 0.855, green: 0.694, blue: 0.365)
    static let mobileTeaTomato = Color(red: 0.780, green: 0.302, blue: 0.251)
}

struct MobileTeaBackground: View {
    var body: some View {
        ZStack {
            LinearGradient(
                colors: [Color.mobileTeaBackgroundTop, Color.mobileTeaBackground],
                startPoint: .topLeading,
                endPoint: .bottomTrailing
            )
            RadialGradient(
                colors: [Color.mobileTeaAccent.opacity(0.15), .clear],
                center: .topTrailing,
                startRadius: 8,
                endRadius: 520
            )
            RadialGradient(
                colors: [Color.mobileTeaAmber.opacity(0.05), .clear],
                center: .bottomLeading,
                startRadius: 8,
                endRadius: 460
            )
        }
        .ignoresSafeArea()
    }
}

struct MobileTeaStreamMark: View {
    var size: CGFloat = 40

    var body: some View {
        Image("TeaStreamMark")
            .resizable()
            .scaledToFill()
            .frame(width: size, height: size)
            .clipShape(RoundedRectangle(cornerRadius: size * 0.28, style: .continuous))
            .overlay {
                RoundedRectangle(cornerRadius: size * 0.28, style: .continuous)
                    .stroke(Color.mobileTeaCream.opacity(0.24), lineWidth: max(1, size * 0.018))
            }
            .shadow(color: Color.mobileTeaAccent.opacity(0.16), radius: 9, y: 4)
            .accessibilityHidden(true)
    }
}

struct MobileBrandHeader: View {
    @Environment(\.horizontalSizeClass) private var horizontalSizeClass
    var compact = false

    var body: some View {
        HStack(spacing: compact ? 9 : horizontalSizeClass == .regular ? 14 : 11) {
            MobileTeaStreamMark(
                size: compact ? 34 : horizontalSizeClass == .regular ? 48 : 40
            )
            HStack(spacing: 0) {
                Text("TEA")
                    .foregroundStyle(Color.mobileTeaAccentLight)
                Text("STREAM")
                    .foregroundStyle(Color.mobileTeaCream)
            }
            .font(.system(
                size: compact ? 18 : horizontalSizeClass == .regular ? 29 : 23,
                weight: .black,
                design: .rounded
            ))
            .tracking(compact ? 1.1 : horizontalSizeClass == .regular ? 1.9 : 1.5)
        }
        .accessibilityElement(children: .combine)
        .accessibilityLabel("TeaStream")
    }
}

struct MobileSectionHeader: View {
    @Environment(\.horizontalSizeClass) private var horizontalSizeClass
    let title: String

    var body: some View {
        Text(title)
            .font(horizontalSizeClass == .regular ? .title.weight(.bold) : .title2.weight(.bold))
            .foregroundStyle(Color.mobileTeaCream)
    }
}

struct MobileCardButtonStyle: ButtonStyle {
    func makeBody(configuration: Configuration) -> some View {
        configuration.label
            .opacity(configuration.isPressed ? 0.84 : 1)
            .scaleEffect(configuration.isPressed ? 0.975 : 1)
            .animation(.easeOut(duration: 0.12), value: configuration.isPressed)
            .hoverEffect(.lift)
    }
}

struct MobileMovieCard: View {
    let movie: Movie
    var progress: Double? = nil
    var contentRating: String? = nil
    var width: CGFloat = 300

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            MobileShelfArtwork(movie: movie)
                .frame(width: width, height: width * 9 / 16)
                .clipped()
                .clipShape(RoundedRectangle(cornerRadius: 15, style: .continuous))
                .overlay {
                    RoundedRectangle(cornerRadius: 15, style: .continuous)
                        .stroke(Color.mobileTeaCream.opacity(0.13), lineWidth: 1)
                }
                .overlay(alignment: .topTrailing) {
                    Image(systemName: "chevron.right")
                        .font(.caption.weight(.black))
                        .foregroundStyle(Color.mobileTeaCream)
                        .frame(width: 30, height: 30)
                        .background(Color.mobileTeaBackground.opacity(0.78), in: Circle())
                        .overlay {
                            Circle()
                                .stroke(Color.mobileTeaCream.opacity(0.16), lineWidth: 1)
                        }
                        .padding(10)
                        .accessibilityHidden(true)
                }
                .overlay(alignment: .bottom) {
                    if let progress, progress > 0 {
                        ProgressView(value: progress)
                            .tint(Color.mobileTeaAccent)
                            .padding(.horizontal, 9)
                            .padding(.bottom, 8)
                    }
                }
                .shadow(color: .black.opacity(0.36), radius: 11, y: 6)

            Text(movie.title)
                .font(.headline.weight(.bold))
                .foregroundStyle(Color.mobileTeaCream)
                .lineLimit(2, reservesSpace: true)
                .frame(width: width, alignment: .leading)

            Text(movie.catalogMetadata)
                .font(.caption.weight(.semibold))
                .foregroundStyle(Color.mobileTeaMuted)
                .lineLimit(2, reservesSpace: true)

            HStack(spacing: 7) {
                if let episodeLabel = movie.episodeLabel {
                    Text(episodeLabel)
                        .foregroundStyle(Color.mobileTeaAccentLight)
                }
                if let contentRating {
                    Text(contentRating)
                }
                if let progress, progress > 0 {
                    Text("\(Int(progress * 100))% watched")
                        .foregroundStyle(Color.mobileTeaAccentLight)
                }
            }
            .font(.caption.weight(.semibold))
            .foregroundStyle(Color.mobileTeaMuted)
            .frame(height: 16, alignment: .leading)

            Text(movie.overview.flatMap { $0.isEmpty ? nil : $0 } ?? " ")
                .font(.caption)
                .foregroundStyle(Color.mobileTeaCream.opacity(0.72))
                .lineLimit(2, reservesSpace: true)
                .frame(width: width, alignment: .leading)
        }
        .frame(width: width, alignment: .leading)
        .contentShape(Rectangle())
    }
}

private struct MobileShelfArtwork: View {
    let movie: Movie

    var body: some View {
        AsyncImage(url: movie.backdropURL ?? movie.posterURL) { phase in
            switch phase {
            case let .success(image):
                image
                    .resizable()
                    .scaledToFill()
            case .empty:
                ZStack {
                    Color.mobileTeaPanel
                    ProgressView()
                        .tint(Color.mobileTeaAccent)
                }
            default:
                LinearGradient(
                    colors: [Color.mobileTeaPanelElevated, Color.mobileTeaBackground],
                    startPoint: .topLeading,
                    endPoint: .bottomTrailing
                )
                .overlay {
                    Image(systemName: "film.stack")
                        .font(.system(size: 34))
                        .foregroundStyle(Color.mobileTeaAccentLight.opacity(0.38))
                }
            }
        }
        .overlay {
            LinearGradient(
                stops: [
                    .init(color: .clear, location: 0.5),
                    .init(color: Color.mobileTeaBackground.opacity(0.34), location: 1),
                ],
                startPoint: .top,
                endPoint: .bottom
            )
        }
        .clipShape(RoundedRectangle(cornerRadius: 15, style: .continuous))
    }
}

struct MobilePosterImage: View {
    let movie: Movie

    var body: some View {
        AsyncImage(url: movie.posterURL ?? movie.backdropURL) { phase in
            switch phase {
            case let .success(image):
                image
                    .resizable()
                    .scaledToFill()
            case .empty:
                ZStack {
                    Color.mobileTeaPanel
                    ProgressView()
                        .tint(Color.mobileTeaAccent)
                }
            default:
                placeholder
            }
        }
        .clipShape(RoundedRectangle(cornerRadius: 14, style: .continuous))
    }

    private var placeholder: some View {
        ZStack {
            LinearGradient(
                colors: [Color.mobileTeaPanelElevated, Color.mobileTeaBackground],
                startPoint: .topLeading,
                endPoint: .bottomTrailing
            )
            Image(systemName: "film.stack")
                .font(.system(size: 34))
                .foregroundStyle(Color.mobileTeaAccentLight.opacity(0.38))
        }
    }
}

struct MobileBackdropImage: View {
    let movie: Movie

    var body: some View {
        AsyncImage(url: movie.backdropURL) { phase in
            if case let .success(image) = phase {
                image
                    .resizable()
                    .scaledToFill()
            } else {
                LinearGradient(
                    colors: [Color.mobileTeaPanelElevated, Color.mobileTeaBackground],
                    startPoint: .topTrailing,
                    endPoint: .bottomLeading
                )
            }
        }
    }
}

/// The regular-width detail treatment mirrors tvOS without compromising compact scrolling.
struct IOSCinematicDetailBackground: View {
    let movie: Movie

    var body: some View {
        MobileBackdropImage(movie: movie)
            .frame(maxWidth: .infinity, maxHeight: .infinity)
            .clipped()
            .overlay {
                LinearGradient(
                    stops: [
                        .init(color: Color.mobileTeaBackground.opacity(0.99), location: 0),
                        .init(color: Color.mobileTeaBackground.opacity(0.9), location: 0.34),
                        .init(color: Color.mobileTeaBackground.opacity(0.24), location: 0.76),
                        .init(color: .clear, location: 1),
                    ],
                    startPoint: .leading,
                    endPoint: .trailing
                )
            }
            .overlay {
                LinearGradient(
                    stops: [
                        .init(color: .clear, location: 0.48),
                        .init(color: Color.mobileTeaBackground.opacity(0.64), location: 0.82),
                        .init(color: Color.mobileTeaBackground, location: 1),
                    ],
                    startPoint: .top,
                    endPoint: .bottom
                )
            }
            .ignoresSafeArea()
    }
}

struct MobileRatingBadges: View {
    let ratings: MovieRatings?

    var body: some View {
        HStack(spacing: 7) {
            if let imdb = ratings?.imdb {
                MobileRatingBadge(source: .imdb, value: String(format: "%.1f", imdb))
            }
            if let rottenTomatoes = ratings?.rottenTomatoes {
                MobileRatingBadge(source: .rottenTomatoes, value: "\(rottenTomatoes)%")
            }
        }
        .accessibilityElement(children: .combine)
    }
}

private enum MobileRatingSource {
    case imdb
    case rottenTomatoes

    var accessibilityName: String {
        switch self {
        case .imdb: "IMDb"
        case .rottenTomatoes: "Rotten Tomatoes"
        }
    }
}

private struct MobileRatingBadge: View {
    let source: MobileRatingSource
    let value: String

    var body: some View {
        HStack(spacing: 5) {
            mark
            Text(value)
                .font(.caption.weight(.semibold))
                .monospacedDigit()
                .foregroundStyle(Color.mobileTeaCream.opacity(0.94))
        }
        .padding(.horizontal, 8)
        .padding(.vertical, 6)
        .background(Color.mobileTeaPanel.opacity(0.88), in: RoundedRectangle(cornerRadius: 9))
        .overlay {
            RoundedRectangle(cornerRadius: 9)
                .stroke(Color.mobileTeaCream.opacity(0.11), lineWidth: 1)
        }
        .accessibilityElement(children: .ignore)
        .accessibilityLabel("\(source.accessibilityName) \(value)")
    }

    @ViewBuilder
    private var mark: some View {
        switch source {
        case .imdb:
            Text("IMDb")
                .font(.system(size: 9, weight: .black, design: .rounded))
                .foregroundStyle(Color.mobileTeaBackground)
                .padding(.horizontal, 3)
                .padding(.vertical, 2)
                .background(Color.mobileTeaHoney, in: RoundedRectangle(cornerRadius: 3))
        case .rottenTomatoes:
            ZStack {
                Circle()
                    .fill(Color.mobileTeaTomato)
                    .frame(width: 14, height: 14)
                Image(systemName: "leaf.fill")
                    .font(.system(size: 7, weight: .bold))
                    .foregroundStyle(Color.mobileTeaAccent)
                    .offset(y: -6)
            }
            .frame(width: 16, height: 17)
        }
    }
}

enum MobileDetailButtonKind: Equatable {
    case prominent
    case standard
    case destructive
}

struct MobileDetailButtonStyle: ButtonStyle {
    let kind: MobileDetailButtonKind

    func makeBody(configuration: Configuration) -> some View {
        configuration.label
            .font(.headline.weight(.bold))
            .foregroundStyle(foregroundColor)
            .padding(.horizontal, 16)
            .frame(maxWidth: .infinity)
            .frame(height: 52)
            .background(backgroundColor, in: RoundedRectangle(cornerRadius: 14, style: .continuous))
            .overlay {
                RoundedRectangle(cornerRadius: 14, style: .continuous)
                    .stroke(borderColor, lineWidth: 1)
            }
            .shadow(
                color: kind == .prominent ? Color.mobileTeaAccent.opacity(0.16) : .clear,
                radius: 14,
                y: 7
            )
            .opacity(configuration.isPressed ? 0.82 : 1)
            .scaleEffect(configuration.isPressed ? 0.985 : 1)
            .animation(.easeOut(duration: 0.1), value: configuration.isPressed)
            .hoverEffect(.highlight)
    }

    private var foregroundColor: Color {
        switch kind {
        case .prominent:
            return Color.mobileTeaBackground
        case .standard:
            return Color.mobileTeaCream
        case .destructive:
            return Color.mobileTeaAmber
        }
    }

    private var backgroundColor: Color {
        switch kind {
        case .prominent:
            return Color.mobileTeaAccent
        case .standard:
            return Color.mobileTeaPanelElevated
        case .destructive:
            return Color.mobileTeaBackground.opacity(0.7)
        }
    }

    private var borderColor: Color {
        kind == .destructive ? Color.mobileTeaAmber.opacity(0.42) : Color.mobileTeaCream.opacity(0.12)
    }
}
