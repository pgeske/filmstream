import FilmstreamCore
import Foundation
import SwiftUI

extension Color {
    static let macTeaBackground = Color(red: 0.027, green: 0.067, blue: 0.047)
    static let macTeaBackgroundTop = Color(red: 0.063, green: 0.145, blue: 0.102)
    static let macTeaPanel = Color(red: 0.071, green: 0.153, blue: 0.110)
    static let macTeaPanelElevated = Color(red: 0.106, green: 0.212, blue: 0.153)
    static let macTeaAccent = Color(red: 0.624, green: 0.769, blue: 0.482)
    static let macTeaAccentLight = Color(red: 0.784, green: 0.867, blue: 0.663)
    static let macTeaCream = Color(red: 0.953, green: 0.922, blue: 0.867)
    static let macTeaMuted = Color(red: 0.682, green: 0.725, blue: 0.659)
    static let macTeaAmber = Color(red: 0.839, green: 0.631, blue: 0.373)
    static let macTeaHoney = Color(red: 0.855, green: 0.694, blue: 0.365)
    static let macTeaTomato = Color(red: 0.780, green: 0.302, blue: 0.251)
}

struct MacTeaBackground: View {
    var body: some View {
        ZStack {
            LinearGradient(
                colors: [Color.macTeaBackgroundTop, Color.macTeaBackground],
                startPoint: .topLeading,
                endPoint: .bottomTrailing
            )
            RadialGradient(
                colors: [Color.macTeaAccent.opacity(0.13), .clear],
                center: .topTrailing,
                startRadius: 10,
                endRadius: 720
            )
            RadialGradient(
                colors: [Color.macTeaAmber.opacity(0.05), .clear],
                center: .bottomLeading,
                startRadius: 10,
                endRadius: 620
            )
        }
        .ignoresSafeArea()
    }
}

struct MacTeaStreamMark: View {
    var size: CGFloat = 42

    var body: some View {
        ZStack {
            RoundedRectangle(cornerRadius: size * 0.3, style: .continuous)
                .fill(
                    LinearGradient(
                        colors: [Color.macTeaAccentLight, Color.macTeaAccent],
                        startPoint: .topLeading,
                        endPoint: .bottomTrailing
                    )
                )
            Image(systemName: "leaf.fill")
                .font(.system(size: size * 0.58, weight: .semibold))
                .foregroundStyle(Color.macTeaBackground)
                .rotationEffect(.degrees(-18))
            Image(systemName: "play.fill")
                .font(.system(size: size * 0.22, weight: .black))
                .foregroundStyle(Color.macTeaCream)
                .offset(x: -1)
        }
        .frame(width: size, height: size)
        .shadow(color: Color.macTeaAccent.opacity(0.2), radius: 12, y: 5)
        .accessibilityHidden(true)
    }
}

struct MacBrandHeader: View {
    var compact = false

    var body: some View {
        HStack(spacing: compact ? 10 : 13) {
            MacTeaStreamMark(size: compact ? 32 : 42)
            HStack(spacing: 0) {
                Text("TEA")
                    .foregroundStyle(Color.macTeaAccentLight)
                Text("STREAM")
                    .foregroundStyle(Color.macTeaCream)
            }
            .font(.system(size: compact ? 18 : 25, weight: .black, design: .rounded))
            .tracking(compact ? 1.1 : 1.7)
        }
        .accessibilityElement(children: .combine)
        .accessibilityLabel("TeaStream")
    }
}

struct MacTeaActionButtonStyle: ButtonStyle {
    var prominent = false

    func makeBody(configuration: Configuration) -> some View {
        configuration.label
            .foregroundStyle(prominent ? Color.macTeaBackground : Color.macTeaCream)
            .padding(.horizontal, 18)
            .padding(.vertical, 10)
            .background(
                prominent ? Color.macTeaAccent : Color.macTeaPanelElevated,
                in: Capsule()
            )
            .overlay {
                Capsule()
                    .stroke(Color.macTeaAccent.opacity(0.32), lineWidth: 1)
            }
            .shadow(color: .black.opacity(0.2), radius: 8, y: 4)
            .opacity(configuration.isPressed ? 0.82 : 1)
            .scaleEffect(configuration.isPressed ? 0.98 : 1)
            .animation(.easeOut(duration: 0.1), value: configuration.isPressed)
    }
}

struct MacMovieRatingBadges: View {
    let ratings: MovieRatings?
    let tmdbRating: Double?

