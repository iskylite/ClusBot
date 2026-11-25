import AppKit
import UserNotifications
import WebKit

private struct ServiceSession: Decodable {
    let mode: String
}

private final class DesktopWebView: WKWebView {
    var lastMouseDownEvent: NSEvent?

    override func mouseDown(with event: NSEvent) {
        lastMouseDownEvent = event
        super.mouseDown(with: event)
    }
}

@MainActor
final class ClusBot: NSObject, NSApplicationDelegate, NSWindowDelegate, WKNavigationDelegate, WKUIDelegate, WKDownloadDelegate, WKScriptMessageHandler, WKScriptMessageHandlerWithReply, UNUserNotificationCenterDelegate {
    private let service = Process()
    private let output = Pipe()
    private var window: NSWindow!
    private let windowControlsToolbar = NSToolbar(identifier: "WindowControls")
    private var webView: DesktopWebView!
    private var origin: URL?
    private var manualZoom: CGFloat?
    private var closing = false
    private var failureShown = false
    private var drafts: [String: String] = [:]
    private var draftFile: URL?
    private var downloads: [ObjectIdentifier: (folder: URL, staged: URL, destination: URL)] = [:]
    private var csvBlobDownloads: Set<ObjectIdentifier> = []

    func applicationDidFinishLaunching(_ notification: Notification) {
        let menu = NSMenu()
        let appItem = NSMenuItem()
        let appMenu = NSMenu()
        appMenu.addItem(withTitle: "关于 ClusBot", action: #selector(NSApplication.orderFrontStandardAboutPanel(_:)), keyEquivalent: "")
        appMenu.addItem(.separator())
        appMenu.addItem(withTitle: "退出 ClusBot", action: #selector(NSApplication.terminate(_:)), keyEquivalent: "q")
        appItem.submenu = appMenu
        menu.addItem(appItem)
        let editItem = NSMenuItem()
        let editMenu = NSMenu(title: "编辑")
        for (title, action, key) in [("剪切", "cut:", "x"), ("复制", "copy:", "c"),
                                     ("粘贴", "paste:", "v"), ("全选", "selectAll:", "a")] {
            editMenu.addItem(withTitle: title, action: Selector(action), keyEquivalent: key)
        }
        editItem.submenu = editMenu
        menu.addItem(editItem)
        let viewItem = NSMenuItem()
        let viewMenu = NSMenu(title: "显示")
        let sidebarItem = viewMenu.addItem(withTitle: "显示或隐藏侧边栏", action: #selector(toggleSidebar), keyEquivalent: "s")
        sidebarItem.keyEquivalentModifierMask = [.control, .command]
        sidebarItem.target = self
        viewMenu.addItem(.separator())
        for (title, action, key) in [("放大", #selector(zoomIn), "+"),
                                     ("缩小", #selector(zoomOut), "-"),
                                     ("适应窗口", #selector(fitWindow), "0")] {
            let item = viewMenu.addItem(withTitle: title, action: action, keyEquivalent: key)
            item.target = self
        }
        viewItem.submenu = viewMenu
        menu.addItem(viewItem)
        NSApp.mainMenu = menu

        let dataDir = FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask)[0]
            .appendingPathComponent("ClusBot", isDirectory: true)
        let file = dataDir.appendingPathComponent("desktop-drafts.json")
        draftFile = file
        if let data = try? Data(contentsOf: file),
           let saved = try? JSONDecoder().decode([String: String].self, from: data) {
            drafts = saved
        }
        let config = WKWebViewConfiguration()
        installDraftSnapshot(in: config.userContentController)
        config.userContentController.add(self, name: "drafts")
        config.userContentController.add(self, name: "clusbotWindow")
        config.userContentController.addScriptMessageHandler(self, contentWorld: .page, name: "clusbotNotifications")
        UNUserNotificationCenter.current().delegate = self
        config.websiteDataStore = .nonPersistent()
        webView = DesktopWebView(frame: .zero, configuration: config)
        webView.navigationDelegate = self
        webView.uiDelegate = self

        window = NSWindow(
            contentRect: NSRect(x: 0, y: 0, width: 1280, height: 820),
            styleMask: [.titled, .closable, .miniaturizable, .resizable, .fullSizeContentView],
            backing: .buffered, defer: false
        )
        window.title = "ClusBot"
        window.titleVisibility = .hidden
        window.titlebarAppearsTransparent = true
        window.toolbar = windowControlsToolbar
        window.toolbarStyle = .unified
        window.titlebarSeparatorStyle = .none
        window.contentMinSize = NSSize(width: 760, height: 480)
        window.delegate = self
        window.contentView = webView
        updateZoom()
        window.center()
        window.makeKeyAndOrderFront(nil)
        NSApp.activate(ignoringOtherApps: true)

        service.executableURL = Bundle.main.url(forAuxiliaryExecutable: "clusbot-service")
        var serviceArguments = ["serve", "--mode", "desktop", "--listen", "127.0.0.1:0", "--data-dir", dataDir.path, "--print-url"]
        let serviceConfig = dataDir.appendingPathComponent("config.yaml")
        if FileManager.default.fileExists(atPath: serviceConfig.path) {
            serviceArguments += ["--config", serviceConfig.path]
        }
        service.arguments = serviceArguments
        service.standardOutput = output
        service.terminationHandler = { [weak self] process in
            let status = process.terminationStatus
            Task { @MainActor in
                guard let self else { return }
                if self.closing {
                    NSApp.reply(toApplicationShouldTerminate: true)
                } else {
                    self.fail("本地服务已退出（状态码 \(status)）。请检查是否已打开另一个 ClusBot 实例。")
                }
            }
        }

        do {
            try service.run()
            try output.fileHandleForWriting.close()
        } catch {
            fail(error.localizedDescription)
            return
        }

        let reader = output.fileHandleForReading
        Task.detached { [weak self] in
            defer { try? reader.close() }
            var line = Data()
            while let byte = try? reader.read(upToCount: 1), !byte.isEmpty, line.count < 128 {
                if byte.first == 10 { break }
                line.append(byte)
            }
            await self?.connect(line, dataDir: dataDir)
        }
        DispatchQueue.main.asyncAfter(deadline: .now() + 20) { [weak self] in
            guard let self, self.origin == nil else { return }
            self.fail("等待本地服务启动超时。")
        }
    }

    func windowDidResize(_ notification: Notification) {
        updateZoom()
    }

    func windowWillEnterFullScreen(_ notification: Notification) {
        window.toolbar = nil
    }

    func windowDidFailToEnterFullScreen(_ window: NSWindow) {
        restoreWindowControls()
    }

    func windowDidExitFullScreen(_ notification: Notification) {
        restoreWindowControls()
    }

    private func restoreWindowControls() {
        window.toolbar = windowControlsToolbar
        window.titlebarSeparatorStyle = .none
    }

    @objc private func zoomIn() {
        manualZoom = min(1.15, webView.pageZoom + 0.1)
        updateZoom()
    }

    @objc private func toggleSidebar() {
        webView.evaluateJavaScript("""
            (() => {
            const visible = button => button && button.getClientRects().length > 0;
            const target = Array.from(document.querySelectorAll('button.sidebar-collapse, button.app-mobile-nav')).find(visible);
            if (target) target.click();
            })();
            """, completionHandler: nil)
    }

    @objc private func zoomOut() {
        manualZoom = max(0.75, webView.pageZoom - 0.1)
        updateZoom()
    }

    @objc private func fitWindow() {
        manualZoom = nil
        updateZoom()
    }

    private func updateZoom() {
        webView.pageZoom = manualZoom ?? 1
    }

    private func connect(_ line: Data, dataDir: URL) async {
        guard !closing, !failureShown else { return }
        guard let value = String(data: line, encoding: .utf8),
              let url = URL(string: value), url.scheme == "http",
              url.host == "127.0.0.1", let port = url.port, (1...65535).contains(port),
              url.absoluteString == "http://127.0.0.1:\(port)" else {
            fail("未收到本地服务的启动地址。")
            return
        }
        do {
            let session = URLSession(configuration: .ephemeral)
            defer { session.finishTasksAndInvalidate() }
            let statusURL = url.appendingPathComponent("api/v1/session")
            let (data, response) = try await session.data(for: URLRequest(url: statusURL, timeoutInterval: 10))
            guard let response = response as? HTTPURLResponse, response.statusCode == 200,
                  let status = try? JSONDecoder().decode(ServiceSession.self, from: data),
                  status.mode == "local" || status.mode == "server" else {
                fail("无法读取本地服务的会话状态。")
                return
            }
            if status.mode == "local" {
                let token = try String(contentsOf: dataDir.appendingPathComponent("access-token"), encoding: .utf8)
                    .trimmingCharacters(in: .whitespacesAndNewlines)
                var request = URLRequest(url: statusURL, timeoutInterval: 10)
                request.httpMethod = "POST"
                request.setValue(url.absoluteString, forHTTPHeaderField: "Origin")
                request.setValue("application/json", forHTTPHeaderField: "Content-Type")
                request.httpBody = try JSONEncoder().encode(["token": token])
                let (_, loginResponse) = try await session.data(for: request)
                guard let loginResponse = loginResponse as? HTTPURLResponse, loginResponse.statusCode == 200,
                      let cookie = session.configuration.httpCookieStorage?.cookies(for: url)?
                        .first(where: { $0.name == "clusbot_session" }) else {
                    fail("无法建立原有本机会话。")
                    return
                }
                await webView.configuration.websiteDataStore.httpCookieStore.setCookie(cookie)
            }
            guard !closing, !failureShown else { return }
            origin = url
            webView.load(URLRequest(url: url))
        } catch {
            fail(error.localizedDescription)
        }
    }

    func webView(_ webView: WKWebView, runJavaScriptConfirmPanelWithMessage message: String,
                 initiatedByFrame frame: WKFrameInfo, completionHandler: @escaping (Bool) -> Void) {
        guard frame.isMainFrame, sameOrigin(frame.request.url), !closing, window.attachedSheet == nil else {
            completionHandler(false)
            return
        }
        let alert = NSAlert()
        alert.alertStyle = .warning
        alert.messageText = "请确认操作"
        alert.informativeText = message
        alert.addButton(withTitle: "取消")
        alert.addButton(withTitle: "确认")
        alert.beginSheetModal(for: window) { response in
            completionHandler(response == .alertSecondButtonReturn)
        }
    }

    func webView(_ webView: WKWebView, decidePolicyFor action: WKNavigationAction) async -> WKNavigationActionPolicy {
        if isCSVBlobDownload(action) { return .download }
        guard sameOrigin(action.request.url), action.targetFrame != nil else { return .cancel }
        return action.shouldPerformDownload ? .download : .allow
    }

    private func sameOrigin(_ url: URL?) -> Bool {
        guard let url, let origin else { return false }
        return url.scheme == origin.scheme && url.host == origin.host && url.port == origin.port
    }

    private func sameOriginBlob(_ url: URL?) -> Bool {
        guard let value = url?.absoluteString, value.hasPrefix("blob:"),
              let inner = URL(string: String(value.dropFirst(5))) else { return false }
        return sameOrigin(inner)
    }

    private func isCSVBlobDownload(_ action: WKNavigationAction) -> Bool {
        action.shouldPerformDownload && action.sourceFrame.isMainFrame &&
            sameOrigin(action.sourceFrame.request.url) && sameOriginBlob(action.request.url)
    }

    func webView(_ webView: WKWebView, decidePolicyFor response: WKNavigationResponse) async -> WKNavigationResponsePolicy {
        guard sameOrigin(response.response.url) else { return .cancel }
        if let http = response.response as? HTTPURLResponse,
           http.value(forHTTPHeaderField: "Content-Disposition")?
            .split(separator: ";").first?.trimmingCharacters(in: .whitespaces).lowercased() == "attachment" {
            return .download
        }
        return .allow
    }

    func webView(_ webView: WKWebView, navigationAction: WKNavigationAction, didBecome download: WKDownload) {
        if isCSVBlobDownload(navigationAction) { csvBlobDownloads.insert(ObjectIdentifier(download)) }
        download.delegate = self
    }

    func webView(_ webView: WKWebView, navigationResponse: WKNavigationResponse, didBecome download: WKDownload) {
        download.delegate = self
    }

    func download(_ download: WKDownload, decideDestinationUsing response: URLResponse,
                  suggestedFilename: String) async -> URL? {
        let isCSVBlob = csvBlobDownloads.remove(ObjectIdentifier(download)) != nil &&
            sameOriginBlob(response.url) && response.mimeType?.lowercased() == "text/csv"
        let isHTTPDownload = sameOrigin(response.url) && (response as? HTTPURLResponse)?.statusCode == 200
        guard isCSVBlob || isHTTPDownload else {
            showDownloadError("服务未能提供下载文件。")
            return nil
        }
        let panel = NSSavePanel()
        panel.nameFieldStringValue = suggestedFilename
        let choice = await panel.beginSheetModal(for: window)
        guard choice == .OK, let destination = panel.url else { return nil }
        do {
            let folder = try FileManager.default.url(for: .itemReplacementDirectory, in: .userDomainMask,
                                                     appropriateFor: destination, create: true)
            let staged = folder.appendingPathComponent("download")
            downloads[ObjectIdentifier(download)] = (folder, staged, destination)
            return staged
        } catch {
            showDownloadError(error.localizedDescription)
            return nil
        }
    }

    func downloadDidFinish(_ download: WKDownload) {
        guard let item = downloads.removeValue(forKey: ObjectIdentifier(download)) else { return }
        let files = FileManager.default
        defer { try? files.removeItem(at: item.folder) }
        do {
            if files.fileExists(atPath: item.destination.path) {
                _ = try files.replaceItemAt(item.destination, withItemAt: item.staged)
            } else {
                try files.moveItem(at: item.staged, to: item.destination)
            }
        } catch {
            showDownloadError(error.localizedDescription)
        }
    }

    func download(_ download: WKDownload, didFailWithError error: Error, resumeData: Data?) {
        csvBlobDownloads.remove(ObjectIdentifier(download))
        if let item = downloads.removeValue(forKey: ObjectIdentifier(download)) {
            try? FileManager.default.removeItem(at: item.folder)
        }
        if (error as NSError).code != NSURLErrorCancelled {
            showDownloadError(error.localizedDescription)
        }
    }

    private func showDownloadError(_ detail: String) {
        let alert = NSAlert()
        alert.alertStyle = .warning
        alert.messageText = "无法保存下载文件"
        alert.informativeText = "\(detail)\n\n请检查目标目录写入权限或磁盘空间后重试。"
        alert.beginSheetModal(for: window)
    }

    private func installDraftSnapshot(in controller: WKUserContentController) {
        guard let data = try? JSONEncoder().encode(drafts) else { return }
        let json = String(decoding: data, as: UTF8.self)
        controller.removeAllUserScripts()
        controller.addUserScript(WKUserScript(
            source: "window.clusbotDrafts = Object.assign(Object.create(null), \(json));",
            injectionTime: .atDocumentStart, forMainFrameOnly: true
        ))
        installWindowDragScript(in: controller)
    }

    private func installWindowDragScript(in controller: WKUserContentController) {
        let source = """
        (() => {
          if (window.__clusbotWindowDragInstalled) return;
          window.__clusbotWindowDragInstalled = true;
          const interactiveSelector = 'button, a, input, textarea, select, option, label, summary, details, [role="button"], [role="link"], [role="tab"], [role="menuitem"], [role="option"], [role="switch"], [role="checkbox"], [role="group"], [contenteditable="true"], .sidebar-items, .toolbar-search, .toolbar-chat-actions, .workspace-notices, [data-no-drag]';
          const regionSelector = '.app-sidebar, .sidebar, .unified-toolbar, [data-window-drag]';
          const elementFromTarget = (target) => target instanceof Element ? target : target?.parentElement ?? null;
          const appRegionFor = (el) => {
            for (let cur = el; cur; cur = cur.parentElement) {
              const region = getComputedStyle(cur).getPropertyValue('-webkit-app-region').trim()
                || getComputedStyle(cur).getPropertyValue('app-region').trim();
              if (region === 'no-drag' || region === 'drag') return region;
            }
            return '';
          };
          const canDragEvent = (event, allowLoginTopStrip) => {
            if (event.button !== 0) return false;
            const el = elementFromTarget(event.target);
            if (!el) return false;
            if (el.closest(interactiveSelector)) return false;
            const region = appRegionFor(el);
            if (region === 'no-drag') return false;
            if (region === 'drag') return true;
            if (el.closest(regionSelector)) return true;
            if (allowLoginTopStrip && !document.querySelector('.workspace') && event.clientY <= 56 && !el.closest('.login-card, .acct-login-card')) {
              return true;
            }
            return false;
          };
          window.addEventListener('mousedown', (event) => {
            if (event.detail !== 1) return;
            if (!canDragEvent(event, true)) return;
            window.webkit?.messageHandlers?.clusbotWindow?.postMessage({ action: 'drag' });
          }, true);
          window.addEventListener('dblclick', (event) => {
            if (!canDragEvent(event, true)) return;
            const el = elementFromTarget(event.target);
            if (el?.closest('.unified-toolbar') || event.clientY <= 56) {
              window.webkit?.messageHandlers?.clusbotWindow?.postMessage({ action: 'zoom' });
            }
          }, true);
        })();
        """
        controller.addUserScript(WKUserScript(
            source: source,
            injectionTime: .atDocumentStart, forMainFrameOnly: true
        ))
    }

    func userContentController(_ controller: WKUserContentController, didReceive message: WKScriptMessage) {
        guard message.frameInfo.isMainFrame, sameOrigin(message.frameInfo.request.url) else { return }
        if message.name == "clusbotWindow" {
            guard let body = message.body as? [String: Any],
                  let action = body["action"] as? String else { return }
            switch action {
            case "drag":
                if let event = webView.lastMouseDownEvent ?? NSApp.currentEvent, event.type == .leftMouseDown {
                    window.performDrag(with: event)
                }
            case "zoom":
                window.performZoom(nil)
            default:
                break
            }
            return
        }
        guard let body = message.body as? [String: Any], let key = body["key"] as? String,
              key.hasPrefix("clusbot.composer.v1.") || key.hasPrefix("clusbot.memory-draft.v1."),
              let draftFile else { return }
        if let value = body["value"] as? String {
            drafts[key] = value
        } else if body["value"] is NSNull {
            drafts.removeValue(forKey: key)
        } else {
            return
        }
        installDraftSnapshot(in: controller)
        do {
            try JSONEncoder().encode(drafts).write(to: draftFile, options: .atomic)
        } catch {
            NSLog("Unable to save desktop drafts: %@", error.localizedDescription)
        }
    }

    func userContentController(_ controller: WKUserContentController, didReceive message: WKScriptMessage,
                               replyHandler: @escaping (Any?, String?) -> Void) {
        guard message.name == "clusbotNotifications", message.frameInfo.isMainFrame,
              sameOrigin(message.frameInfo.request.url),
              let body = message.body as? [String: String], let action = body["action"] else {
            replyHandler(nil, "Invalid notification request")
            return
        }
        Task { @MainActor in
            do {
                let center = UNUserNotificationCenter.current()
                switch action {
                case "status":
                    replyHandler(await notificationPermission(center), nil)
                case "request":
                    let allowed = try await center.requestAuthorization(options: [.alert, .sound])
                    replyHandler(allowed ? "granted" : "denied", nil)
                case "send":
                    guard let id = body["id"], !id.isEmpty, id.count <= 128,
                          id.utf8.allSatisfy({ (48...57).contains($0) || (65...90).contains($0) ||
                                              (97...122).contains($0) || $0 == 45 || $0 == 58 || $0 == 95 }),
                          let kind = body["kind"], let notice = notificationText(kind) else {
                        replyHandler(nil, "Invalid notification event")
                        return
                    }
                    guard await notificationPermission(center) == "granted" else {
                        replyHandler("denied", nil)
                        return
                    }
                    let content = UNMutableNotificationContent()
                    content.title = "ClusBot"
                    content.body = notice
                    content.sound = .default
                    try await center.add(UNNotificationRequest(identifier: id, content: content, trigger: nil))
                    replyHandler("sent", nil)
                default:
                    replyHandler(nil, "Unknown notification action")
                }
            } catch {
                replyHandler(nil, error.localizedDescription)
            }
        }
    }

    private func notificationPermission(_ center: UNUserNotificationCenter) async -> String {
        switch await center.notificationSettings().authorizationStatus {
        case .authorized, .provisional, .ephemeral: return "granted"
        case .denied: return "denied"
        default: return "default"
        }
    }

    private func notificationText(_ kind: String) -> String? {
        let english = Locale.preferredLanguages.first?.hasPrefix("en") == true
        switch kind {
        case "completed": return english ? "A task has completed." : "有任务已完成。"
        case "waiting_user": return english ? "A task needs your attention." : "有任务需要你确认。"
        case "interrupted": return english ? "A task was interrupted." : "有任务已中断。"
        case "test": return english ? "System notification test." : "系统通知测试。"
        default: return nil
        }
    }

    nonisolated func userNotificationCenter(_ center: UNUserNotificationCenter,
                                            willPresent notification: UNNotification) async -> UNNotificationPresentationOptions {
        [.banner, .list, .sound]
    }

    private func fail(_ detail: String) {
        guard !closing, !failureShown else { return }
        failureShown = true
        let alert = NSAlert()
        alert.alertStyle = .critical
        alert.messageText = "无法打开 ClusBot"
        alert.informativeText = "\(detail)\n\n请退出后重新打开 ClusBot；若问题持续，请检查是否已有另一个实例正在运行。"
        alert.addButton(withTitle: "退出")
        alert.beginSheetModal(for: window) { _ in NSApp.terminate(nil) }
    }

    func applicationShouldTerminateAfterLastWindowClosed(_ sender: NSApplication) -> Bool { true }

    func applicationShouldTerminate(_ sender: NSApplication) -> NSApplication.TerminateReply {
        guard service.isRunning else { return .terminateNow }
        guard !closing else { return .terminateLater }
        closing = true
        service.terminate()
        return .terminateLater
    }
}

MainActor.assumeIsolated {
    let app = NSApplication.shared
    let delegate = ClusBot()
    app.setActivationPolicy(.regular)
    app.delegate = delegate
    app.run()
}
