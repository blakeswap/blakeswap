package api

import (
	"encoding/json"
	"errors"
	pb "github.com/blakeswap/blakeswap/api/gen/blakeswap/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
)

// NormalizeSensitiveAction uses the same typed input and encoding as Service.
// This helper is called only over the private native broker; it grants nothing.
func NormalizeSensitiveAction(method string, raw json.RawMessage) (json.RawMessage, error) {
	var input proto.Message
	switch method {
	case "wallet.send":
		input = &pb.SendCoinsRequest{}
	case "transaction.bump":
		input = &pb.BumpRequest{}
	case "offer.create":
		input = &pb.CreateOfferRequest{}
	case "swap.take":
		input = &pb.TakeOfferRequest{}
	case "trade.confirm":
		input = &pb.ConfirmTradeRequest{}
	case "offer.cancel":
		input = &pb.CancelOfferRequest{}
	case "automation.save":
		input = &pb.AutomationEdit{}
	case "automation.disable":
		input = &pb.DisableAutomationRequest{}
	case "strategy.save":
		input = &pb.StrategyEdit{}
	case "strategy.stop":
		input = &pb.StopStrategyRequest{}
	case "wallet.recovery", "wallet.backup", "onboarding.get":
		input = &emptypb.Empty{}
	case "pause":
		input = &pb.SetPausedRequest{}
	case "backup.export":
		input = &pb.ExportPortableBackupRequest{}
	case "backup.import":
		input = &pb.ImportBackupRequest{}
	case "wallet.create":
		input = &pb.CreateWalletRequest{}
	case "onboarding.prepare":
		input = &pb.PrepareFirstWalletRequest{}
	case "onboarding.confirm":
		input = &pb.ConfirmFirstWalletRequest{}
	case "onboarding.export":
		input = &pb.ExportFirstWalletRequest{}
	case "onboarding.finish", "settings.update":
		input = &pb.Settings{}
	default:
		return nil, errors.New("unknown sensitive action")
	}
	if len(raw) != 0 {
		if err := protojson.Unmarshal(raw, input); err != nil {
			return nil, errors.New("invalid sensitive action parameters")
		}
	}
	return json.Marshal(input)
}
