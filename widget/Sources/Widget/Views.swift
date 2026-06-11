import SwiftUI
import WidgetKit

struct StatusWidgetView: View {
    @Environment(\.widgetFamily) private var family
    let entry: StatusEntry

    var body: some View {
        switch entry.state {
        case .noFile:
            MessageView(symbol: "moon.zzz", title: "daemon not running",
                        detail: "ccp service install")
        case .denied:
            MessageView(symbol: "lock.slash", title: "can't read ~/.cc-pool",
                        detail: "check widget entitlements")
        case .unreadable:
            MessageView(symbol: "exclamationmark.triangle", title: "status unreadable",
                        detail: "version skew? run ccp doctor")
        case .ok(let status, let stale):
            if status.accounts.isEmpty {
                MessageView(symbol: "person.crop.circle.badge.plus", title: "no accounts",
                            detail: "run ccp add")
            } else {
                switch family {
                case .systemSmall:
                    SmallView(status: status, stale: stale, seed: entry.date)
                case .systemLarge:
                    PoolBoardView(status: status, stale: stale, seed: entry.date,
                                  maxRows: 5, style: .detailed)
                default:
                    PoolBoardView(status: status, stale: stale, seed: entry.date,
                                  maxRows: 3, style: .compact)
                }
            }
        }
    }
}

// MARK: - Medium & large: pool header + ranked account list

/// Row density per family: medium rows are name + bars; large rows add the
/// resets/overage detail line.
enum RowStyle { case compact, detailed }

/// The medium/large body: mascot header over the ranked list. The stale dim
/// wraps both so a dead daemon mutes the whole board, mascot included.
struct PoolBoardView: View {
    let status: PoolStatus
    let stale: Bool
    let seed: Date
    let maxRows: Int
    let style: RowStyle

    var body: some View {
        VStack(alignment: .leading, spacing: style == .detailed ? 6 : 4) {
            PoolHeaderView(outlook: status.outlook,
                           critterSize: style == .detailed ? 44 : 26,
                           detailed: style == .detailed, seed: seed)
            AccountListView(status: status, stale: stale, maxRows: maxRows, style: style)
        }
        .opacity(stale ? 0.55 : 1)
    }
}

/// Mascot + pool headline. Compact: one line with the caption right-aligned;
/// detailed: bigger critter and a 7d/burn second line.
struct PoolHeaderView: View {
    let outlook: PoolOutlook?
    let critterSize: CGFloat
    let detailed: Bool
    let seed: Date

    var body: some View {
        HStack(spacing: 8) {
            CritterView(mood: outlook?.mood ?? .chill, size: critterSize, seed: seed)
            if let outlook {
                VStack(alignment: .leading, spacing: 1) {
                    Text(headline(outlook))
                        .font(.system(size: detailed ? 13 : 12, weight: .semibold))
                        .monospacedDigit()
                        .lineLimit(1)
                    if detailed {
                        Text(secondLine(outlook))
                            .font(.system(size: 10))
                            .monospacedDigit()
                            .foregroundStyle(.secondary)
                            .lineLimit(1)
                    }
                }
                Spacer(minLength: 4)
                if !detailed {
                    Text(outlook.caption())
                        .font(.caption2)
                        .monospacedDigit()
                        .foregroundStyle(.secondary)
                        .lineLimit(1)
                        .layoutPriority(1)
                }
            } else {
                Text("no usage data yet")
                    .font(.system(size: 12, weight: .semibold))
                    .foregroundStyle(.secondary)
                Spacer(minLength: 0)
            }
        }
    }

    private func headline(_ o: PoolOutlook) -> String {
        let pct = "Pool \(Int(o.remaining5hPct.rounded()))%"
        guard detailed else { return pct }
        // The compact caption moves into the headline on the large widget;
        // burn lives on the second line, so only the dry clock joins here.
        if let dry = o.dryAt, dry > .now {
            return pct + " · dry ~" + dry.formatted(date: .omitted, time: .shortened)
        }
        return o.dryAt == nil ? pct : pct + " · drying now"
    }

    private func secondLine(_ o: PoolOutlook) -> String {
        var parts = ["7d \(Int(o.remaining7dPct.rounded()))%"]
        if o.burn5hPerHour >= 1 {
            parts.append("burn \(Int(o.burn5hPerHour.rounded()))%/h")
        }
        return parts.joined(separator: " · ")
    }
}

