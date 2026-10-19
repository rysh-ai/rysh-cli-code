import AppKit
import Contacts
import SystemConfiguration

func permission() -> String {
    switch CNContactStore.authorizationStatus(for: .contacts) {
    case .notDetermined: return "not_determined"
    case .restricted: return "restricted"
    case .denied: return "denied"
    case .authorized: return "authorized"
    @unknown default: return "limited_or_unknown"
    }
}
func ensurePermission(_ store: CNContactStore, request: Bool) throws {
    if permission() == "not_determined", request {
        var finished = false
        store.requestAccess(for: .contacts) { _, _ in
            DispatchQueue.main.async { finished = true }
        }
        while !finished { RunLoop.current.run(until: Date(timeIntervalSinceNow: 0.05)) }
    }
    guard !["not_determined", "denied", "restricted"].contains(permission()) else {
        throw fail("permission_denied", "Allow Contacts access for Rysh Contacts (or the launching terminal/Rysh app) in System Settings > Privacy & Security > Contacts, then retry.")
    }
}
let keys: [CNKeyDescriptor] = [CNContactIdentifierKey as CNKeyDescriptor,
    CNContactFormatter.descriptorForRequiredKeys(for: .fullName),
    CNContactPhoneNumbersKey as CNKeyDescriptor, CNContactEmailAddressesKey as CNKeyDescriptor]
func person(_ c: CNContact) -> Person {
    let phones = c.phoneNumbers.map { Address(id: $0.identifier,
        label: CNLabeledValue<NSString>.localizedString(forLabel: $0.label ?? "phone"),
        kind: "phone", value: $0.value.stringValue) }
    let emails = c.emailAddresses.map { Address(id: $0.identifier,
        label: CNLabeledValue<NSString>.localizedString(forLabel: $0.label ?? "email"),
        kind: "email", value: $0.value as String) }
    return Person(id: c.identifier, name: CNContactFormatter.string(from: c, style: .fullName) ?? "Unnamed contact",
                  addresses: phones + emails)
}
func resolve(_ store: CNContactStore, _ id: String) throws -> Person {
    do { return person(try store.unifiedContact(withIdentifier: id, keysToFetch: keys)) }
    catch { throw fail("stale_selection", "Contact unavailable. Refresh the contact list and choose again.") }
}
func selected(_ store: CNContactStore, _ r: Request) throws -> (Person, Address) {
    let p = try resolve(store, r.contactID!)
    return (p, try selectedAddress(p, r))
}
func handle(_ r: Request) throws -> Reply {
    try validate(r)
    var consoleUID: uid_t = 0
    let consoleName = SCDynamicStoreCopyConsoleUser(nil, &consoleUID, nil) as String?
    guard ProcessInfo.processInfo.environment["SSH_CONNECTION"] == nil,
          ProcessInfo.processInfo.environment["SSH_TTY"] == nil,
          consoleUID == getuid(), consoleName != nil, consoleName != "loginwindow" else {
        throw fail("unsupported_session", "Use FaceTime from the locally signed-in Mac desktop, not SSH or a headless session.")
    }
    let probe = URL(string: "facetime-audio:user@example.invalid")!
    let handler = NSWorkspace.shared.urlForApplication(toOpen: probe)
    // Do not hand contacts to a different application's registered URL handler.
    let handlerName = handler.flatMap { audioHandlerName(identifier: Bundle(url: $0)?.bundleIdentifier, path: $0.path) }
    let available = handlerName != nil
    if r.operation == "doctor" { return Reply(permission: permission(), available: available, handler: handlerName) }
    if r.operation == "bot_doctor" { return Reply(available: available, handler: handlerName, bot: try botDoctor()) }
    if r.operation == "bot_probe" { return try runAudioProbe(r) }
    let store = CNContactStore()
    try ensurePermission(store, request: r.operation == "contacts")
    if r.operation == "contacts" {
        var people: [Person] = []
        let request = CNContactFetchRequest(keysToFetch: keys)
        do { try store.enumerateContacts(with: request) { c, _ in people.append(person(c)) } }
        catch { throw fail("contacts_failed", "Could not read Contacts. Check permission and retry.") }
        var reply = page(people, r)
        reply.permission = permission()
        return reply
    }
    if r.operation == "resolve" { return Reply(contact: try resolve(store, r.contactID!)) }
    guard available else { throw fail("unavailable", "FaceTime audio handler unavailable on this Mac. No video or phone fallback was attempted.") }
    let (p, a) = try selected(store, r)
    _ = try audioURL(a)
    let app = NSApplication.shared
    app.setActivationPolicy(.accessory)
    app.activate(ignoringOtherApps: true)
    let alert = NSAlert()
    alert.messageText = "FaceTime audio call?"
    alert.informativeText = "\(p.name)\n\(a.label): \(a.value)\n\nApple’s \(handlerName ?? "FaceTime") app will handle FaceTime audio on this Mac."
    alert.addButton(withTitle: "Cancel")
    alert.addButton(withTitle: "Call audio")
    guard alert.runModal() == .alertSecondButtonReturn else { return Reply(opened: false) }
    try ensurePermission(store, request: false)
    let (_, current) = try selected(store, r)
    let url = try audioURL(current)
    // Resolve again after confirmation in case URL associations changed.
    guard let currentHandler = NSWorkspace.shared.urlForApplication(toOpen: url),
          let currentName = audioHandlerName(identifier: Bundle(url: currentHandler)?.bundleIdentifier, path: currentHandler.path),
          currentName == handlerName else {
        throw fail("unavailable", "FaceTime is no longer the audio handler.")
    }
    var finished = false
    var succeeded = false
    NSWorkspace.shared.open([url], withApplicationAt: currentHandler,
                            configuration: NSWorkspace.OpenConfiguration()) { running, error in
        DispatchQueue.main.async { succeeded = running != nil && error == nil; finished = true }
    }
    while !finished { RunLoop.current.run(until: Date(timeIntervalSinceNow: 0.05)) }
    guard succeeded else { throw fail("launch_failed", "Could not open FaceTime. Check that it is installed and available.") }
    return Reply(handler: currentName, opened: true)
}
var reply: Reply
 do {
    let input = FileHandle.standardInput.readDataToEndOfFile()
    guard input.count <= 16384 else { throw fail("invalid_request", "Request too large.") }
    reply = try handle(JSONDecoder().decode(Request.self, from: input))
} catch let error as Failure {
    reply = Reply(error: error.message, code: error.code)
} catch {
    reply = Reply(error: "Invalid request or Contacts unavailable.", code: "request_failed")
}
let data = try JSONEncoder().encode(reply)
FileHandle.standardOutput.write(data)
FileHandle.standardOutput.write(Data([10]))
