import Foundation

public struct SubtitleCue: Equatable, Sendable {
    public let startSeconds: Double
    public let endSeconds: Double
    public let text: String

    public init(startSeconds: Double, endSeconds: Double, text: String) {
        self.startSeconds = startSeconds
        self.endSeconds = endSeconds
        self.text = text
    }
}

public enum WebVTTParser {
    public static func parse(_ data: Data, offsetSeconds: Double = 0) -> [SubtitleCue] {
        guard let contents = String(data: data, encoding: .utf8) else { return [] }
        let lines = contents.replacingOccurrences(of: "\r\n", with: "\n").components(separatedBy: "\n")
        var cues: [SubtitleCue] = []
        var index = 0

        while index < lines.count {
            var timing = lines[index].trimmingCharacters(in: .whitespaces)
            if !timing.contains("-->") && index + 1 < lines.count {
                let next = lines[index + 1].trimmingCharacters(in: .whitespaces)
                if next.contains("-->") {
                    index += 1
                    timing = next
                }
            }
            guard timing.contains("-->") else {
                index += 1
                continue
            }

            let parts = timing.components(separatedBy: "-->")
            guard parts.count == 2,
                  let start = timestamp(parts[0]),
                  let end = timestamp(
                    parts[1].trimmingCharacters(in: .whitespaces)
                        .components(separatedBy: .whitespaces).first ?? ""
                  ) else {
                index += 1
                continue
            }

            index += 1
            var textLines: [String] = []
            while index < lines.count, !lines[index].isEmpty {
                textLines.append(lines[index])
                index += 1
            }
            let text = cleanText(textLines.joined(separator: "\n"))
            if !text.isEmpty, end >= start {
                cues.append(
                    SubtitleCue(
                        startSeconds: offsetSeconds + start,
                        endSeconds: offsetSeconds + end,
                        text: text
                    )
                )
            }
        }
        return cues
    }

    private static func timestamp(_ value: String) -> Double? {
        let components = value.trimmingCharacters(in: .whitespaces).split(separator: ":")
        guard components.count == 2 || components.count == 3 else { return nil }
        guard let seconds = Double(components.last ?? "") else { return nil }
        let minutesIndex = components.count - 2
        guard let minutes = Double(components[minutesIndex]) else { return nil }
        let hours = components.count == 3 ? Double(components[0]) ?? 0 : 0
        return hours * 3600 + minutes * 60 + seconds
    }

    private static func cleanText(_ value: String) -> String {
        var text = value.replacingOccurrences(
            of: "<[^>]+>",
            with: "",
            options: .regularExpression
        )
        for (entity, replacement) in [
            ("&amp;", "&"), ("&lt;", "<"), ("&gt;", ">"),
            ("&quot;", "\""), ("&#39;", "'"), ("&nbsp;", " "),
            ("&lrm;", ""), ("&rlm;", "")
        ] {
            text = text.replacingOccurrences(of: entity, with: replacement)
        }
        return text.trimmingCharacters(in: .whitespacesAndNewlines)
    }
}
