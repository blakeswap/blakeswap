// swift-tools-version: 6.1
import PackageDescription
let package = Package(
    name: "BlakeswapMac",
    platforms: [.macOS(.v15)],
    products: [.executable(name: "Blakeswap", targets: ["Blakeswap"])],
    dependencies: [
        .package(url: "https://github.com/grpc/grpc-swift-2.git", exact: "2.4.3"),
        .package(url: "https://github.com/grpc/grpc-swift-nio-transport.git", exact: "2.9.2"),
        .package(url: "https://github.com/grpc/grpc-swift-protobuf.git", exact: "2.4.1"),
        .package(url: "https://github.com/apple/swift-protobuf.git", exact: "1.38.1"),
    ],
    targets: [
        .executableTarget(name: "Blakeswap", dependencies: [
            .product(name: "GRPCCore", package: "grpc-swift-2"),
            .product(name: "GRPCNIOTransportHTTP2", package: "grpc-swift-nio-transport"),
            .product(name: "GRPCProtobuf", package: "grpc-swift-protobuf"),
            .product(name: "SwiftProtobuf", package: "swift-protobuf"),
        ], path: "Blakeswap"),
        .testTarget(name: "BlakeswapTests", dependencies: ["Blakeswap"], path: "Tests"),
    ],
    swiftLanguageModes: [.v5]
)
