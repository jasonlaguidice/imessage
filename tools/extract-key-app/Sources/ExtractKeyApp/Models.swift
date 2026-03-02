import Foundation

// MARK: - ByteArray

/// A wrapper around [UInt8] that encodes to/from a JSON array of integers
/// (e.g. [10, 20, 30]) matching Rust's serde serialize_bytes format.
/// Empty values serialize as [] (not null).
struct ByteArray: Codable, Equatable {
    var bytes: [UInt8]

    init(_ bytes: [UInt8] = []) {
        self.bytes = bytes
    }

    init(_ data: Data) {
        self.bytes = Array(data)
    }

    var isEmpty: Bool { bytes.isEmpty }
    var count: Int { bytes.count }

    func encode(to encoder: Encoder) throws {
        var container = encoder.unkeyedContainer()
        for byte in bytes {
            try container.encode(Int(byte))
        }
    }

    init(from decoder: Decoder) throws {
        var container = try decoder.unkeyedContainer()
        var result: [UInt8] = []
        while !container.isAtEnd {
            let val = try container.decode(UInt8.self)
            result.append(val)
        }
        self.bytes = result
    }

    /// Hex representation for display.
    var hexString: String {
        bytes.map { String(format: "%02x", $0) }.joined(separator: ":")
    }
}

// MARK: - HardwareConfig

/// Matches rustpush/open-absinthe/src/nac.rs HardwareConfig exactly.
/// Uses a custom encode(to:) to guarantee field order matches Go/Rust output.
struct HardwareConfig: Codable {
    var productName: String
    var ioMacAddress: ByteArray
    var platformSerialNumber: String
    var platformUUID: String
    var rootDiskUUID: String
    var boardID: String
    var osBuildNum: String
    var platformSerialNumberEnc: ByteArray
    var platformUUIDEnc: ByteArray
    var rootDiskUUIDEnc: ByteArray
    var rom: ByteArray
    var romEnc: ByteArray
    var mlb: String
    var mlbEnc: ByteArray

    enum CodingKeys: String, CodingKey {
        case productName = "product_name"
        case ioMacAddress = "io_mac_address"
        case platformSerialNumber = "platform_serial_number"
        case platformUUID = "platform_uuid"
        case rootDiskUUID = "root_disk_uuid"
        case boardID = "board_id"
        case osBuildNum = "os_build_num"
        case platformSerialNumberEnc = "platform_serial_number_enc"
        case platformUUIDEnc = "platform_uuid_enc"
        case rootDiskUUIDEnc = "root_disk_uuid_enc"
        case rom
        case romEnc = "rom_enc"
        case mlb
        case mlbEnc = "mlb_enc"
    }

    // Explicit encode to guarantee key ordering matches Go struct field order.
    // Swift's auto-synthesized Codable uses a dictionary-backed container
    // that does NOT preserve insertion order.
    func encode(to encoder: Encoder) throws {
        var c = encoder.container(keyedBy: CodingKeys.self)
        try c.encode(productName, forKey: .productName)
        try c.encode(ioMacAddress, forKey: .ioMacAddress)
        try c.encode(platformSerialNumber, forKey: .platformSerialNumber)
        try c.encode(platformUUID, forKey: .platformUUID)
        try c.encode(rootDiskUUID, forKey: .rootDiskUUID)
        try c.encode(boardID, forKey: .boardID)
        try c.encode(osBuildNum, forKey: .osBuildNum)
        try c.encode(platformSerialNumberEnc, forKey: .platformSerialNumberEnc)
        try c.encode(platformUUIDEnc, forKey: .platformUUIDEnc)
        try c.encode(rootDiskUUIDEnc, forKey: .rootDiskUUIDEnc)
        try c.encode(rom, forKey: .rom)
        try c.encode(romEnc, forKey: .romEnc)
        try c.encode(mlb, forKey: .mlb)
        try c.encode(mlbEnc, forKey: .mlbEnc)
    }
}

// MARK: - MacOSConfig

/// Matches rustpush/src/macos.rs MacOSConfig.
/// Uses a custom encode(to:) to guarantee field order matches Go struct field order.
struct MacOSConfig: Codable {
    var inner: HardwareConfig
    var version: String
    var protocolVersion: UInt32
    var deviceID: String
    var icloudUA: String
    var aoskitVersion: String

    enum CodingKeys: String, CodingKey {
        case inner
        case version
        case protocolVersion = "protocol_version"
        case deviceID = "device_id"
        case icloudUA = "icloud_ua"
        case aoskitVersion = "aoskit_version"
    }

    // Explicit encode to guarantee key ordering matches Go struct field order.
    func encode(to encoder: Encoder) throws {
        var c = encoder.container(keyedBy: CodingKeys.self)
        try c.encode(inner, forKey: .inner)
        try c.encode(version, forKey: .version)
        try c.encode(protocolVersion, forKey: .protocolVersion)
        try c.encode(deviceID, forKey: .deviceID)
        try c.encode(icloudUA, forKey: .icloudUA)
        try c.encode(aoskitVersion, forKey: .aoskitVersion)
    }
}

// MARK: - ExtractionResult

/// Result returned by the hardware extractor for the UI.
struct ExtractionResult {
    var config: MacOSConfig
    var base64Key: String
    var warnings: [String]
    var isAppleSilicon: Bool
    var hasEncFields: Bool
}

// MARK: - ExtractionError

enum ExtractionError: Error, LocalizedError {
    case noIOPlatformExpertDevice

    var errorDescription: String? {
        switch self {
        case .noIOPlatformExpertDevice:
            return "Failed to find IOPlatformExpertDevice in IOKit registry"
        }
    }
}
