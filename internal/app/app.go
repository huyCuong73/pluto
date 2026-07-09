package app

import (
	"context"
	"fmt"
	"log/slog"

	"encoding/json"
	"math/big"

	abci "github.com/cometbft/cometbft/abci/types"
	"github.com/huyCuong73/pluto/internal/evm"
	"github.com/huyCuong73/pluto/internal/store"
	"strconv"

	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/common"
	"github.com/holiman/uint256"
)

const (
	AppVersion uint64 = 1
)

// Chain ID
var chainID = big.NewInt(1)

type App struct {
	abci.BaseApplication
	db            *store.PebbleDB
	logger        *slog.Logger
	currentHeight int64
	appHash       []byte
	txProcessor   *evm.TxProcessor
}

type GenesisState struct {
	Alloc map[string]GenesisAccount `json:"alloc"`
}

type GenesisAccount struct {
	Balance string `json:"balance"` // Dùng string cho BigInt
}

// Chạy một lần khi block height = 0
func (app *App) InitChain(ctx context.Context, req *abci.InitChainRequest) (*abci.InitChainResponse, error) {
	app.logger.Info("Initializing chain with genesis data...")

	var genesisState GenesisState

	if len(req.AppStateBytes) == 0 {
		app.logger.Info("No app_state in genesis, skipping genesis allocation")
		return &abci.InitChainResponse{
			AppHash: app.appHash,
		}, nil
	}

	if err := json.Unmarshal(req.AppStateBytes, &genesisState); err != nil {
		return nil, fmt.Errorf("failed to parse genesis state: %w", err)
	}

	stateDB := evm.NewPebbleStateDB(app.db)

	for addrHex, acc := range genesisState.Alloc {
		addr := common.HexToAddress(addrHex)

		balance, ok := new(big.Int).SetString(acc.Balance, 10)
		if !ok {
			app.logger.Error("Invalid balance in genesis", "address", addrHex)
			continue
		}

		stateDB.AddBalanceBig(addr, balance)
		app.logger.Info("Genesis Alloc",
			"address", addrHex,
			"balance_wei", balance.String(),
			"balance_eth", new(big.Int).Div(balance, big.NewInt(1e18)).String(),
		)
	}

	if err := stateDB.Commit(); err != nil {
		return nil, fmt.Errorf("failed to commit genesis state: %w", err)
	}

	// Tính AppHash cho genesis
	app.appHash = stateDB.ComputeAppHash()

	return &abci.InitChainResponse{
		AppHash: app.appHash,
	}, nil
}

func NewApp(dbPath string, logger *slog.Logger) (*App, error) {
	db, err := store.NewPebbleDB("pluto", dbPath)
	if err != nil {
		return nil, fmt.Errorf("failed to create PebbleDB: %w", err)
	}

	// Khôi phục state khi restart
	currentHeight := int64(0)
	appHash := []byte{}

	// Đọc height
	heightBytes, err := db.Get([]byte("height"))
	if err == nil && len(heightBytes) > 0 {
		if h, err := strconv.ParseInt(string(heightBytes), 10, 64); err == nil {
			currentHeight = h
			logger.Info("Restored state", "height", currentHeight)
		}
	}

	// Đọc appHash
	savedHash, err := db.Get([]byte("appHash"))
	if err == nil && len(savedHash) > 0 {
		appHash = savedHash
	}

	return &App{
		db:            db,
		logger:        logger,
		currentHeight: currentHeight,
		appHash:       appHash,
		txProcessor:   evm.NewTxProcessor(chainID.Int64()),
	}, nil
}

// Info: trả về state hiện tại khi restart node
// Handshake
func (app *App) Info(ctx context.Context, req *abci.InfoRequest) (*abci.InfoResponse, error) {
	app.logger.Info("CometBFT Handshake received",
		"comet_version", req.Version,
		"p2p_version", req.P2PVersion,
		"block_version", req.BlockVersion)
	return &abci.InfoResponse{
		Data:             "pluto-v1",
		Version:          req.Version,
		AppVersion:       AppVersion,
		LastBlockHeight:  app.currentHeight,
		LastBlockAppHash: app.appHash,
	}, nil
}

