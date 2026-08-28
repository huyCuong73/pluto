package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"strconv"

	abci "github.com/cometbft/cometbft/abci/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/params"
	"github.com/holiman/uint256"
	projectconfig "github.com/huyCuong73/pluto/internal/config"
	"github.com/huyCuong73/pluto/internal/evm"
	"github.com/huyCuong73/pluto/internal/store"
	plutotx "github.com/huyCuong73/pluto/internal/tx"
)

const (
	AppVersion uint64 = 1
)

type App struct {
	abci.BaseApplication
	db            *store.PebbleDB
	logger        *slog.Logger
	currentHeight int64
	appHash       []byte
	txValidator   plutotx.TransactionValidator
	chainID       *big.Int
}

type GenesisState struct {
	EVMChainID        int64                     `json:"evm_chain_id"`
	TransactionPolicy string                    `json:"transaction_policy"`
	Alloc             map[string]GenesisAccount `json:"alloc"`
	PQCKeys           map[string]string         `json:"pqc_keys,omitempty"`
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
	if genesisState.EVMChainID != 0 && genesisState.EVMChainID != app.chainID.Int64() {
		return nil, fmt.Errorf("genesis EVM chain ID %d does not match application chain ID %d", genesisState.EVMChainID, app.chainID.Int64())
	}
	transactionPolicy, err := projectconfig.NormalizeTransactionPolicy(genesisState.TransactionPolicy)
	if err != nil {
		return nil, fmt.Errorf("invalid genesis transaction policy: %w", err)
	}
	if (transactionPolicy == projectconfig.TransactionPolicyHybridMLDSA65 || transactionPolicy == projectconfig.TransactionPolicyPQCOptInMLDSA65) && len(genesisState.PQCKeys) == 0 {
		return nil, fmt.Errorf("ML-DSA-65 transaction policy requires at least one genesis PQ key binding")
	}
	if _, err := plutotx.NewStaticPQKeyRegistryFromHex(genesisState.PQCKeys); err != nil {
		return nil, fmt.Errorf("invalid genesis PQ key registry: %w", err)
	}

	stateDB := evm.NewPebbleStateDB(app.db)

	for addrHex, acc := range genesisState.Alloc {
		if !common.IsHexAddress(addrHex) {
			return nil, fmt.Errorf("invalid genesis address %q", addrHex)
		}
		addr := common.HexToAddress(addrHex)

		balance, ok := new(big.Int).SetString(acc.Balance, 10)
		if !ok || balance.Sign() < 0 {
			return nil, fmt.Errorf("invalid genesis balance %q for address %s", acc.Balance, addrHex)
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
	return NewAppWithChainID(dbPath, logger, projectconfig.DefaultEVMChainID)
}

func NewAppWithChainID(dbPath string, logger *slog.Logger, evmChainID int64) (*App, error) {
	validator, err := plutotx.NewECDSAValidator(evmChainID)
	if err != nil {
		return nil, fmt.Errorf("create default transaction validator: %w", err)
	}
	return NewAppWithComponents(dbPath, logger, evmChainID, validator)
}

// NewAppWithComponents is the application composition boundary. The caller
// selects a transaction validator while consensus, EVM execution and storage
// remain unchanged. A future hybrid ML-DSA validator will be plugged in here.
func NewAppWithComponents(dbPath string, logger *slog.Logger, evmChainID int64, validator plutotx.TransactionValidator) (*App, error) {
	if evmChainID <= 0 {
		return nil, fmt.Errorf("EVM chain ID must be positive")
	}
	if validator == nil {
		return nil, fmt.Errorf("transaction validator is required")
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	db, err := store.NewPebbleDB("pluto", dbPath)
	if err != nil {
		return nil, fmt.Errorf("failed to create PebbleDB: %w", err)
	}

	// Khôi phục state khi restart
	currentHeight := int64(0)
	appHash := []byte{}

	// Đọc height
	heightBytes, err := db.Get([]byte("height"))
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to restore block height: %w", err)
	}
	if len(heightBytes) > 0 {
		h, parseErr := strconv.ParseInt(string(heightBytes), 10, 64)
		if parseErr != nil {
			db.Close()
			return nil, fmt.Errorf("invalid persisted block height %q: %w", heightBytes, parseErr)
		}
		currentHeight = h
		logger.Info("Restored state", "height", currentHeight)
	}

	// Đọc appHash
	savedHash, err := db.Get([]byte("appHash"))
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to restore app hash: %w", err)
	}
	if len(savedHash) > 0 {
		appHash = savedHash
	}

	chainID := big.NewInt(evmChainID)

	return &App{
		db:            db,
		logger:        logger,
		currentHeight: currentHeight,
		appHash:       appHash,
		txValidator:   validator,
		chainID:       chainID,
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
	for index, rawTx := range req.Txs {
		if _, err := app.txValidator.Validate(rawTx); err != nil {
			app.logger.Error("Rejecting block: invalid transaction", "tx_index", index, "error", err)
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
		ChainID:             app.chainID,
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
		Coinbase:    common.Address{},                                    // TODO: Validator address
		BlockNumber: big.NewInt(req.Height),
		Time:        uint64(req.Time.Unix()),
		Difficulty:  big.NewInt(0),
		BaseFee:     big.NewInt(0), // Phase 1: gas miễn phí
		GasLimit:    30000000,
	}

	// Khởi tạo EVM cho cả block
	vmenv := vm.NewEVM(blockContext, stateDB, &chainConfig, vm.Config{})

	for i, txBytes := range req.Txs {
		// Validate the selected wire format and authentication policy.
		validatedTx, err := app.txValidator.Validate(txBytes)
		if err != nil {
			txResults[i] = validationExecResult(err)
			continue
		}

		msg := validatedTx.Message
		if msg == nil {
			txResults[i] = &abci.ExecTxResult{Code: CodeInvalidEncoding, Codespace: Codespace, Log: "transaction validator returned a nil EVM message"}
			continue
		}
		if msg.From != validatedTx.Sender {
			txResults[i] = &abci.ExecTxResult{Code: CodeInvalidSignature, Codespace: Codespace, Log: "validated sender does not match EVM sender"}
			continue
		}

		// Kiểm tra nonce
		expectedNonce := stateDB.GetNonce(msg.From)
		if msg.Nonce != expectedNonce {
			txResults[i] = &abci.ExecTxResult{
				Code:      CodeNonceMismatch,
				Codespace: Codespace,
				Log:       fmt.Sprintf("nonce mismatch: expected %d, got %d", expectedNonce, msg.Nonce),
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
		code := CodeOK
		if errExec != nil {
			code = CodeExecutionFailed
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
			Code:      code,
			Codespace: Codespace,
			GasUsed:   int64(gasUsed),
			Log:       executionLog(errExec),
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
	_, err := app.txValidator.Validate(req.Tx)
	if err != nil {
		return &abci.CheckTxResponse{
			Code:      validationCode(err),
			Codespace: Codespace,
			Log:       err.Error(),
		}, nil
	}

	// Bỏ qua nonce/balance check ở CheckTx (sẽ check kỹ ở FinalizeBlock)

	return &abci.CheckTxResponse{Code: CodeOK, Codespace: Codespace}, nil
}

func validationExecResult(err error) *abci.ExecTxResult {
	return &abci.ExecTxResult{
		Code:      validationCode(err),
		Codespace: Codespace,
		Log:       err.Error(),
	}
}

func validationCode(err error) uint32 {
	switch plutotx.FailureOf(err) {
	case plutotx.FailureEncoding:
		return CodeInvalidEncoding
	case plutotx.FailurePQC:
		return CodeInvalidPQC
	case plutotx.FailureAuthentication, plutotx.FailureUnknown:
		return CodeInvalidSignature
	default:
		return CodeInvalidSignature
	}
}

func executionLog(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (app *App) Close() error {
	return app.db.Close()
}
