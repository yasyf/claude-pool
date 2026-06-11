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
                .containerBackground(.fill.tertiary, for: .widget)
        }
        .configurationDisplayName("cc-pool")
        .description("Per-account usage of your pooled Claude subscriptions.")
        .supportedFamilies([.systemSmall, .systemMedium])
    }
}
