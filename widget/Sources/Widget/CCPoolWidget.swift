import SwiftUI
import WidgetKit

@main
struct CCPoolWidgetBundle: WidgetBundle {
    var body: some Widget { CCPoolStatusWidget() }
}

struct CCPoolStatusWidget: Widget {
    var body: some WidgetConfiguration {
        StaticConfiguration(kind: "CCPoolStatus", provider: StatusProvider()) { entry in
            StatusWidgetView(entry: entry)
                .containerBackground(for: .widget) {
                    MoodWashBackground(state: entry.state)
                }
        }
        .configurationDisplayName("cc-pool")
        .description("Per-account usage of your pooled Claude subscriptions.")
        .supportedFamilies([.systemSmall, .systemMedium, .systemLarge])
    }
}

/// The widget background: the system tertiary fill with a faint radial wash
/// of the mascot's mood color. Lives inside containerBackground — macOS 14
/// widgets must not paint their own full-bleed background — and stays subtle
/// (≤ 0.18 opacity) so secondary/tertiary text keeps contrast in both
/// appearances. Dimmed alongside the content when the snapshot is stale.
private struct MoodWashBackground: View {
    let state: StatusEntry.State

    var body: some View {
        ZStack {
            Rectangle().fill(.fill.tertiary)
            if case .ok(let status, let stale) = state, let outlook = status.outlook {
                RadialGradient(colors: [outlook.mood.tint.opacity(0.18), .clear],
                               center: .topLeading, startRadius: 0, endRadius: 240)
                    .opacity(stale ? 0.55 : 1)
            }
        }
    }
}
