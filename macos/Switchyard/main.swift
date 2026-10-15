// SPDX-License-Identifier: GPL-3.0-or-later
//
// Switchyard's macOS menu-bar app. It has no Dock icon (LSUIElement) and no
// logic of its own: it runs the swyd binary bundled beside it
// (Contents/MacOS/swyd) to read the state (`swyd service status --json`)
// and to install, start, stop and restart the per-user LaunchAgents. Nothing
// runs as root.
//
// Build: build-aux/macos/build-dmg.sh (swiftc, no Xcode project).

import AppKit
import ServiceManagement

// MARK: - `swyd service status --json`

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
    private let worker = DispatchQueue(label: "io.github.jm2.Switchyard.swyd")

    private var swydURL: URL {
        Bundle.main.bundleURL.appendingPathComponent("Contents/MacOS/swyd")
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

    // MARK: Running swyd

    private func runSwyd(_ args: [String]) -> CommandResult {
        let process = Process()
        process.executableURL = swydURL
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
            let result = self.runSwyd(["service", "status", "--json"])
            var decoded: Overview?
            var problem: String?
            if result.status == 0 {
                let decoder = JSONDecoder()
                decoder.keyDecodingStrategy = .convertFromSnakeCase
                do {
                    decoded = try decoder.decode(Overview.self, from: Data(result.output.utf8))
                } catch {
                    problem = "Unreadable status from swyd: \(error.localizedDescription)"
                }
            } else {
                problem = result.error.isEmpty ? "swyd failed (\(result.status))" : result.error
            }
            DispatchQueue.main.async {
                self.overview = decoded
                self.lastError = problem
                self.rebuildMenu()
            }
        }
    }

    /// Runs `swyd args` off the main thread, then refreshes; shows an alert
    /// if it fails.
    private func runAction(_ label: String, _ args: [String]) {
        guard busy == nil else { return }
        busy = label
        rebuildMenu()
        worker.async {
            let result = self.runSwyd(args)
            DispatchQueue.main.async {
                self.busy = nil
                if result.status != 0 {
                    self.showAlert("Switchyard could not finish \(label)",
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
        if let image = NSImage(systemSymbolName: symbol, accessibilityDescription: "Switchyard") {
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
            return role == "node" ? [infoItem("Enroll with: swyd node enroll <token>")] : []
        }
        if !info.installed {
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
            menu.addItem(infoItem(lastError.map { "Switchyard: \($0)" } ?? "Switchyard: reading status…"))
            menu.addItem(.separator())
            menu.addItem(ActionItem("Quit Switchyard") { NSApp.terminate(nil) })
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
        let ui = o.webUi.flatMap { URL(string: $0) }
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
        let login = ActionItem("Open at Login") { [weak self] in self?.toggleLoginItem() }
        login.state = SMAppService.mainApp.status == .enabled ? .on : .off
        menu.addItem(login)
        menu.addItem(.separator())
        menu.addItem(infoItem("Switchyard \(o.version)"))
        menu.addItem(ActionItem("Quit Switchyard") { NSApp.terminate(nil) })
    }

    private func toggleLoginItem() {
        do {
            if SMAppService.mainApp.status == .enabled {
                try SMAppService.mainApp.unregister()
            } else {
                try SMAppService.mainApp.register()
            }
        } catch {
            showAlert("Switchyard could not change its login item", error.localizedDescription)
        }
        rebuildMenu()
    }
}

// One menu-bar icon per user session.
let bundleID = Bundle.main.bundleIdentifier ?? "io.github.jm2.Switchyard"
if NSRunningApplication.runningApplications(withBundleIdentifier: bundleID).count > 1 {
    exit(0)
}

let app = NSApplication.shared
let delegate = AppDelegate()
app.delegate = delegate
app.setActivationPolicy(.accessory)
app.run()
