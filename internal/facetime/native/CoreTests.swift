import Foundation

@main struct CoreTests {
 static func main() throws {
    func check(_ condition: Bool, _ message: String) { if !condition { fatalError(message) } }
    let a = Address(id:"a", label:"work", kind:"email", value:"alice+test@example.invalid")
    check(try audioURL(a).absoluteString == "facetime-audio:alice+test@example.invalid", "email encoding")
    let phone = Address(id:"p", label:"mobile", kind:"phone", value:"+1 (555) 555-0100")
    check(try audioURL(phone).absoluteString == "facetime-audio:+15555550100", "phone normalization")
    for invalid in ["alice@example.invalid?video=1", "x@example.invalid#y", "https://evil.invalid", "a\n@example.invalid", "a%40b@example.invalid", "a@example.invalid\\x", "a@", "a@example.invalid;1555", "a,b@example.invalid", "a@example.invalid&video=1", "a@-invalid", "a@invalid..test"] {
      var bad=a;bad.value=invalid
      do { _ = try audioURL(bad); fatalError("accepted injected recipient") } catch {}
    }
    for invalid in ["*123#", "+1555;echo bad", "++123", "123 ext 4", "+", "1234567890123456"] {
      var bad=phone;bad.value=invalid
      do { _ = try audioURL(bad); fatalError("accepted invalid phone") } catch {}
    }
    let people = [Person(id:"2",name:"Zoë",addresses:[]),Person(id:"1",name:"Alex",addresses:[a]),Person(id:"3",name:"Alex",addresses:[phone])]
    let first = page(people,Request(version:1,operation:"contacts",limit:1))
    check(first.contacts?.first?.id == "1" && first.nextOffset == 1,"stable duplicate name pagination")
    let second = page(people,Request(version:1,operation:"contacts",offset:1,limit:1))
    check(second.contacts?.first?.id == "3","second page")
    check(page(people,Request(version:1,operation:"contacts",search:"zoë")).contacts?.count == 1,"unicode search")
    check(page(people,Request(version:1,operation:"contacts",search:"example.invalid")).contacts?.count == 1,"address search")
    check(page(people,Request(version:1,operation:"contacts",search:"missing")).contacts?.isEmpty == true,"empty search")
    do { try validate(Request(version:2,operation:"doctor"));fatalError("version accepted") } catch {}
    do { try validate(Request(version:1,operation:"contacts",limit:201));fatalError("large page accepted") } catch {}
    let selectedPerson = Person(id:"c",name:"Alice",addresses:[a])
    let call = Request(version:1,operation:"call",contactID:"c",addressID:"a",expectedValue:a.value)
    check(try selectedAddress(selectedPerson,call).id == "a", "selected address reachable")
    var stale = call; stale.expectedValue = "changed@example.invalid"
    do { _ = try selectedAddress(selectedPerson,stale); fatalError("changed value accepted") } catch {}
    do { _ = try selectedAddress(Person(id:"c",name:"Alice",addresses:[]),call); fatalError("deleted address accepted") } catch {}
    check(audioHandlerName(identifier:"com.apple.mobilephone",path:"/System/Applications/Phone.app") == "Phone", "Tahoe audio handler")
    check(audioHandlerName(identifier:"com.apple.FaceTime",path:"/System/Applications/FaceTime.app") == "FaceTime", "FaceTime audio handler")
    check(audioHandlerName(identifier:"com.apple.mobilephone",path:"/Applications/Fake.app") == nil, "reject spoofed bundle")
    let rx = AudioDevice(uid: "rx", name: "Receive", inputChannels: 2, outputChannels: 2, virtual: true)
    let tx = AudioDevice(uid: "tx", name: "Transmit", inputChannels: 2, outputChannels: 2, virtual: true)
    let probe = Request(version: 1, operation: "bot_probe", receiveUID: "rx", transmitUID: "tx", seconds: 10)
    try validate(probe)
    check(try probeDevices(probe, [rx, tx]).0.uid == "rx", "valid isolated virtual routes")
    for seconds in [-1, 0, 4, 31] {
        var bad = probe; bad.seconds = seconds
        do { try validate(bad); fatalError("invalid probe duration accepted") } catch {}
    }
    for uid in ["", "rx", "bad\n", String(repeating: "x", count: 1025)] {
        var bad = probe; bad.transmitUID = uid
        do { try validate(bad); fatalError("invalid route accepted") } catch {}
    }
    var physical = tx; physical.virtual = false
    var noInput = tx; noInput.inputChannels = 0
    for inventory in [[rx], [rx, physical], [rx, noInput]] {
        do { _ = try probeDevices(probe, inventory); fatalError("unusable route accepted") } catch {}
    }
    var meter = ProbeMeter()
    let fixture: [Float] = [0.5, -0.5, 0, .nan, .infinity]
    fixture.withUnsafeBufferPointer { meter.add($0) }
    check(meter.samples == 3 && meter.peak == 0.5 && abs(meter.rms - sqrt(0.5 / 3)) < 0.000001, "finite RMS and peak")
    check(ProbeMeter().rms == 0, "empty capture must not imply audio")
    let tone = (0..<96000).map { probeTone(frame: $0) }
    check(tone.allSatisfy { $0.isFinite && abs($0) <= 0.050001 }, "tone amplitude bounded")
    check(tone[48000...].allSatisfy { $0 == 0 }, "second second silent")
    check(tone[0] == 0 && tone[1000..<47000].contains { abs($0) > 0.049 }, "tone present with fade")
    print("PASS: bot route isolation, virtual device validation, finite audio meter and bounded pulsed tone")
    print("PASS: synthetic Contacts search, pagination, recipient validation, audio URLs and protocol checks (no native Contacts access or calls)")
 }
}
