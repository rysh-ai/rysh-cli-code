import Foundation

struct AudioDevice: Codable {
    var uid: String
    var name: String
    var inputChannels: Int
    var outputChannels: Int
    var virtual: Bool
}
struct BotDoctor: Codable {
    var os: String
    var microphone: String
    var accessibilityTrusted: Bool
    var devices: [AudioDevice]
    // Hardware/permission inventory is never proof of an operational call bot.
    var incomingCallsImplemented = false
}
struct AudioProbe: Codable {
    var seconds: Int
    var receivedSamples: Int
    var rms: Double
    var peak: Float
    var queuedToneFrames: Int
    var remoteAudibilityVerified = false
}
func validateProbe(_ r: Request) throws {
    guard let receive = r.receiveUID, let transmit = r.transmitUID,
          !receive.isEmpty, !transmit.isEmpty, receive != transmit,
          receive.utf8.count <= 1024, transmit.utf8.count <= 1024,
          !(receive + transmit).unicodeScalars.contains(where: { CharacterSet.controlCharacters.contains($0) }),
          (5...30).contains(r.seconds ?? 10) else {
        throw fail("invalid_probe", "Choose distinct receive/transmit device UIDs and a duration of 5–30 seconds.")
    }
}
func probeDevices(_ r: Request, _ devices: [AudioDevice]) throws -> (AudioDevice, AudioDevice) {
    try validateProbe(r)
    guard let receive = devices.first(where: { $0.uid == r.receiveUID }),
          let transmit = devices.first(where: { $0.uid == r.transmitUID }),
          [receive, transmit].allSatisfy({ $0.virtual && $0.inputChannels > 0 && $0.outputChannels > 0 }) else {
        throw fail("invalid_devices", "Probe needs two distinct virtual devices with both input and output channels. Run bot doctor and check your routing.")
    }
    return (receive, transmit)
}
// A quiet 440 Hz pulse, one second on/one off, with 10 ms fades; never echoes input.
func probeTone(frame: Int, sampleRate: Double = 48000) -> Float {
    let position = Double(frame).truncatingRemainder(dividingBy: sampleRate * 2)
    guard position < sampleRate else { return 0 }
    let fade = min(1, min(position, sampleRate - position) / (sampleRate * 0.01))
    return Float(0.05 * fade * sin(2 * Double.pi * 440 * Double(frame) / sampleRate))
}
struct ProbeMeter {
    var samples = 0
    var sumSquares = 0.0
    var peak: Float = 0
    mutating func add(_ values: UnsafeBufferPointer<Float>) {
        for sample in values where sample.isFinite {
            samples += 1
            sumSquares += Double(sample) * Double(sample)
            peak = max(peak, abs(sample))
        }
    }
    var rms: Double { samples == 0 ? 0 : sqrt(sumSquares / Double(samples)) }
}
