// Manual interop check against Apple CoreMIDI (run from a normal Terminal, not a sandbox).
//
//   1. EMM_INTEROP_PORT=5504 EMM_INTEROP_SECONDS=30 go test -tags netgo -run TestInteropServe -v ./internal/rtpmidi
//   2. swiftc -O -o /tmp/emm-interop internal/rtpmidi/testdata/interop.swift && /tmp/emm-interop 5504
//
// Expect on the Go side: "PEERS: 1", "RX ... 90 40 7F", "RX ... B0 07 64" and one 3002-byte SysEx.
// Expect on the Apple side: "APPLE RX: 90 3C 64" and a 5000-byte SysEx.
// NOT YET RUN SUCCESSFULLY: MIDINetworkSession was non-functional in the sandbox it was written in.
import Foundation
import CoreMIDI

// Usage: swift interop.swift <control port>
// Connects Apple's network MIDI session to 127.0.0.1:<port>, sends a note and a
// SysEx, and prints everything it receives.

let port = Int(CommandLine.arguments[1])!
let session = MIDINetworkSession.default()
session.isEnabled = true
session.connectionPolicy = .anyone
print("session enabled=\(session.isEnabled) networkPort=\(session.networkPort) name=\(session.networkName)")

let host = MIDINetworkHost(name: "EMM-test", address: "127.0.0.1", port: port)
let conn = MIDINetworkConnection(host: host)
session.addConnection(conn)
print("connection added to \(host.address):\(host.port)")

var client = MIDIClientRef()
let cs = MIDIClientCreate("interop" as CFString, nil, nil, &client)
print("MIDIClientCreate status \(cs)")

var inPort = MIDIPortRef()
let ips = MIDIInputPortCreateWithBlock(client, "in" as CFString, &inPort) { list, _ in
    for pkt in list.unsafeSequence() {
        let n = Int(pkt.pointee.length)
        let bytes: [UInt8] = withUnsafeBytes(of: pkt.pointee.data) { Array($0.prefix(n)) }
        if n > 16 {
            print("APPLE RX: \(n) bytes, starts \(bytes.prefix(8).map { String(format: "%02X", $0) }.joined(separator: " ")) ends \(bytes.suffix(4).map { String(format: "%02X", $0) }.joined(separator: " "))")
        } else {
            print("APPLE RX: \(bytes.map { String(format: "%02X", $0) }.joined(separator: " "))")
        }
    }
}
print("MIDIInputPortCreate status \(ips)")
let src = session.sourceEndpoint()
print("source endpoint \(src)")
print("connect source status \(MIDIPortConnectSource(inPort, src, nil))")

var outPort = MIDIPortRef()
print("MIDIOutputPortCreate status \(MIDIOutputPortCreate(client, "out" as CFString, &outPort))")
let dst = session.destinationEndpoint()
print("destination endpoint \(dst)")

func send(_ bytes: [UInt8]) {
    var buffer = [UInt8](repeating: 0, count: 16384)
    buffer.withUnsafeMutableBytes { raw in
        let pl = raw.baseAddress!.assumingMemoryBound(to: MIDIPacketList.self)
        var p = MIDIPacketListInit(pl)
        p = MIDIPacketListAdd(pl, 16384, p, 0, bytes.count, bytes)
        let st = MIDISend(outPort, dst, pl)
        print("APPLE TX \(bytes.count) bytes status \(st)")
    }
}

DispatchQueue.main.asyncAfter(deadline: .now() + 4) {
    send([0x90, 0x40, 0x7F])
    send([0xB0, 0x07, 0x64])
}
DispatchQueue.main.asyncAfter(deadline: .now() + 5) {
    var sx: [UInt8] = [0xF0]
    for i in 0..<3000 { sx.append(UInt8((i * 7) & 0x7F)) }
    sx.append(0xF7)
    send(sx)
}
DispatchQueue.main.asyncAfter(deadline: .now() + 9) {
    print("connections: \(session.connections().count)")
    session.removeConnection(conn)
    print("removed connection")
}

RunLoop.main.run(until: Date(timeIntervalSinceNow: 12))
print("SWIFT DONE")
