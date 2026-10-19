import AppKit
import AVFoundation
import AudioToolbox
import CoreAudio
import ApplicationServices

private func audioCheck(_ status: OSStatus) throws {
    guard status == noErr else {
        throw fail("audio_failed", "Audio device operation failed (OSStatus \(status)). Check device availability and permissions; retry bot doctor.")
    }
}
private func audioString(_ device: AudioObjectID, _ selector: AudioObjectPropertySelector) throws -> String {
    var address = AudioObjectPropertyAddress(mSelector: selector, mScope: kAudioObjectPropertyScopeGlobal, mElement: kAudioObjectPropertyElementMain)
    var value: CFString = "" as CFString
    var size = UInt32(MemoryLayout<CFString>.size)
    try withUnsafeMutablePointer(to: &value) { pointer in
        try audioCheck(AudioObjectGetPropertyData(device, &address, 0, nil, &size, pointer))
    }
    return value as String
}
private func channels(_ device: AudioObjectID, _ scope: AudioObjectPropertyScope) throws -> Int {
    var address = AudioObjectPropertyAddress(mSelector: kAudioDevicePropertyStreamConfiguration, mScope: scope, mElement: kAudioObjectPropertyElementMain)
    var size: UInt32 = 0
    try audioCheck(AudioObjectGetPropertyDataSize(device, &address, 0, nil, &size))
    let headerSize = MemoryLayout<AudioBufferList>.offset(of: \.mBuffers)!
    guard size >= headerSize, size <= 65536 else {
        throw fail("audio_failed", "Audio device returned an invalid channel layout.")
    }
    let memory = UnsafeMutableRawPointer.allocate(byteCount: Int(size), alignment: MemoryLayout<AudioBufferList>.alignment)
    defer { memory.deallocate() }
    try audioCheck(AudioObjectGetPropertyData(device, &address, 0, nil, &size, memory))
    let list = memory.assumingMemoryBound(to: AudioBufferList.self)
    guard Int(list.pointee.mNumberBuffers) <= (Int(size) - headerSize) / MemoryLayout<AudioBuffer>.stride else {
        throw fail("audio_failed", "Invalid audio buffer count.")
    }
    return UnsafeMutableAudioBufferListPointer(list).reduce(0) { $0 + Int($1.mNumberChannels) }
}
func audioDevices() throws -> [AudioDevice] {
    var address = AudioObjectPropertyAddress(mSelector: kAudioHardwarePropertyDevices, mScope: kAudioObjectPropertyScopeGlobal, mElement: kAudioObjectPropertyElementMain)
    var size: UInt32 = 0
    try audioCheck(AudioObjectGetPropertyDataSize(AudioObjectID(kAudioObjectSystemObject), &address, 0, nil, &size))
    guard size <= 65536, size % UInt32(MemoryLayout<AudioObjectID>.size) == 0 else {
        throw fail("audio_failed", "Invalid audio device inventory.")
    }
    if size == 0 { return [] }
    var ids = [AudioObjectID](repeating: 0, count: Int(size) / MemoryLayout<AudioObjectID>.size)
    try audioCheck(AudioObjectGetPropertyData(AudioObjectID(kAudioObjectSystemObject), &address, 0, nil, &size, &ids))
    return try ids.prefix(Int(size) / MemoryLayout<AudioObjectID>.size).map { id in
        var transport: UInt32 = 0
        var transportSize = UInt32(MemoryLayout<UInt32>.size)
        var transportAddress = AudioObjectPropertyAddress(mSelector: kAudioDevicePropertyTransportType, mScope: kAudioObjectPropertyScopeGlobal, mElement: kAudioObjectPropertyElementMain)
        try audioCheck(AudioObjectGetPropertyData(id, &transportAddress, 0, nil, &transportSize, &transport))
        return AudioDevice(uid: try audioString(id, kAudioDevicePropertyDeviceUID), name: try audioString(id, kAudioObjectPropertyName),
                           inputChannels: try channels(id, kAudioObjectPropertyScopeInput), outputChannels: try channels(id, kAudioObjectPropertyScopeOutput),
                           virtual: transport == kAudioDeviceTransportTypeVirtual)
    }.sorted { $0.uid < $1.uid }
}
func microphonePermission() -> String {
    switch AVCaptureDevice.authorizationStatus(for: .audio) {
    case .notDetermined: return "not_determined"
    case .restricted: return "restricted"
    case .denied: return "denied"
    case .authorized: return "authorized"
    @unknown default: return "unknown"
    }
}
func botDoctor() throws -> BotDoctor {
    // Neither API below requests permission, captures audio, or inspects call UI.
    return BotDoctor(os: ProcessInfo.processInfo.operatingSystemVersionString,
                     microphone: microphonePermission(), accessibilityTrusted: AXIsProcessTrusted(), devices: try audioDevices())
}

