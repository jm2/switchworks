// SPDX-License-Identifier: GPL-3.0-or-later
//
// Switchworks's macOS menu-bar app. It has no Dock icon (LSUIElement) and no
// logic of its own: it runs the swk binary bundled beside it
// (Contents/MacOS/swk) to read the state (`swk service status --json`)
// and to install, start, stop and restart the per-user LaunchAgents. Nothing
// runs as root.
//
// Build: build-aux/macos/build-dmg.sh (swiftc, no Xcode project).

import AppKit
import ServiceManagement

// MARK: - `swk service status --json`

struct RunStatus: Decodable {
    let host: String?
    let coordinator: String?
    let connected: Bool?
    let lastError: String?
    let webUi: String?
}

struct RoleInfo: Decodable {
    let role: String
    let manager: String
    let unit: String
    let configured: Bool
    let installed: Bool
    let enabled: Bool
    let running: Bool
    let status: RunStatus?
    let dir: String
    let logDir: String
    let error: String?
}

struct Overview: Decodable {
    let version: String
    let node: RoleInfo
    let coordinator: RoleInfo
    let webUi: String?
    let configDir: String
}

// MARK: - Menu items that run a closure

final class ActionItem: NSMenuItem {
    private let handler: () -> Void

    init(_ title: String, enabled: Bool = true, handler: @escaping () -> Void) {
        self.handler = handler
        super.init(title: title, action: #selector(fire), keyEquivalent: "")
        target = self
        isEnabled = enabled
    }

    required init(coder: NSCoder) {
        fatalError("init(coder:) is not supported")
    }

    @objc private func fire() {
        handler()
    }
}

func infoItem(_ title: String) -> NSMenuItem {
    let item = NSMenuItem(title: title, action: nil, keyEquivalent: "")
    item.isEnabled = false
    return item
}

// MARK: - App

enum IconState {
    case idle, ok, warn
}

struct CommandResult {
    let status: Int32
    let output: String
    let error: String
}

final class AppDelegate: NSObject, NSApplicationDelegate, NSMenuDelegate {
    private var statusItem: NSStatusItem!
    private let menu = NSMenu()
    private var overview: Overview?
    private var lastError: String?
    private var busy: String?
    private var timer: Timer?
    private let worker = DispatchQueue(label: "io.github.jm2.Switchworks.swk")

    private var swkURL: URL {
        Bundle.main.bundleURL.appendingPathComponent("Contents/MacOS/swk")
    }

    func applicationDidFinishLaunching(_ notification: Notification) {
        statusItem = NSStatusBar.system.statusItem(withLength: NSStatusItem.variableLength)
        menu.autoenablesItems = false
        menu.delegate = self
        statusItem.menu = menu
        setIcon(.idle)
        rebuildMenu()
        refresh()
        timer = Timer.scheduledTimer(withTimeInterval: 5, repeats: true) { [weak self] _ in
            self?.refresh()
        }
    }

    func menuWillOpen(_ menu: NSMenu) {
        refresh()
    }

    // MARK: Running swk

    private func runSwk(_ args: [String]) -> CommandResult {
        let process = Process()
        process.executableURL = swkURL
        process.arguments = args
        let out = Pipe()
        let err = Pipe()
        process.standardOutput = out
        process.standardError = err
        do {
            try process.run()
        } catch {
            return CommandResult(status: -1, output: "", error: error.localizedDescription)
        }
        // Drain stderr while stdout is read, so a chatty child cannot block
        // on a full pipe.
        var errData = Data()
        let errDone = DispatchSemaphore(value: 0)
        DispatchQueue.global().async {
            errData = err.fileHandleForReading.readDataToEndOfFile()
            errDone.signal()
        }
        let outData = out.fileHandleForReading.readDataToEndOfFile()
        errDone.wait()
        process.waitUntilExit()
        return CommandResult(status: process.terminationStatus,
                             output: String(decoding: outData, as: UTF8.self),
                             error: String(decoding: errData, as: UTF8.self))
    }

    private func refresh() {
        worker.async {
            let result = self.runSwk(["service", "status", "--json"])
            var decoded: Overview?
            var problem: String?
            if result.status == 0 {
                let decoder = JSONDecoder()
                decoder.keyDecodingStrategy = .convertFromSnakeCase
                do {
                    decoded = try decoder.decode(Overview.self, from: Data(result.output.utf8))
                } catch {
                    problem = "Unreadable status from swk: \(error.localizedDescription)"
                }
            } else {
                problem = result.error.isEmpty ? "swk failed (\(result.status))" : result.error
            }
            DispatchQueue.main.async {
                self.overview = decoded
                self.lastError = problem
                self.rebuildMenu()
            }
        }
    }

