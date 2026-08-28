package app

import (
	"context"
	"fmt"
	"strings"

	abci "github.com/cometbft/cometbft/abci/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/huyCuong73/pluto/internal/evm"
)

const (
	QueryBalance = "/balance"
	QueryNonce   = "/nonce"
	QueryCode    = "/code"
)

// Query provides the minimal read-only API needed to verify committed state.
// The request data is an EVM address encoded as UTF-8 hex text.
func (app *App) Query(_ context.Context, req *abci.QueryRequest) (*abci.QueryResponse, error) {
	addressText := strings.TrimSpace(string(req.Data))
	if !common.IsHexAddress(addressText) {
		return queryError(fmt.Sprintf("invalid EVM address %q", addressText)), nil
	}

	address := common.HexToAddress(addressText)
	stateDB := evm.NewPebbleStateDB(app.db)
	response := &abci.QueryResponse{
		Code:      CodeOK,
		Codespace: Codespace,
		Key:       address.Bytes(),
		Height:    app.currentHeight,
	}

	switch req.Path {
	case QueryBalance:
		response.Value = []byte(stateDB.GetBalance(address).ToBig().String())
	case QueryNonce:
		response.Value = []byte(fmt.Sprintf("%d", stateDB.GetNonce(address)))
	case QueryCode:
		// Trả raw bytecode; lớp Ethereum JSON-RPC sẽ mã hóa thành hex data.
		response.Value = stateDB.GetCode(address)
	default:
		return queryError(fmt.Sprintf("unsupported query path %q", req.Path)), nil
	}

	return response, nil
}

func queryError(message string) *abci.QueryResponse {
	return &abci.QueryResponse{
		Code:      CodeInvalidQuery,
		Codespace: Codespace,
		Log:       message,
	}
}
