import FilmstreamCore
import Foundation
import SwiftUI
import UIKit

extension Color {
    static let teaBackground = Color(red: 0.027, green: 0.067, blue: 0.047)
    static let teaBackgroundTop = Color(red: 0.063, green: 0.145, blue: 0.102)
    static let teaPanel = Color(red: 0.071, green: 0.153, blue: 0.110)
    static let teaPanelElevated = Color(red: 0.106, green: 0.212, blue: 0.153)
    static let teaAccent = Color(red: 0.624, green: 0.769, blue: 0.482)
    static let teaAccentLight = Color(red: 0.784, green: 0.867, blue: 0.663)
    static let teaCream = Color(red: 0.953, green: 0.922, blue: 0.867)
    static let teaMuted = Color(red: 0.682, green: 0.725, blue: 0.659)
    static let teaAmber = Color(red: 0.839, green: 0.631, blue: 0.373)
}

struct TeaBackground: View {
    var body: some View {
        ZStack {
            LinearGradient(
                colors: [Color.teaBackgroundTop, Color.teaBackground],
                startPoint: .topLeading,
                endPoint: .bottomTrailing
            )
            RadialGradient(
                colors: [Color.teaAccent.opacity(0.14), .clear],
                center: .topTrailing,
                startRadius: 20,
                endRadius: 920
            )
            RadialGradient(
                colors: [Color.teaAmber.opacity(0.055), .clear],
                center: .bottomLeading,
                startRadius: 10,
                endRadius: 760
            )
        }
        .ignoresSafeArea()
    }
}

struct TeaStreamMark: View {
    var size: CGFloat = 54

    var body: some View {
        ZStack {
            RoundedRectangle(cornerRadius: size * 0.3, style: .continuous)
                .fill(
                    LinearGradient(
                        colors: [Color.teaAccentLight, Color.teaAccent],
                        startPoint: .topLeading,
                        endPoint: .bottomTrailing
                    )
                )
            Image(systemName: "leaf.fill")
                .font(.system(size: size * 0.58, weight: .semibold))
                .foregroundStyle(Color.teaBackground)
                .rotationEffect(.degrees(-18))
            Image(systemName: "play.fill")
                .font(.system(size: size * 0.22, weight: .black))
                .foregroundStyle(Color.teaCream)
                .offset(x: -1)
        }
        .frame(width: size, height: size)
        .shadow(color: Color.teaAccent.opacity(0.22), radius: 18, y: 8)
        .accessibilityHidden(true)
    }
}

struct BrandHeader: View {
    var body: some View {
        HStack(spacing: 16) {
            TeaStreamMark()
            HStack(spacing: 0) {
                Text("TEA")
                    .foregroundStyle(Color.teaAccentLight)
                Text("STREAM")
                    .foregroundStyle(Color.teaCream)
            }
            .font(.system(size: 35, weight: .black, design: .rounded))
            .tracking(2.2)
        }
        .accessibilityElement(children: .combine)
        .accessibilityLabel("TeaStream")
    }
}

struct TeaActionButtonStyle: ButtonStyle {
    var prominent = false

    func makeBody(configuration: Configuration) -> some View {
        TeaActionButtonBody(
            configuration: configuration,
            prominent: prominent
        )
    }
}

private struct TeaActionButtonBody: View {
    let configuration: ButtonStyle.Configuration
    let prominent: Bool

    @Environment(\.isFocused) private var isFocused

    var body: some View {
        configuration.label
            .foregroundStyle(isFocused ? Color.teaBackground : Color.teaCream)
            .padding(.horizontal, 24)
            .padding(.vertical, 14)
            .background(
                isFocused
                    ? Color.teaAccentLight
                    : prominent ? Color.teaAccent : Color.teaPanelElevated,
                in: Capsule()
            )
            .overlay {
                Capsule()
                    .stroke(
                        isFocused ? Color.teaCream.opacity(0.62) : Color.teaAccent.opacity(0.32),
                        lineWidth: 1.5
                    )
            }
            .shadow(
                color: isFocused ? Color.teaAccent.opacity(0.36) : .black.opacity(0.18),
                radius: isFocused ? 24 : 10,
                y: isFocused ? 10 : 6
            )
            .scaleEffect(configuration.isPressed ? 0.97 : isFocused ? 1.055 : 1)
            .animation(.snappy(duration: 0.2), value: isFocused)
            .animation(.easeOut(duration: 0.1), value: configuration.isPressed)
    }
}

private struct MovieCardButtonStyle: ButtonStyle {
    func makeBody(configuration: Configuration) -> some View {
        configuration.label
            .opacity(configuration.isPressed ? 0.82 : 1)
            .scaleEffect(configuration.isPressed ? 0.985 : 1)
            .animation(.easeOut(duration: 0.12), value: configuration.isPressed)
    }
}