// Gọi khi bắt đầu tạo block mới (chọn lọc/sắp xếp tx)
func (app *App) PrepareProposal(_ context.Context, req *abci.PrepareProposalRequest) (*abci.PrepareProposalResponse, error) {
	return &abci.PrepareProposalResponse{
		Txs: req.Txs,
	}, nil
}

// Validator kiểm tra block trước khi vote precommit
func (app *App) ProcessProposal(_ context.Context, req *abci.ProcessProposalRequest) (*abci.ProcessProposalResponse, error) {
	for _, tx := range req.Txs {
		if len(tx) == 0 {
			app.logger.Error("Rejecting block: contains empty transaction")
			return &abci.ProcessProposalResponse{
				Status: abci.PROCESS_PROPOSAL_STATUS_REJECT,
			}, nil
		}
	}

	return &abci.ProcessProposalResponse{
		Status: abci.PROCESS_PROPOSAL_STATUS_ACCEPT,
	}, nil
}

// FinalizeBlock xử lý các tx trong block
func (app *App) FinalizeBlock(ctx context.Context, req *abci.FinalizeBlockRequest) (*abci.FinalizeBlockResponse, error) {
	txResults := make([]*abci.ExecTxResult, len(req.Txs))

	// Tạo StateDB mới cho block
	stateDB := evm.NewPebbleStateDB(app.db)

	// Cấu hình Chain, kích hoạt toàn bộ EIP từ block 0
	chainConfig := params.ChainConfig{
		ChainID:             chainID,
		HomesteadBlock:      big.NewInt(0),
		DAOForkBlock:        big.NewInt(0),
		DAOForkSupport:      true,
		EIP150Block:         big.NewInt(0),
		EIP155Block:         big.NewInt(0),
		EIP158Block:         big.NewInt(0),
		ByzantiumBlock:      big.NewInt(0),
		ConstantinopleBlock: big.NewInt(0),
		PetersburgBlock:     big.NewInt(0),
		IstanbulBlock:       big.NewInt(0),
		MuirGlacierBlock:    big.NewInt(0),
		BerlinBlock:         big.NewInt(0),
		LondonBlock:         big.NewInt(0),
	}

	// Block Context
	blockContext := vm.BlockContext{
		CanTransfer: core.CanTransfer,
		Transfer:    core.Transfer,
		GetHash:     func(n uint64) common.Hash { return common.Hash{} }, // TODO: Block hash cache
		Coinbase:    common.Address{},                                     // TODO: Validator address
		BlockNumber: big.NewInt(req.Height),
		Time:        uint64(req.Time.Unix()),
		Difficulty:  big.NewInt(0),
		BaseFee:     big.NewInt(0), // Phase 1: gas miễn phí
		GasLimit:    30000000,
	}

	// Khởi tạo EVM cho cả block
	vmenv := vm.NewEVM(blockContext, stateDB, &chainConfig, vm.Config{})

	signer := types.LatestSignerForChainID(chainID)

	for i, txBytes := range req.Txs {
		// Decode tx
		ethTx, err := app.txProcessor.DecodeTx(txBytes)
		if err != nil {
			txResults[i] = &abci.ExecTxResult{Code: 1, Log: fmt.Sprintf("decode error: %v", err)}
			continue
		}

		// Lấy sender
		msg, err := core.TransactionToMessage(ethTx, signer, big.NewInt(0))
		if err != nil {
			txResults[i] = &abci.ExecTxResult{Code: 1, Log: fmt.Sprintf("invalid signature: %v", err)}
			continue
		}

		// Kiểm tra nonce
		expectedNonce := stateDB.GetNonce(msg.From)
		if msg.Nonce != expectedNonce {
			txResults[i] = &abci.ExecTxResult{
				Code: 1,
				Log:  fmt.Sprintf("nonce mismatch: expected %d, got %d", expectedNonce, msg.Nonce),
			}
			continue
		}

		// Snapshot trước khi chạy, revert nếu lỗi
		snapID := stateDB.Snapshot()

		// Set Tx Context 
		txContext := core.NewEVMTxContext(msg)
		vmenv.SetTxContext(txContext)

		// Tăng nonce trước khi chạy (Ethereum convention) 
		stateDB.SetNonce(msg.From, msg.Nonce+1, 0)

		// Thực thi tx 
		var ret []byte
		var leftOverGas uint64
		var errExec error

		value, _ := uint256.FromBig(msg.Value)
		if msg.To == nil {
			// Tạo contract
			ret, _, leftOverGas, errExec = vmenv.Create(
				msg.From,
				msg.Data,
				msg.GasLimit,
				value,
			)
		} else {
			// Gọi contract
			ret, leftOverGas, errExec = vmenv.Call(
				msg.From,
				*msg.To,
				msg.Data,
				msg.GasLimit,
				value,
			)
		}
		gasUsed := msg.GasLimit - leftOverGas

		// Xử lý kết quả
		code := uint32(0)
		if errExec != nil {
			code = 1
			// Revert state nếu thực thi lỗi
			stateDB.RevertToSnapshot(snapID)
			// Nonce vẫn tăng khi tx lỗi (Ethereum convention)
			stateDB.SetNonce(msg.From, msg.Nonce+1, 0)
		}

		app.logger.Info("EVM executed",
			"height", req.Height,
			"txIndex", i,
			"from", msg.From.Hex(),
			"gasUsed", gasUsed,
			"err", errExec,
			"retLen", len(ret),
		)

		txResults[i] = &abci.ExecTxResult{
			Code:    code,
			GasUsed: int64(gasUsed),
		}
	}

	// Commit state
	if err := stateDB.Commit(); err != nil {
		return nil, fmt.Errorf("failed to commit stateDB: %w", err)
	}

	// Tính AppHash
	newAppHash := stateDB.ComputeAppHash()
	if newAppHash != nil {
		app.appHash = newAppHash
	}

	app.currentHeight = req.Height

	return &abci.FinalizeBlockResponse{
		TxResults: txResults,
		AppHash:   app.appHash,
	}, nil
}