/// Ranked account list filling the widget. Every row gets an equal flexible
/// slot (maxHeight .infinity) so surplus height spreads across rows instead
/// of pooling above the footer; with fewer accounts each row simply breathes.
struct AccountListView: View {
    let status: PoolStatus
    let stale: Bool
    let maxRows: Int
    let style: RowStyle

    var body: some View {
        let ranked = status.accounts.ranked
        VStack(alignment: .leading, spacing: 0) {
            ForEach(Array(ranked.prefix(maxRows).enumerated()), id: \.element.id) { rank, account in
                AccountRow(account: account, style: style, rank: rank)
                    .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .leading)
            }
            FooterView(generatedAt: status.generatedAt, stale: stale,
                       overflow: max(0, ranked.count - maxRows))
                .padding(.top, 2)
        }
    }
}

struct AccountRow: View {
    let account: AccountStatus
    let style: RowStyle
    let rank: Int

    /// Lines below the name align past the live dot: dot (7) + spacing (5).
    private static let indent: CGFloat = 12

    var body: some View {
        HStack(alignment: .top, spacing: 6) {
            RoundedRectangle(cornerRadius: 1.5)
                .fill(rankAccent(rank))
                .frame(width: 3)
                .frame(maxHeight: .infinity)
                .padding(.vertical, 1)
            VStack(alignment: .leading, spacing: 2) {
                HStack(spacing: 5) {
                    LiveDot(count: account.activeSessions)
                    Text(account.displayName)
                        .font(.system(size: 12, weight: .medium))
                        .lineLimit(1)
                        .truncationMode(.middle)
                    Spacer(minLength: 4)
                    BadgeRow(account: account, style: style)
                        .layoutPriority(1) // badges hug; the name truncates first
                }
                Group {
                    if account.hasUsage {
                        HStack(spacing: 10) {
                            UsageCell(window: "5h", remaining: account.remaining5h,
                                      alert: account.unusable, kind: .fiveHour)
                            UsageCell(window: "7d", remaining: account.remaining7d,
                                      alert: account.unusable, kind: .sevenDay)
                        }
                    } else {
                        Text("no data")
                            .font(.system(size: 10))
                            .foregroundStyle(.tertiary)
                    }
                }
                .padding(.leading, Self.indent)
                if style == .detailed, let detail {
                    Text(detail)
                        .font(.system(size: 10))
                        .monospacedDigit()
                        .foregroundStyle(.tertiary)
                        .lineLimit(1)
                        .padding(.leading, Self.indent)
                }
            }
        }
    }

    /// Detail line: burn projection first (the forward-looking fact), then
    /// the CLI's RESETS column for both windows plus overage spend.
    private var detail: String? {
        var parts: [String] = []
        if let prediction = account.predictionText(detailed: true) {
            parts.append(prediction)
        }
        if let reset = account.resets5h?.nonZero {
            parts.append("5h resets " + reset.formatted(date: .omitted, time: .shortened))
        }
        if let reset = account.resets7d?.nonZero {
            parts.append("7d resets " + reset.formatted(.dateTime.weekday(.abbreviated).hour().minute()))
        }
        if account.hasOverage {
            let used = (account.extraUsed ?? 0) / 100
            if let limit = account.extraLimit, limit > 0 {
                parts.append(String(format: "extra $%.2f of $%.2f", used, limit / 100))
            } else {
                parts.append(String(format: "extra $%.2f", used))
            }
        }
        return parts.isEmpty ? nil : parts.joined(separator: " · ")
    }
}

// MARK: - Small: the pool at a glance (mascot + headline + next pick)

struct SmallView: View {
    let status: PoolStatus
    let stale: Bool
    let seed: Date

