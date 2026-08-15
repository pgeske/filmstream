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
        ZStack {
            RoundedRectangle(cornerRadius: size * 0.3, style: .continuous)
                .fill(
                    LinearGradient(
                        colors: [Color.mobileTeaAccentLight, Color.mobileTeaAccent],
                        startPoint: .topLeading,
                        endPoint: .bottomTrailing
                    )
                )
            Image(systemName: "leaf.fill")
                .font(.system(size: size * 0.58, weight: .semibold))
                .foregroundStyle(Color.mobileTeaBackground)
                .rotationEffect(.degrees(-18))
            Image(systemName: "play.fill")
                .font(.system(size: size * 0.22, weight: .black))
                .foregroundStyle(Color.mobileTeaCream)
                .offset(x: -1)
        }
        .frame(width: size, height: size)
        .shadow(color: Color.mobileTeaAccent.opacity(0.2), radius: 10, y: 4)
        .accessibilityHidden(true)
    }
}

struct MobileBrandHeader: View {
    var body: some View {
        HStack(spacing: 11) {
            MobileTeaStreamMark()
            HStack(spacing: 0) {
                Text("TEA")
                    .foregroundStyle(Color.mobileTeaAccentLight)
                Text("STREAM")
                    .foregroundStyle(Color.mobileTeaCream)
            }
            .font(.system(size: 23, weight: .black, design: .rounded))
            .tracking(1.5)
        }
        .accessibilityElement(children: .combine)
        .accessibilityLabel("TeaStream")
    }
}

struct MobileSectionHeader: View {
    let title: String

    var body: some View {
        Text(title)
            .font(.title2.weight(.bold))
            .foregroundStyle(Color.mobileTeaCream)
    }
}

struct MobileMovieCard: View {
    let movie: Movie
    var progress: Double? = nil
    var width: CGFloat = 286

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            MobileShelfArtwork(movie: movie)
                .frame(width: width, height: width * 9 / 16)
                .overlay {
                    RoundedRectangle(cornerRadius: 15, style: .continuous)
                        .stroke(Color.mobileTeaAccentLight.opacity(0.22), lineWidth: 1)
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
                .lineLimit(1)
                .frame(width: width, alignment: .leading)

            HStack(spacing: 6) {
                Text("Movie")
                if let year = movie.year {
                    Text("•")
                    Text(String(year))
                }
                if let progress, progress > 0 {
                    Text("•")
                    Text("\(Int(progress * 100))% watched")
                        .foregroundStyle(Color.mobileTeaAccentLight)
                }
            }
            .font(.caption.weight(.semibold))
            .foregroundStyle(Color.mobileTeaMuted)

            if let overview = movie.overview, !overview.isEmpty {
                Text(overview)
                    .font(.caption)
                    .foregroundStyle(Color.mobileTeaCream.opacity(0.72))
                    .lineLimit(2)
                    .frame(width: width, alignment: .leading)
            }
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
                colors: [.clear, Color.mobileTeaBackground.opacity(0.24)],
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

struct MobileRatingBadges: View {
    let ratings: MovieRatings?
    let tmdbRating: Double?

    var body: some View {
        HStack(spacing: 7) {
            if let tmdbRating, tmdbRating > 0 {
                MobileRatingBadge(source: .tmdb, value: String(format: "%.1f", tmdbRating))
            }
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
    case tmdb
    case imdb
    case rottenTomatoes

    var accessibilityName: String {
        switch self {
        case .tmdb: "TMDB"
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
        case .tmdb:
            Text("TMDB")
                .font(.system(size: 9, weight: .bold, design: .rounded))
                .foregroundStyle(Color.mobileTeaAccentLight)
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
            .opacity(configuration.isPressed ? 0.82 : 1)
            .scaleEffect(configuration.isPressed ? 0.985 : 1)
            .animation(.easeOut(duration: 0.1), value: configuration.isPressed)
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
