import Foundation

struct Request: Codable {
    var version: Int
    var operation: String
    var search: String? = nil
    var offset: Int? = nil
    var limit: Int? = nil
    var contactID: String? = nil
    var addressID: String? = nil
    var expectedValue: String? = nil
    var receiveUID: String? = nil
    var transmitUID: String? = nil
    var seconds: Int? = nil
}
struct Address: Codable { var id: String; var label: String; var kind: String; var value: String }
struct Person: Codable { var id: String; var name: String; var addresses: [Address] }
struct Reply: Codable {
    var version = 1
    var permission: String? = nil
    var available: Bool? = nil
    var handler: String? = nil
    var contacts: [Person]? = nil
    var nextOffset: Int? = nil
    var contact: Person? = nil
    var opened: Bool? = nil
    var error: String? = nil
    var code: String? = nil
    var bot: BotDoctor? = nil
    var probe: AudioProbe? = nil
}
struct Failure: Error { let code: String; let message: String }
func fail(_ code: String, _ message: String) -> Failure { Failure(code: code, message: message) }
func validate(_ r: Request) throws {
    guard r.version == 1 else { throw fail("version_mismatch", "Update Rysh and its Contacts helper together.") }
    guard ["doctor", "contacts", "resolve", "call", "bot_doctor", "bot_probe"].contains(r.operation),
          (r.search ?? "").utf8.count <= 256,
          (0...100000).contains(r.offset ?? 0), (1...200).contains(r.limit ?? 50)
    else { throw fail("invalid_request", "Invalid FaceTime request.") }
    if r.operation == "bot_probe" { try validateProbe(r) }
    if ["resolve", "call"].contains(r.operation) {
        guard let id = r.contactID, !id.isEmpty, id.utf8.count <= 1024 else {
            throw fail("invalid_request", "Choose a contact first.")
        }
    }
    if r.operation == "call" {
        guard let id = r.addressID, !id.isEmpty, id.utf8.count <= 1024,
              let value = r.expectedValue, !value.isEmpty, value.utf8.count <= 1024 else {
            throw fail("invalid_request", "Choose an address first.")
        }
    }
}
func audioURL(_ address: Address) throws -> URL {
    let raw = address.value.trimmingCharacters(in: .whitespaces)
    guard !raw.isEmpty, raw.utf8.count <= 1024,
          !raw.unicodeScalars.contains(where: { CharacterSet.controlCharacters.contains($0) }),
          !raw.contains(where: { ":/?#*%\\".contains($0) }) else {
        throw fail("invalid_recipient", "This address cannot be used for a FaceTime audio call.")
    }
    var target = raw
    if address.kind == "phone" {
        let allowed = CharacterSet(charactersIn: "+0123456789 ()-.")
        guard raw.unicodeScalars.allSatisfy({ allowed.contains($0) }) else {
            throw fail("invalid_recipient", "Choose a phone number without an extension or service code.")
        }
        target = raw.filter { "+0123456789".contains($0) }
        let digits = target.hasPrefix("+") ? String(target.dropFirst()) : target
        guard !digits.isEmpty, digits.count <= 15, digits.allSatisfy({ $0.isASCII && $0.isNumber }) else {
            throw fail("invalid_recipient", "Invalid phone number.")
        }
    } else if address.kind == "email" {
        let parts = raw.split(separator: "@", omittingEmptySubsequences: false)
        let localChars = CharacterSet.alphanumerics.union(CharacterSet(charactersIn: "._+-"))
        let domainChars = CharacterSet.alphanumerics.union(CharacterSet(charactersIn: ".-"))
        guard parts.count == 2, parts.allSatisfy({ !$0.isEmpty }), raw.utf8.count <= 254,
              parts[0].unicodeScalars.allSatisfy({ localChars.contains($0) }),
              parts[1].unicodeScalars.allSatisfy({ domainChars.contains($0) }),
              !parts[0].hasPrefix("."), !parts[0].hasSuffix("."), !parts[0].contains(".."),
              parts[1].split(separator: ".", omittingEmptySubsequences: false).allSatisfy({
                  !$0.isEmpty && !$0.hasPrefix("-") && !$0.hasSuffix("-")
              }) else {
            throw fail("invalid_recipient", "Invalid email address.")
        }
    } else { throw fail("invalid_recipient", "Choose a phone number or email address.") }
    let safe = CharacterSet(charactersIn: "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789@.+-_")
    guard let encoded = target.addingPercentEncoding(withAllowedCharacters: safe),
          let url = URL(string: "facetime-audio:" + encoded) else {
        throw fail("invalid_recipient", "Invalid recipient.")
    }
    return url
}
func page(_ people: [Person], _ r: Request) -> Reply {
    let query = r.search ?? ""
    let matches = people.filter { p in query.isEmpty || p.name.localizedCaseInsensitiveContains(query) ||
        p.addresses.contains { $0.value.localizedCaseInsensitiveContains(query) }
    }.sorted { a, b in
        let cmp = a.name.localizedCaseInsensitiveCompare(b.name)
        return cmp == .orderedSame ? a.id < b.id : cmp == .orderedAscending
    }
    let start = min(r.offset ?? 0, matches.count)
    let end = min(start + (r.limit ?? 50), matches.count)
    return Reply(contacts: Array(matches[start..<end]), nextOffset: end < matches.count ? end : nil)
}

func selectedAddress(_ p: Person, _ r: Request) throws -> Address {
    guard let a = p.addresses.first(where: { $0.id == r.addressID && $0.value == r.expectedValue }) else {
        throw fail("stale_selection", "Address changed or disappeared. Refresh the contact list and choose again.")
    }
    return a
}

// macOS 26 moved the facetime-audio registration to Apple's Phone app.
// This remains a FaceTime-audio URI; never substitute tel: or facetime:.
func audioHandlerName(identifier: String?, path: String) -> String? {
    if identifier == "com.apple.FaceTime" && path == "/System/Applications/FaceTime.app" { return "FaceTime" }
    if identifier == "com.apple.mobilephone" && path == "/System/Applications/Phone.app" { return "Phone" }
    return nil
}
