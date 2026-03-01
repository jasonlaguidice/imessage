// swift-tools-version: 5.9
import PackageDescription

let package = Package(
    name: "ExtractKeyApp",
    platforms: [
        .macOS(.v11)
    ],
    targets: [
        .executableTarget(
            name: "ExtractKeyApp",
            path: "Sources/ExtractKeyApp",
            linkerSettings: [
                .linkedFramework("IOKit"),
                .linkedFramework("DiskArbitration"),
            ]
        )
    ]
)