    var body: some View {
        let ranked = status.accounts.ranked
        let best = ranked[0]
        let liveTotal = status.accounts.reduce(0) { $0 + $1.activeSessions }
        VStack(alignment: .leading, spacing: 6) {
            HStack(spacing: 8) {
                CritterView(mood: status.outlook?.mood ?? .chill, size: 54, seed: seed)
                VStack(alignment: .leading, spacing: 0) {
                    if let outlook = status.outlook {
                        Text("\(Int(outlook.remaining5hPct.rounded()))%")
                            .font(.system(size: 26, weight: .bold, design: .rounded))
                            .monospacedDigit()
                        Text(outlook.caption())
                            .font(.caption2)
                            .monospacedDigit()
                            .foregroundStyle(.secondary)
                            .lineLimit(1)
                            .minimumScaleFactor(0.8)
                    } else {
                        Text("—")
                            .font(.system(size: 26, weight: .bold, design: .rounded))
                            .foregroundStyle(.secondary)
                        Text("no data yet")
                            .font(.caption2)
                            .foregroundStyle(.tertiary)
                    }
                }
            }
            VStack(alignment: .leading, spacing: 2) {
                HStack(spacing: 5) {
                    LiveDot(count: liveTotal)
                    Text(best.displayName)
                        .font(.system(size: 11, weight: .medium))
                        .lineLimit(1)
                        .truncationMode(.middle)
                    Spacer(minLength: 4)
                    if let prediction = best.predictionText() {
                        Text(prediction)
                            .font(.system(size: 9))
                            .monospacedDigit()
                            .foregroundStyle(.secondary)
                            .layoutPriority(1)
                    }
                }
                if best.hasUsage {
                    HeadroomBar(remaining: min(max(best.remaining5h, 0), 100),
                                alert: best.unusable, kind: .fiveHour)
                } else {
                    Text("no data yet")
                        .font(.caption2)
                        .foregroundStyle(.tertiary)
                }
            }
            Spacer(minLength: 0)
            FooterView(generatedAt: status.generatedAt, stale: stale, overflow: 0)
        }
        .opacity(stale ? 0.55 : 1)
        .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .topLeading)
    }
}

// MARK: - Pieces

extension AccountStatus {
    var unusable: Bool { rateLimited || isExhausted }
}

/// Capsule gauge. The bar fills with % USED (matching the `ccp status`
/// columns); the gradient runs the window's identity hues, heating toward
/// amber/red as headroom drains.
struct HeadroomBar: View {
    let remaining: Double // clamped to 0...100 by the caller
    let alert: Bool
    let kind: WindowKind

    private static let height: CGFloat = 6

    var body: some View {
        let usedFraction = (100 - remaining) / 100
        GeometryReader { geo in
            ZStack(alignment: .leading) {
                Capsule().fill(.quaternary)
                if usedFraction > 0 {
                    // Floor at the capsule's own diameter so a barely-used
                    // account renders a full dot, not a degenerate sliver.
                    Capsule()
                        .fill(usageGradient(kind: kind, remaining: remaining, alert: alert))
                        .frame(width: max(Self.height, geo.size.width * usedFraction))
                }
            }
        }
        .frame(height: Self.height)
    }
}

/// "5h ▮▮▮░ 58%" cell: fixed label and percent columns around a flexible
/// bar, so bars align across rows and split leftover width with siblings.
struct UsageCell: View {
    let window: String
    let remaining: Double
    let alert: Bool
    let kind: WindowKind

    var body: some View {
        let clamped = min(max(remaining, 0), 100)
        HStack(spacing: 4) {
            Text(window)
                .font(.system(size: 10))
                .foregroundStyle(.tertiary)
                .frame(width: 16, alignment: .leading)
            HeadroomBar(remaining: clamped, alert: alert, kind: kind)
            Text("\(Int((100 - clamped).rounded()))%")
                .font(.system(size: 10, weight: .medium, design: .monospaced))
                .foregroundStyle(.secondary)
                .frame(width: 30, alignment: .trailing)
        }
        .frame(maxWidth: .infinity)
    }
}

struct LiveDot: View {
    let count: Int

    var body: some View {
        HStack(spacing: 2) {
            Circle()
                .fill(count > 0 ? Color.green : Color.secondary.opacity(0.35))
                .frame(width: 7, height: 7)
            if count > 0 {
                Text("\(count)")
                    .font(.system(size: 10, design: .monospaced))
                    .foregroundStyle(.secondary)
            }
        }
    }
}

/// The CLI's stale / rate-limited / exhausted / overage flags as compact
/// glyphs, plus the row's burn projection when one exists.
struct BadgeRow: View {
    let account: AccountStatus
    let style: RowStyle