// Commit metadata (height + appHash)
func (app *App) Commit(ctx context.Context, req *abci.CommitRequest) (*abci.CommitResponse, error) {
	// Lưu height
	k := []byte("height")
	v := []byte(fmt.Sprintf("%d", app.currentHeight))
	if err := app.db.Set(k, v); err != nil {
		return nil, fmt.Errorf("failed to commit block height: %w", err)
	}

	// Lưu appHash
	if len(app.appHash) > 0 {
		if err := app.db.Set([]byte("appHash"), app.appHash); err != nil {
			return nil, fmt.Errorf("failed to commit appHash: %w", err)
		}
	}

	return &abci.CommitResponse{}, nil
}

// CheckTx kiểm tra tx trước khi vào Mempool
func (app *App) CheckTx(ctx context.Context, req *abci.CheckTxRequest) (*abci.CheckTxResponse, error) {
	// Decode tx
	_, err := app.txProcessor.DecodeTx(req.Tx)
	if err != nil {
		return &abci.CheckTxResponse{
			Code: 1,
			Log:  fmt.Sprintf("invalid tx encoding: %v", err),
		}, nil
	}

	// Verify signature
	ethTx, _ := app.txProcessor.DecodeTx(req.Tx)
	_, err = app.txProcessor.RecoverSender(ethTx)
	if err != nil {
		return &abci.CheckTxResponse{
			Code: 1,
			Log:  fmt.Sprintf("invalid signature: %v", err),
		}, nil
	}

	// Bỏ qua nonce/balance check ở CheckTx (sẽ check kỹ ở FinalizeBlock)

	return &abci.CheckTxResponse{Code: 0}, nil
}

func (app *App) Query(ctx context.Context, req *abci.QueryRequest) (*abci.QueryResponse, error) {
	return &abci.QueryResponse{Code: 0}, nil
}

func (app *App) Close() error {
	return app.db.Close()
}