struct MovieNavigationCard: View {
    let movie: Movie
    var progress: Double? = nil
    var requestsInitialFocus = false
    var action: (() -> Void)? = nil
    @FocusState private var isFocused: Bool

    private let cardWidth: CGFloat = 250

    var body: some View {
        Group {
            if let action {
                Button(action: action) {
                    cardContent
                }
            } else {
                NavigationLink(value: movie) {
                    cardContent
                }
            }
        }
        .buttonStyle(MovieCardButtonStyle())
        .focusEffectDisabled()
        .focused($isFocused)
        .zIndex(isFocused ? 1 : 0)
        .task {
            if requestsInitialFocus {
                isFocused = true
            }
        }
    }

    private var cardContent: some View {
        VStack(alignment: .leading, spacing: 13) {
            PosterImage(movie: movie)
                .frame(width: cardWidth, height: 375)
                .overlay {
                    RoundedRectangle(cornerRadius: 20, style: .continuous)
                        .stroke(
                            isFocused ? Color.teaAccentLight : Color.teaCream.opacity(0.12),
                            lineWidth: isFocused ? 3 : 1
                        )
                }
                .overlay(alignment: .bottom) {
                    if let progress, progress > 0 {
                        ProgressView(value: progress)
                            .tint(Color.teaAccent)
                            .padding(.horizontal, 12)
                            .padding(.bottom, 11)
                    }
                }
                .shadow(
                    color: isFocused ? Color.teaAccent.opacity(0.25) : .black.opacity(0.35),
                    radius: isFocused ? 28 : 12,
                    y: isFocused ? 14 : 8
                )

            Text(movie.title)
                .font(.headline.weight(.semibold))
                .foregroundStyle(Color.teaCream)
                .lineLimit(2)
                .frame(width: cardWidth, height: 58, alignment: .topLeading)
            if let year = movie.year {
                Text(String(year))
                    .font(.subheadline)
                    .foregroundStyle(Color.teaMuted)
            }
        }
        .frame(width: cardWidth, alignment: .leading)
        .contentShape(RoundedRectangle(cornerRadius: 20, style: .continuous))
        .scaleEffect(isFocused ? 1.035 : 1)
        .offset(y: isFocused ? -6 : 0)
        .animation(.snappy(duration: 0.22), value: isFocused)
    }
}

struct PosterImage: View {
    let movie: Movie

    @State private var imageData: Data?
    @State private var isLoading = false

    var body: some View {
        Group {
            if let imageData, let image = UIImage(data: imageData) {
                Image(uiImage: image)
                    .resizable()
                    .scaledToFill()
            } else if isLoading {
                ZStack {
                    Color.teaPanel
                    ProgressView()
                }
            } else {
                placeholder
            }
        }
        .clipShape(RoundedRectangle(cornerRadius: 20, style: .continuous))
        .task(id: movie) {
            await loadArtwork()
        }
    }

    private var placeholder: some View {
        ZStack {
            LinearGradient(
                colors: [Color.teaPanelElevated, Color.teaBackground],
                startPoint: .topLeading,
                endPoint: .bottomTrailing
            )
            Image(systemName: "film.stack")
                .font(.system(size: 54))
                .foregroundStyle(Color.teaAccentLight.opacity(0.38))
        }
    }

    private func loadArtwork() async {
        imageData = nil
        let urls = [movie.posterURL, movie.backdropURL].compactMap { $0 }
        guard !urls.isEmpty else { return }

        isLoading = true
        defer { isLoading = false }
        for url in urls {
            for attempt in 0..<2 {
                do {
                    var request = URLRequest(
                        url: url,
                        cachePolicy: attempt == 0 ? .returnCacheDataElseLoad : .reloadIgnoringLocalCacheData,
                        timeoutInterval: 20
                    )
                    request.setValue("image/avif,image/webp,image/*,*/*;q=0.8", forHTTPHeaderField: "Accept")
                    let (data, response) = try await URLSession.shared.data(for: request)
                    guard let response = response as? HTTPURLResponse,
                          (200..<300).contains(response.statusCode),
                          UIImage(data: data) != nil else {
                        continue
                    }
                    imageData = data
                    return
                } catch is CancellationError {
                    return
                } catch {
                    if attempt == 0 {
                        try? await Task.sleep(for: .milliseconds(350))
                    }
                }
            }
        }
    }
}

struct BackdropImage: View {
    let movie: Movie

    var body: some View {
        AsyncImage(url: movie.backdropURL) { phase in
            if case let .success(image) = phase {
                image
                    .resizable()
                    .scaledToFill()
            } else {
                LinearGradient(
                    colors: [Color.teaPanelElevated, Color.teaBackground],
                    startPoint: .topTrailing,
                    endPoint: .bottomLeading
                )
            }
        }
    }
}