    /// Runs `swk args` off the main thread, then refreshes; shows an alert
    /// if it fails.
    private func runAction(_ label: String, _ args: [String]) {
        guard busy == nil else { return }
        busy = label
        rebuildMenu()
        worker.async {
            let result = self.runSwk(args)
            DispatchQueue.main.async {
                self.busy = nil
                if result.status != 0 {
                    self.showAlert("Switchworks could not finish \(label)",
                                   result.error.isEmpty ? result.output : result.error)
                }
                self.refresh()
            }
        }
    }

    private func showAlert(_ title: String, _ text: String) {
        NSApp.activate(ignoringOtherApps: true)
        let alert = NSAlert()
        alert.messageText = title
        alert.informativeText = text.trimmingCharacters(in: .whitespacesAndNewlines)
        alert.alertStyle = .warning
        alert.runModal()
    }

    // MARK: Icon and menu

    private func setIcon(_ state: IconState) {
        guard let button = statusItem.button else { return }
        let symbol: String
        switch state {
        case .ok: symbol = "arrow.triangle.branch"
        case .warn: symbol = "exclamationmark.triangle"
        case .idle: symbol = "circle.dashed"
        }
        if let image = NSImage(systemSymbolName: symbol, accessibilityDescription: "Switchworks") {
            image.isTemplate = true
            button.image = image
            button.title = ""
        } else {
            button.image = nil
            button.title = "SY"
        }
    }

    private func iconState(_ o: Overview) -> IconState {
        if o.node.configured {
            return (o.node.status?.connected ?? false) ? .ok : .warn
        }
        if o.coordinator.configured {
            return o.coordinator.running ? .ok : .warn
        }
        return .idle
    }

    private func hostOnly(_ hostPort: String) -> String {
        if hostPort.hasSuffix("]") { return hostPort }
        guard let idx = hostPort.lastIndex(of: ":") else { return hostPort }
        return String(hostPort[..<idx]).trimmingCharacters(in: CharacterSet(charactersIn: "[]"))
    }

    private func nodeLine(_ n: RoleInfo) -> String {
        if !n.configured { return "Node: not enrolled" }
        if let s = n.status {
            let host = s.host ?? "this Mac"
            let coord = hostOnly(s.coordinator ?? "")
            return (s.connected ?? false) ? "Node \(host): connected to \(coord)" : "Node \(host): connecting to \(coord)…"
        }
        return n.running ? "Node: running" : "Node: stopped"
    }

    private func roleItems(_ info: RoleInfo, _ title: String) -> [NSMenuItem] {
        let role = info.role
        let idle = busy == nil
        if !info.configured {
            return role == "node" ? [infoItem("Enroll with: swk node enroll <token>")] : []
        }
        if !info.installed {
            if !installedInApplications() {
                return [infoItem("Move Switchworks to Applications to run the \(title) at login")]
            }
            return [ActionItem("Run \(title) at login", enabled: idle) { [weak self] in
                self?.runAction("setting up the \(role) service", ["service", "install", role])
            }]
        }
        return [
            ActionItem("Start \(title)", enabled: idle && !info.running) { [weak self] in
                self?.runAction("starting the \(role)", ["service", "start", role])
            },
            ActionItem("Stop \(title)", enabled: idle && info.running) { [weak self] in
                self?.runAction("stopping the \(role)", ["service", "stop", role])
            },
            ActionItem("Restart \(title)", enabled: idle) { [weak self] in
                self?.runAction("restarting the \(role)", ["service", "restart", role])
            },
        ]
    }

    private func openFolder(_ path: String) {
        try? FileManager.default.createDirectory(atPath: path, withIntermediateDirectories: true,
                                                 attributes: [.posixPermissions: 0o700])
        NSWorkspace.shared.open(URL(fileURLWithPath: path, isDirectory: true))
    }