    var body: some View {
        HStack(spacing: 3) {
            if account.stale {
                Image(systemName: "clock.badge.exclamationmark")
                    .font(.system(size: 10))
                    .foregroundStyle(.orange)
            }
            if account.rateLimited {
                Image(systemName: "exclamationmark.octagon.fill")
                    .font(.system(size: 10))
                    .foregroundStyle(.red)
            }
            if account.isExhausted {
                Image(systemName: "hourglass.bottomhalf.filled")
                    .font(.system(size: 10))
                    .foregroundStyle(.red)
            }
            // Detailed rows carry overage, reset times, and the projection on
            // their own detail line; repeating them here would double-print.
            if style == .compact {
                if account.hasOverage {
                    Text(String(format: "$%.2f", (account.extraUsed ?? 0) / 100))
                        .font(.system(size: 10, design: .monospaced))
                        .foregroundStyle(.orange)
                }
                // When the account can't serve, the time it recovers is the one
                // fact that matters — the CLI's RESETS column, shown on demand.
                // predictionText is nil for unusable rows, so the slot is shared,
                // never crowded.
                if account.unusable, let reset = account.resets5h?.nonZero {
                    Text(reset, format: .dateTime.hour().minute())
                        .font(.system(size: 10))
                        .foregroundStyle(.secondary)
                } else if let prediction = account.predictionText() {
                    Text(prediction)
                        .font(.system(size: 10))
                        .monospacedDigit()
                        .foregroundStyle(.secondary)
                }
            }
        }
    }
}

struct FooterView: View {
    let generatedAt: Date
    let stale: Bool
    let overflow: Int

    var body: some View {
        HStack(spacing: 4) {
            if overflow > 0 {
                Text("+\(overflow) more")
                    .font(.caption2)
                    .foregroundStyle(.tertiary)
            }
            Spacer(minLength: 0)
            if stale {
                Text("stale · ")
                    .font(.caption2)
                    .foregroundStyle(.orange)
                + Text(generatedAt, style: .relative)
                    .font(.caption2)
                    .foregroundStyle(.orange)
                + Text(" ago")
                    .font(.caption2)
                    .foregroundStyle(.orange)
            } else {
                Text("updated ")
                    .font(.caption2)
                    .foregroundStyle(.tertiary)
                + Text(generatedAt, style: .relative)
                    .font(.caption2)
                    .foregroundStyle(.tertiary)
                + Text(" ago")
                    .font(.caption2)
                    .foregroundStyle(.tertiary)
            }
        }
    }
}

struct MessageView: View {
    let symbol: String
    let title: String
    let detail: String

    var body: some View {
        VStack(spacing: 4) {
            Image(systemName: symbol)
                .font(.title3)
                .foregroundStyle(.secondary)
            Text(title)
                .font(.caption.weight(.semibold))
            Text(detail)
                .font(.caption2.monospaced())
                .foregroundStyle(.secondary)
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity)
    }
}

// MARK: - Previews

#Preview("medium", as: .systemMedium) {
    CCPoolStatusWidget()
} timeline: {
    StatusEntry(date: .now, state: .ok(.sample, stale: false))
    StatusEntry(date: .now, state: .ok(.sampleLegacy, stale: false))
    StatusEntry(date: .now, state: .ok(.sample, stale: true))
    StatusEntry(date: .now, state: .noFile)
}

#Preview("small", as: .systemSmall) {
    CCPoolStatusWidget()
} timeline: {
    StatusEntry(date: .now, state: .ok(.sample, stale: false))
    StatusEntry(date: .now, state: .ok(.sampleLegacy, stale: false))
    StatusEntry(date: .now, state: .unreadable)
}

#Preview("large", as: .systemLarge) {
    CCPoolStatusWidget()
} timeline: {
    StatusEntry(date: .now, state: .ok(.sample, stale: false))
    StatusEntry(date: .now, state: .ok(.sample, stale: true))
}

#Preview("moods", as: .systemMedium) {
    CCPoolStatusWidget()
} timeline: {
    StatusEntry(date: .now, state: .ok(.sample(mood: .chill), stale: false))
    StatusEntry(date: .now, state: .ok(.sample(mood: .easy), stale: false))
    StatusEntry(date: .now, state: .ok(.sample(mood: .uneasy), stale: false))
    StatusEntry(date: .now, state: .ok(.sample(mood: .worried), stale: false))
    StatusEntry(date: .now, state: .ok(.sample(mood: .alarmed), stale: false))
    StatusEntry(date: .now, state: .ok(.sample(mood: .panic), stale: false))
}
