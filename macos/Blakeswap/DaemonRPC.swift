import Foundation
import GRPCCore
import GRPCNIOTransportHTTP2
import SwiftProtobuf

struct DaemonEndpoint: Decodable {
    let socket: String
    let http: String
    let token: String
}

enum DaemonRPC {
    static func endpoint(root: String, profile: String) throws -> DaemonEndpoint {
        let path = "\(root)/runtime.json"
        let attrs = try FileManager.default.attributesOfItem(atPath: path)
        guard attrs[.type] as? FileAttributeType == .typeRegular,
              let mode = attrs[.posixPermissions] as? NSNumber, mode.intValue & 0o077 == 0 else {
            throw RPCError.message("Daemon runtime file must be private.")
        }
        let raw = try Data(contentsOf: URL(fileURLWithPath: path))
        guard raw.count < 16_384,
              let endpoint = try JSONDecoder().decode([String: DaemonEndpoint].self, from: raw)[profile],
              endpoint.socket.hasPrefix("/"), endpoint.socket.utf8.count < 104,
              endpoint.token.count == 64, endpoint.token.allSatisfy({ $0.isHexDigit }) else {
            throw RPCError.message("Invalid daemon endpoint.")
        }
        return endpoint
    }
    static func call(root: String, profile: String, method: String, params: [String: Any] = [:]) async throws -> Data {
        try await call(root: root, profile: profile, method: method, payload: JSONSerialization.data(withJSONObject: params))
    }
    static func call(root: String, profile: String, method: String, payload: Data) async throws -> Data {
        let endpoint = try endpoint(root: root, profile: profile)
        do { return try await withGRPCClient(transport: .http2NIOPosix(target: .unixDomainSocket(path: endpoint.socket), transportSecurity: .plaintext)) { client in
            let service = Blakeswap_V1_DaemonService.Client(wrapping: client)
            let metadata: Metadata = ["authorization": "Bearer \(endpoint.token)"]
            var options = CallOptions.defaults
            options.timeout = .seconds(45)
            options.maxRequestMessageBytes = 131_072
            options.maxResponseMessageBytes = 8_388_608
            switch method {
            case "status":
                let request = try Google_Protobuf_Empty(jsonUTF8Data: payload)
                let response = try await service.getStatus(request, metadata: metadata, options: options)
                return try response.serializedData()
            case "tower.resolve":
                let request = try Blakeswap_V1_ResolveWatchtowerRequest(jsonUTF8Data: payload)
                let response = try await service.resolveWatchtower(request, metadata: metadata, options: options)
                return try response.serializedData()
            case "pause":
                let request = try Blakeswap_V1_SetPausedRequest(jsonUTF8Data: payload)
                let response = try await service.setPaused(request, metadata: metadata, options: options)
                return try response.serializedData()
            case "offer.create":
                let request = try Blakeswap_V1_CreateOfferRequest(jsonUTF8Data: payload)
                let response = try await service.createOffer(request, metadata: metadata, options: options)
                return try response.serializedData()
            case "offer.cancel":
                let request = try Blakeswap_V1_CancelOfferRequest(jsonUTF8Data: payload)
                let response = try await service.cancelOffer(request, metadata: metadata, options: options)
                return try response.serializedData()
            case "swap.take":
                let request = try Blakeswap_V1_TakeOfferRequest(jsonUTF8Data: payload)
                let response = try await service.takeOffer(request, metadata: metadata, options: options)
                return try response.serializedData()
            case "regtest.mine":
                let request = try Blakeswap_V1_MineRequest(jsonUTF8Data: payload)
                let response = try await service.mine(request, metadata: metadata, options: options)
                return try response.serializedData()
            case "regtest.faucet":
                let request = try Blakeswap_V1_FaucetRequest(jsonUTF8Data: payload)
                let response = try await service.faucet(request, metadata: metadata, options: options)
                return try response.serializedData()
            case "wallet.recovery":
                let request = try Google_Protobuf_Empty(jsonUTF8Data: payload)
                let response = try await service.getRecovery(request, metadata: metadata, options: options)
                return try response.serializedData()
            case "wallet.backup":
                let request = try Google_Protobuf_Empty(jsonUTF8Data: payload)
                let response = try await service.backupWallet(request, metadata: metadata, options: options)
                return try response.serializedData()
            case "settings.get":
                let request = try Google_Protobuf_Empty(jsonUTF8Data: payload)
                let response = try await service.getSettings(request, metadata: metadata, options: options)
                return try response.serializedData()
            case "settings.update":
                let request = try Blakeswap_V1_Settings(jsonUTF8Data: payload)
                let response = try await service.updateSettings(request, metadata: metadata, options: options)
                return try response.serializedData()
            case "settings.check-node":
                let request = try Blakeswap_V1_CheckNodeRequest(jsonUTF8Data: payload)
                let response = try await service.checkNode(request, metadata: metadata, options: options)
                return try response.serializedData()
            default: throw RPCError.message("Unknown daemon action.")
            }
        }
        } catch let error as GRPCCore.RPCError {
            throw RPCError.message(error.message.isEmpty ? "The wallet request failed (\(error.code))." : error.message)
        }
    }
}
