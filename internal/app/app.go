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

// chainID — Sử dụng 1 constant để tránh hardcode rải rác
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
	Balance string `json:"balance"` // Dùng string để chứa số lớn (BigInt)
}

// CHạy 1 lần khi BlockHeight = 0
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

	// Tính AppHash cho genesis block
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

	// Khôi phục trạng thái từ DB khi khởi động lại
	currentHeight := int64(0)
	appHash := []byte{}

	// Đọc height từ DB
	heightBytes, err := db.Get([]byte("height"))
	if err == nil && len(heightBytes) > 0 {
		if h, err := strconv.ParseInt(string(heightBytes), 10, 64); err == nil {
			currentHeight = h
			logger.Info("Restored state", "height", currentHeight)
		}
	}

	// Đọc appHash từ DB
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

// Info: return current state when restarting node
// Handshake ban đầu
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

// Được gọi bởi proposal khi bắt đầu tạo block mới.
// có thể chọn lọc, sắp xếp lại hoặc chèn thêm giao dịch vào Block.
// Input: Giao dịch thô từ Mempool -> Output: Giao dịch sẽ nằm trong Block.
func (app *App) PrepareProposal(_ context.Context, req *abci.PrepareProposalRequest) (*abci.PrepareProposalResponse, error) {
	return &abci.PrepareProposalResponse{
		Txs: req.Txs,
	}, nil
}

// được gọi bởi tất cả validators.
// Sau khi Proposer tạo xong Block và gửi đi khắp mạng lưới,
// các Validators sẽ nhận được Block đó.
// Trước khi bỏ phiếu "Đồng ý" (Precommit), họ gọi hàm này để hỏi ứng dụng xem Block này có "ngon" không.
// Nếu trả về REJECT, Validator sẽ vote nil (từ chối block này).
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

// FinalizeBlock — Xử lý tất cả giao dịch trong block
func (app *App) FinalizeBlock(ctx context.Context, req *abci.FinalizeBlockRequest) (*abci.FinalizeBlockResponse, error) {
	txResults := make([]*abci.ExecTxResult, len(req.Txs))

	// 1. Tạo StateDB mới cho block này
	stateDB := evm.NewPebbleStateDB(app.db)

	// 2. Cấu hình Chain — tất cả EIPs active từ block 0
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

	// 3. Block Context
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

	// 4. Khởi tạo EVM (1 lần cho cả block)
	vmenv := vm.NewEVM(blockContext, stateDB, &chainConfig, vm.Config{})

	signer := types.LatestSignerForChainID(chainID)

	for i, txBytes := range req.Txs {
		// Decode
		ethTx, err := app.txProcessor.DecodeTx(txBytes)
		if err != nil {
			txResults[i] = &abci.ExecTxResult{Code: 1, Log: fmt.Sprintf("decode error: %v", err)}
			continue
		}

		// Recover sender
		msg, err := core.TransactionToMessage(ethTx, signer, big.NewInt(0))
		if err != nil {
			txResults[i] = &abci.ExecTxResult{Code: 1, Log: fmt.Sprintf("invalid signature: %v", err)}
			continue
		}

		// Nonce check
		expectedNonce := stateDB.GetNonce(msg.From)
		if msg.Nonce != expectedNonce {
			txResults[i] = &abci.ExecTxResult{
				Code: 1,
				Log:  fmt.Sprintf("nonce mismatch: expected %d, got %d", expectedNonce, msg.Nonce),
			}
			continue
		}

		// Snapshot trước khi thực thi -> revert nếu tx fail
		snapID := stateDB.Snapshot()

		// Set Tx Context 
		txContext := core.NewEVMTxContext(msg)
		vmenv.SetTxContext(txContext)

		// Increment nonce TRƯỚC khi thực thi (Ethereum convention) 
		stateDB.SetNonce(msg.From, msg.Nonce+1, 0)

		//  Execute 
		var ret []byte
		var leftOverGas uint64
		var errExec error

		value, _ := uint256.FromBig(msg.Value)
		if msg.To == nil {
			// Contract Creation
			ret, _, leftOverGas, errExec = vmenv.Create(
				msg.From,
				msg.Data,
				msg.GasLimit,
				value,
			)
		} else {
			// Transaction Call
			ret, leftOverGas, errExec = vmenv.Call(
				msg.From,
				*msg.To,
				msg.Data,
				msg.GasLimit,
				value,
			)
		}
		gasUsed := msg.GasLimit - leftOverGas

		// --- Xử lý kết quả ---
		code := uint32(0)
		if errExec != nil {
			code = 1
			// Revert state nếu execution fail
			stateDB.RevertToSnapshot(snapID)
			// Nonce vẫn tăng ngay cả khi tx fail (Ethereum convention)
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

	// --- Commit state xuống đĩa ---
	if err := stateDB.Commit(); err != nil {
		return nil, fmt.Errorf("failed to commit stateDB: %w", err)
	}

	// --- Tính AppHash thực sự ---
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

// Commit — Lưu metadata xuống đĩa (block height + appHash)
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

// CheckTx — Kiểm tra giao dịch trước khi đưa vào Mempool
func (app *App) CheckTx(ctx context.Context, req *abci.CheckTxRequest) (*abci.CheckTxResponse, error) {
	// 1. Decode tx
	_, err := app.txProcessor.DecodeTx(req.Tx)
	if err != nil {
		return &abci.CheckTxResponse{
			Code: 1,
			Log:  fmt.Sprintf("invalid tx encoding: %v", err),
		}, nil
	}

	// 2. Verify signature (recover sender)
	ethTx, _ := app.txProcessor.DecodeTx(req.Tx)
	_, err = app.txProcessor.RecoverSender(ethTx)
	if err != nil {
		return &abci.CheckTxResponse{
			Code: 1,
			Log:  fmt.Sprintf("invalid signature: %v", err),
		}, nil
	}

	// Phase 1: Bỏ qua nonce/balance check trong CheckTx
	// (FinalizeBlock sẽ kiểm tra chặt hơn)

	return &abci.CheckTxResponse{Code: 0}, nil
}

func (app *App) Query(ctx context.Context, req *abci.QueryRequest) (*abci.QueryResponse, error) {
	return &abci.QueryResponse{Code: 0}, nil
}

func (app *App) Close() error {
	return app.db.Close()
}