    var body: some View {
        HStack(spacing: 8) {
            if let tmdbRating, tmdbRating > 0 {
                badge(mark: "TMDB", value: String(format: "%.1f", tmdbRating), color: .macTeaAccentLight)
                    .accessibilityLabel("TMDB \(String(format: "%.1f", tmdbRating))")
            }
            if let imdb = ratings?.imdb {
                badge(mark: "IMDb", value: String(format: "%.1f", imdb), color: .macTeaHoney)
                    .accessibilityLabel("IMDb \(String(format: "%.1f", imdb))")
            }
            if let rottenTomatoes = ratings?.rottenTomatoes {
                HStack(spacing: 6) {
                    ZStack {
                        Circle()
                            .fill(Color.macTeaTomato)
                            .frame(width: 16, height: 16)
                        Image(systemName: "leaf.fill")
                            .font(.system(size: 8, weight: .bold))
                            .foregroundStyle(Color.macTeaAccent)
                            .offset(y: -7)
                    }
                    .frame(width: 18, height: 20)
                    Text("\(rottenTomatoes)%")
                        .font(.caption.weight(.semibold))
                        .monospacedDigit()
                }
                .ratingBadgeBackground()
                .accessibilityElement(children: .ignore)
                .accessibilityLabel("Rotten Tomatoes \(rottenTomatoes) percent")
            }
        }
    }

    private func badge(mark: String, value: String, color: Color) -> some View {
        HStack(spacing: 6) {
            Text(mark)
                .font(.system(size: 10, weight: .black, design: .rounded))
                .foregroundStyle(mark == "IMDb" ? Color.macTeaBackground : color)
                .padding(.horizontal, mark == "IMDb" ? 4 : 0)
                .padding(.vertical, mark == "IMDb" ? 2 : 0)
                .background(mark == "IMDb" ? color : .clear, in: RoundedRectangle(cornerRadius: 3))
            Text(value)
                .font(.caption.weight(.semibold))
                .monospacedDigit()
        }
        .ratingBadgeBackground()
    }
}

private extension View {
    func ratingBadgeBackground() -> some View {
        padding(.horizontal, 8)
            .padding(.vertical, 5)
            .background(Color.macTeaPanel.opacity(0.86), in: RoundedRectangle(cornerRadius: 9))
            .overlay {
                RoundedRectangle(cornerRadius: 9)
                    .stroke(Color.macTeaCream.opacity(0.1), lineWidth: 1)
            }
    }
}

struct MacMovieCard: View {
    let movie: Movie
    var progress: Double? = nil
    var width: CGFloat = 176
    @State private var isHovering = false

    var body: some View {
        NavigationLink(value: movie) {
            VStack(alignment: .leading, spacing: 10) {
                MacPosterImage(movie: movie)
                    .frame(width: width, height: width * 1.5)
                    .overlay {
                        RoundedRectangle(cornerRadius: 14, style: .continuous)
                            .stroke(
                                isHovering ? Color.macTeaAccentLight : Color.macTeaCream.opacity(0.1),
                                lineWidth: isHovering ? 2 : 1
                            )
                    }
                    .overlay(alignment: .bottom) {
                        if let progress, progress > 0 {
                            ProgressView(value: progress)
                                .tint(Color.macTeaAccent)
                                .padding(.horizontal, 9)
                                .padding(.bottom, 9)
                        }
                    }
                    .shadow(
                        color: isHovering ? Color.macTeaAccent.opacity(0.22) : .black.opacity(0.34),
                        radius: isHovering ? 18 : 9,
                        y: isHovering ? 9 : 5
                    )

                VStack(alignment: .leading, spacing: 4) {
                    Text(movie.title)
                        .font(.headline.weight(.semibold))
                        .foregroundStyle(Color.macTeaCream)
                        .lineLimit(2)
                        .frame(width: width, alignment: .leading)
                    if let year = movie.year {
                        Text(String(year))
                            .font(.caption)
                            .foregroundStyle(Color.macTeaMuted)
                    }
                }
            }
            .frame(width: width, alignment: .leading)
            .contentShape(RoundedRectangle(cornerRadius: 14, style: .continuous))
            .scaleEffect(isHovering ? 1.025 : 1)
            .offset(y: isHovering ? -3 : 0)
            .animation(.snappy(duration: 0.2), value: isHovering)
        }
        .buttonStyle(.plain)
        .onHover { isHovering = $0 }
    }
}

struct MacPosterImage: View {
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
                    Color.macTeaPanel
                    ProgressView()
                        .tint(Color.macTeaAccent)
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
                colors: [Color.macTeaPanelElevated, Color.macTeaBackground],
                startPoint: .topLeading,
                endPoint: .bottomTrailing
            )
            Image(systemName: "film.stack")
                .font(.system(size: 38))
                .foregroundStyle(Color.macTeaAccentLight.opacity(0.38))
        }
    }
}

struct MacBackdropImage: View {
    let movie: Movie

    var body: some View {
        AsyncImage(url: movie.backdropURL) { phase in
            if case let .success(image) = phase {
                image
                    .resizable()
                    .scaledToFill()
            } else {
                LinearGradient(
                    colors: [Color.macTeaPanelElevated, Color.macTeaBackground],
                    startPoint: .topTrailing,
                    endPoint: .bottomLeading
                )
            }
        }
    }
}