    private func rebuildMenu() {
        menu.removeAllItems()
        guard let o = overview else {
            setIcon(.warn)
            menu.addItem(infoItem(lastError.map { "Switchworks: \($0)" } ?? "Switchworks: reading status…"))
            menu.addItem(.separator())
            menu.addItem(ActionItem("Quit Switchworks") { NSApp.terminate(nil) })
            return
        }
        setIcon(iconState(o))
        menu.addItem(infoItem(nodeLine(o.node)))
        if o.coordinator.configured {
            menu.addItem(infoItem(o.coordinator.running ? "Coordinator: running" : "Coordinator: stopped"))
        }
        if let b = busy {
            menu.addItem(infoItem(b.prefix(1).uppercased() + String(b.dropFirst()) + "…"))
        }
        menu.addItem(.separator())
        // Only plain web addresses: never a file: URL or another app's scheme.
        let ui = o.webUi.flatMap { URL(string: $0) }.flatMap { u in
            (u.scheme == "https" || u.scheme == "http") && u.host != nil ? u : nil
        }
        menu.addItem(ActionItem("Open Web UI", enabled: ui != nil) {
            if let ui = ui { NSWorkspace.shared.open(ui) }
        })
        menu.addItem(.separator())
        roleItems(o.node, "node").forEach { menu.addItem($0) }
        if o.coordinator.configured {
            menu.addItem(.separator())
            roleItems(o.coordinator, "coordinator").forEach { menu.addItem($0) }
        }
        menu.addItem(.separator())
        let logs = (!o.node.configured && o.coordinator.configured) ? o.coordinator.logDir : o.node.logDir
        menu.addItem(ActionItem("Open Logs Folder") { [weak self] in self?.openFolder(logs) })
        menu.addItem(ActionItem("Open Config Folder") { [weak self] in self?.openFolder(o.configDir) })
        menu.addItem(.separator())
        let login = ActionItem("Open at Login", enabled: installedInApplications()) { [weak self] in self?.toggleLoginItem() }
        login.state = SMAppService.mainApp.status == .enabled ? .on : .off
        menu.addItem(login)
        menu.addItem(.separator())
        menu.addItem(infoItem("Switchworks \(o.version)"))
        menu.addItem(ActionItem("Quit Switchworks") { NSApp.terminate(nil) })
    }

    /// Login items and services must point at a copy that stays put: not
    /// the mounted disk image, and not an App Translocation copy of an app
    /// opened straight from Downloads.
    private func installedInApplications() -> Bool {
        let path = Bundle.main.bundlePath
        let home = FileManager.default.homeDirectoryForCurrentUser.path
        return path.hasPrefix("/Applications/") || path.hasPrefix(home + "/Applications/")
    }

    private func toggleLoginItem() {
        do {
            if SMAppService.mainApp.status == .enabled {
                try SMAppService.mainApp.unregister()
            } else {
                try SMAppService.mainApp.register()
            }
        } catch {
            showAlert("Switchworks could not change its login item", error.localizedDescription)
        }
        rebuildMenu()
    }
}

// MARK: - Running swk for the LaunchAgents

// The pid of the swk that launchSwk started, for the signal handlers, and a
// signal that arrived before it started.
var swkChild: pid_t = 0
var swkPendingSignal: Int32 = 0

// `Switchworks --swk <args>` runs the bundled swk with args as its child and
// exits with its status. `swk service install` has launchd start swk this
// way, so that the app is the responsible process: macOS asks about Local
// Network access for Switchworks, an app it knows (NSLocalNetworkUsageDescription
// in Info.plist), and remembers the answer. A helper binary that launchd
// starts directly is denied without a prompt. launchd's SIGTERM and a
// SIGINT are passed on, so swk stops its sessions cleanly.
func launchSwk(_ args: [String]) -> Never {
    // Before the child starts, so a stop request is never lost. The child
    // still gets the default handlers: exec resets caught signals.
    for sig in [SIGTERM, SIGINT] {
        signal(sig) { sig in
            if swkChild > 0 {
                kill(swkChild, sig)
            } else {
                swkPendingSignal = sig
            }
        }
    }
    let process = Process()
    process.executableURL = Bundle.main.bundleURL.appendingPathComponent("Contents/MacOS/swk")
    process.arguments = args
    do {
        try process.run()
    } catch {
        FileHandle.standardError.write("Switchworks: cannot run swk: \(error.localizedDescription)\n".data(using: .utf8)!)
        exit(1)
    }
    swkChild = process.processIdentifier
    if swkPendingSignal != 0 {
        kill(swkChild, swkPendingSignal)
    }
    process.waitUntilExit()
    if process.terminationReason == .uncaughtSignal {
        exit(128 + process.terminationStatus)
    }
    exit(process.terminationStatus)
}

if CommandLine.arguments.count > 1 && CommandLine.arguments[1] == "--swk" {
    launchSwk(Array(CommandLine.arguments.dropFirst(2)))
}

// One menu-bar icon per user session. Only another menu-bar app counts, not
// a `Switchworks --swk` running a service.
let bundleID = Bundle.main.bundleIdentifier ?? "io.github.jm2.Switchworks"
let others = NSRunningApplication.runningApplications(withBundleIdentifier: bundleID).filter {
    $0.processIdentifier != getpid() && $0.activationPolicy == .accessory
}
if !others.isEmpty {
    exit(0)
}

let app = NSApplication.shared
let delegate = AppDelegate()
app.delegate = delegate
app.setActivationPolicy(.accessory)
app.run()