// Both queues deliver callbacks on the main run loop. No shared mutable state is
// touched from another thread; synchronous disposal precedes releasing this object.
private final class ProbeState {
    var active = true
    var meter = ProbeMeter()
    var frames = 0
    var failure: OSStatus = noErr
    func fill(_ buffer: AudioQueueBufferRef) {
        let count = Int(buffer.pointee.mAudioDataBytesCapacity) / MemoryLayout<Float>.size
        let samples = buffer.pointee.mAudioData.assumingMemoryBound(to: Float.self)
        for index in 0..<count { samples[index] = probeTone(frame: frames + index) }
        frames += count
        buffer.pointee.mAudioDataByteSize = UInt32(count * MemoryLayout<Float>.size)
    }
}
private let receiveAudio: AudioQueueInputCallback = { context, queue, buffer, _, _, _ in
    guard let context else { return }
    let state = Unmanaged<ProbeState>.fromOpaque(context).takeUnretainedValue()
    guard state.active else { return }
    state.meter.add(UnsafeBufferPointer(start: buffer.pointee.mAudioData.assumingMemoryBound(to: Float.self), count: Int(buffer.pointee.mAudioDataByteSize) / MemoryLayout<Float>.size))
    let status = AudioQueueEnqueueBuffer(queue, buffer, 0, nil)
    if status != noErr { state.failure = status }
}
private let transmitAudio: AudioQueueOutputCallback = { context, queue, buffer in
    guard let context else { return }
    let state = Unmanaged<ProbeState>.fromOpaque(context).takeUnretainedValue()
    guard state.active else { return }
    state.fill(buffer)
    let status = AudioQueueEnqueueBuffer(queue, buffer, 0, nil)
    if status != noErr { state.failure = status }
}
private func selectDevice(_ queue: AudioQueueRef, _ uid: String) throws {
    var value = uid as CFString
    try withUnsafePointer(to: &value) { pointer in
        try audioCheck(AudioQueueSetProperty(queue, kAudioQueueProperty_CurrentDevice, pointer, UInt32(MemoryLayout<CFString>.size)))
    }
    try checkSelectedDevice(queue, uid)
}
private func checkSelectedDevice(_ queue: AudioQueueRef, _ uid: String) throws {
    // Never silently accept a default-device fallback.
    var actual: CFString = "" as CFString
    var size = UInt32(MemoryLayout<CFString>.size)
    try withUnsafeMutablePointer(to: &actual) { pointer in
        try audioCheck(AudioQueueGetProperty(queue, kAudioQueueProperty_CurrentDevice, pointer, &size))
    }
    guard actual as String == uid else { throw fail("device_changed", "Selected audio route changed; no default device fallback is allowed.") }
}
func runAudioProbe(_ r: Request) throws -> Reply {
    let (receive, transmit) = try probeDevices(r, audioDevices())
    let app = NSApplication.shared
    app.setActivationPolicy(.accessory)
    app.activate(ignoringOtherApps: true)
    let alert = NSAlert()
    alert.messageText = "Test FaceTime audio routing?"
    alert.informativeText = "For \(r.seconds ?? 10) seconds, measure audio from \(receive.name) and send a quiet pulsing tone to \(transmit.name).\n\nThe other caller may hear the tone. Use a consenting test caller and manually answer first. No recordings are saved; no call is answered automatically. System audio defaults are not changed."
    alert.addButton(withTitle: "Cancel")
    alert.addButton(withTitle: "Test audio")
    guard alert.runModal() == .alertSecondButtonReturn else { return Reply(code: "canceled") }
    if microphonePermission() == "not_determined" {
        var finished = false
        AVCaptureDevice.requestAccess(for: .audio) { _ in DispatchQueue.main.async { finished = true } }
        while !finished { RunLoop.current.run(until: Date(timeIntervalSinceNow: 0.05)) }
    }
    guard microphonePermission() == "authorized" else {
        throw fail("microphone_denied", "Allow microphone access for Rysh Contacts or its launching terminal in System Settings, then retry.")
    }
    // A device could disappear while confirmation/permission UI was open.
    _ = try probeDevices(r, audioDevices())
    let state = ProbeState()
    let context = Unmanaged.passUnretained(state).toOpaque()
    var format = AudioStreamBasicDescription(mSampleRate: 48000, mFormatID: kAudioFormatLinearPCM,
        mFormatFlags: kAudioFormatFlagsNativeFloatPacked, mBytesPerPacket: 4, mFramesPerPacket: 1,
        mBytesPerFrame: 4, mChannelsPerFrame: 1, mBitsPerChannel: 32, mReserved: 0)
    var input: AudioQueueRef?
    var output: AudioQueueRef?
    defer {
        state.active = false
        if let output { AudioQueueDispose(output, true) }
        if let input { AudioQueueDispose(input, true) }
        withExtendedLifetime(state) {}
    }
    try audioCheck(AudioQueueNewInput(&format, receiveAudio, context, CFRunLoopGetMain(), CFRunLoopMode.defaultMode.rawValue, 0, &input))
    try audioCheck(AudioQueueNewOutput(&format, transmitAudio, context, CFRunLoopGetMain(), CFRunLoopMode.defaultMode.rawValue, 0, &output))
    guard let input, let output else { throw fail("audio_failed", "Could not create audio queues.") }
    try selectDevice(input, receive.uid)
    try selectDevice(output, transmit.uid)
    for _ in 0..<3 {
        var buffer: AudioQueueBufferRef?
        try audioCheck(AudioQueueAllocateBuffer(input, 1920, &buffer))
        try audioCheck(AudioQueueEnqueueBuffer(input, buffer!, 0, nil))
        try audioCheck(AudioQueueAllocateBuffer(output, 1920, &buffer))
        state.fill(buffer!)
        try audioCheck(AudioQueueEnqueueBuffer(output, buffer!, 0, nil))
    }
    try audioCheck(AudioQueueStart(input, nil))
    try audioCheck(AudioQueueStart(output, nil))
    let deadline = ProcessInfo.processInfo.systemUptime + Double(r.seconds ?? 10)
    var nextRouteCheck = ProcessInfo.processInfo.systemUptime
    while ProcessInfo.processInfo.systemUptime < deadline && state.failure == noErr {
        if ProcessInfo.processInfo.systemUptime >= nextRouteCheck {
            try checkSelectedDevice(input, receive.uid)
            try checkSelectedDevice(output, transmit.uid)
            nextRouteCheck = ProcessInfo.processInfo.systemUptime + 0.25
        }
        RunLoop.current.run(until: Date(timeIntervalSinceNow: 0.02))
    }
    state.active = false
    try audioCheck(AudioQueueStop(output, true))
    try audioCheck(AudioQueueStop(input, true))
    try audioCheck(state.failure)
    // Counts mean local buffers, never remote audibility or call success.
    return Reply(probe: AudioProbe(seconds: r.seconds ?? 10, receivedSamples: state.meter.samples,
                                  rms: state.meter.rms, peak: state.meter.peak, queuedToneFrames: state.frames))
}
