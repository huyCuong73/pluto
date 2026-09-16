package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/big"
	"strconv"

	abci "github.com/cometbft/cometbft/abci/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/params"
	projectconfig "github.com/huyCuong73/pluto/internal/node/config"
	store "github.com/huyCuong73/pluto/internal/platform/storage"
	"github.com/huyCuong73/pluto/modules/evm"
	pqctx "github.com/huyCuong73/pluto/modules/pqc/tx"
	plutotx "github.com/huyCuong73/pluto/modules/transaction"
)

const (
	AppVersion    uint64 = 1
	BlockGasLimit        = projectconfig.EVMBlockGasLimit
)

type applicationDB interface {
	evm.Database
	Close() error
}

type App struct {
	abci.BaseApplication
	db            applicationDB
	logger        *slog.Logger
	currentHeight int64
	appHash       []byte
	pendingState  *evm.PebbleStateDB
	pendingHeight int64
	pendingHash   []byte
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
	if _, err := pqctx.NewStaticPQKeyRegistryFromHex(genesisState.PQCKeys); err != nil {
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

	// Compute the commitment from the same overlay that will be persisted, then
	// commit genesis state and metadata in one atomic Pebble batch.
	app.appHash, err = stateDB.ComputeAppHash()
	if err != nil {
		return nil, fmt.Errorf("compute genesis AppHash: %w", err)
	}
	batch := app.db.NewBatch()
	defer batch.Close()
	if err := stateDB.WriteToBatch(batch); err != nil {
		return nil, fmt.Errorf("stage genesis state: %w", err)
	}
	if err := batch.Set([]byte("height"), []byte("0")); err != nil {
		return nil, fmt.Errorf("stage genesis height: %w", err)
	}
	if len(app.appHash) > 0 {
		if err := batch.Set([]byte("appHash"), app.appHash); err != nil {
			return nil, fmt.Errorf("stage genesis AppHash: %w", err)
		}
	}
	if err := batch.WriteSync(); err != nil {
		return nil, fmt.Errorf("atomically commit genesis state: %w", err)
	}

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
// remain unchanged.
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
	selected := make([][]byte, 0, len(req.Txs))
	var requestedGas uint64
	rules := app.chainConfig().Rules(big.NewInt(req.Height), false, uint64(req.Time.Unix()))
	for _, rawTx := range req.Txs {
		validated, err := app.txValidator.Validate(rawTx)
		if err != nil || validateMessageForBlock(validated.Message, rules) != nil {
			continue
		}
		gas := validated.Message.GasLimit
		if gas > BlockGasLimit-requestedGas {
			continue
		}
		requestedGas += gas
		selected = append(selected, rawTx)
	}
	return &abci.PrepareProposalResponse{Txs: selected}, nil
}

// Validator kiểm tra block trước khi vote precommit
func (app *App) ProcessProposal(_ context.Context, req *abci.ProcessProposalRequest) (*abci.ProcessProposalResponse, error) {
	var requestedGas uint64
	for index, rawTx := range req.Txs {
		validated, err := app.txValidator.Validate(rawTx)
		if err == nil {
			err = validateMessageForBlock(validated.Message, app.chainConfig().Rules(big.NewInt(req.Height), false, uint64(req.Time.Unix())))
			if err == nil {
				gas := validated.Message.GasLimit
				if gas > BlockGasLimit-requestedGas {
					err = fmt.Errorf("cumulative transaction gas limits exceed EVM block gas limit %d", BlockGasLimit)
				} else {
					requestedGas += gas
				}
			}
		}
		if err != nil {
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
	if app.pendingState != nil {
		return nil, fmt.Errorf("cannot finalize height %d while height %d is pending commit", req.Height, app.pendingHeight)
	}
	txResults := make([]*abci.ExecTxResult, len(req.Txs))

	// Tạo StateDB mới cho block
	stateDB := evm.NewPebbleStateDB(app.db)

	// Cấu hình Chain, kích hoạt toàn bộ EIP từ block 0
	chainConfig := app.chainConfig()

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
		GasLimit:    BlockGasLimit,
	}

	// Khởi tạo EVM cho cả block
	vmenv := vm.NewEVM(blockContext, stateDB, chainConfig, vm.Config{NoBaseFee: true})
	gasPool := new(core.GasPool).AddGas(BlockGasLimit)

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
		if err := validateMessageForBlock(msg, chainConfig.Rules(blockContext.BlockNumber, false, blockContext.Time)); err != nil {
			txResults[i] = executionErrorResult(msg, err)
			continue
		}

		// ApplyMessage owns nonce, intrinsic gas, access-list preparation,
		// CREATE/CALL execution and refunds. This outer snapshot is only for
		// consensus-invalid transactions, which must have no state effect.
		snapID := stateDB.Snapshot()
		gasBefore := gasPool.Gas()
		feeFreeMessage := withoutGasFees(msg)
		result, transitionErr := core.ApplyMessage(vmenv, feeFreeMessage, gasPool)
		if transitionErr != nil {
			stateDB.RevertToSnapshot(snapID)
			gasPool.SetGas(gasBefore)
			stateDB.Finalise(true)
			if stateErr := stateDB.Error(); stateErr != nil {
				return nil, fmt.Errorf("execute transaction %d: %w", i, stateErr)
			}
			txResults[i] = executionErrorResult(msg, transitionErr)
			continue
		}
		stateDB.Finalise(true)
		if stateErr := stateDB.Error(); stateErr != nil {
			return nil, fmt.Errorf("execute transaction %d: %w", i, stateErr)
		}

		code := CodeOK
		if result.Failed() {
			code = CodeExecutionFailed
		}

		app.logger.Info("EVM executed",
			"height", req.Height,
			"txIndex", i,
			"from", msg.From.Hex(),
			"gasUsed", result.UsedGas,
			"err", result.Err,
			"retLen", len(result.ReturnData),
		)

		txResults[i] = &abci.ExecTxResult{
			Code:      code,
			Codespace: Codespace,
			GasWanted: int64(msg.GasLimit),
			GasUsed:   int64(result.UsedGas),
			Log:       executionLog(result.Err),
		}
	}

	if err := stateDB.Error(); err != nil {
		return nil, fmt.Errorf("finalize block state: %w", err)
	}
	newAppHash, err := stateDB.ComputeAppHash()
	if err != nil {
		return nil, fmt.Errorf("compute pending AppHash: %w", err)
	}
	if newAppHash == nil {
		newAppHash = bytes.Clone(app.appHash)
	}
	app.pendingState = stateDB
	app.pendingHeight = req.Height
	app.pendingHash = bytes.Clone(newAppHash)

	return &abci.FinalizeBlockResponse{
		TxResults: txResults,
		AppHash:   bytes.Clone(app.pendingHash),
	}, nil
}

// Commit atomically persists the pending block state and its metadata.
func (app *App) Commit(ctx context.Context, req *abci.CommitRequest) (*abci.CommitResponse, error) {
	if app.pendingState == nil {
		return &abci.CommitResponse{}, nil
	}
	batch := app.db.NewBatch()
	defer batch.Close()
	if err := app.pendingState.WriteToBatch(batch); err != nil {
		return nil, fmt.Errorf("stage pending state: %w", err)
	}
	if err := batch.Set([]byte("height"), []byte(strconv.FormatInt(app.pendingHeight, 10))); err != nil {
		return nil, fmt.Errorf("stage block height: %w", err)
	}
	if len(app.pendingHash) > 0 {
		if err := batch.Set([]byte("appHash"), app.pendingHash); err != nil {
			return nil, fmt.Errorf("stage AppHash: %w", err)
		}
	}
	if err := batch.WriteSync(); err != nil {
		return nil, fmt.Errorf("atomically commit block: %w", err)
	}
	app.currentHeight = app.pendingHeight
	app.appHash = bytes.Clone(app.pendingHash)
	app.pendingState = nil
	app.pendingHeight = 0
	app.pendingHash = nil

	return &abci.CommitResponse{}, nil
}

// CheckTx kiểm tra tx trước khi vào Mempool
func (app *App) CheckTx(ctx context.Context, req *abci.CheckTxRequest) (*abci.CheckTxResponse, error) {
	validated, err := app.txValidator.Validate(req.Tx)
	if err != nil {
		return &abci.CheckTxResponse{
			Code:      validationCode(err),
			Codespace: Codespace,
			Log:       err.Error(),
		}, nil
	}
	if err := validateMessageForBlock(validated.Message, app.chainConfig().Rules(big.NewInt(app.currentHeight+1), false, 0)); err != nil {
		return &abci.CheckTxResponse{Code: CodeTransactionRejected, Codespace: Codespace, Log: err.Error()}, nil
	}

	// Bỏ qua nonce/balance check ở CheckTx (sẽ check kỹ ở FinalizeBlock)

	return &abci.CheckTxResponse{Code: CodeOK, Codespace: Codespace, GasWanted: int64(validated.Message.GasLimit)}, nil
}

func (app *App) chainConfig() *params.ChainConfig {
	return &params.ChainConfig{
		ChainID: app.chainID, HomesteadBlock: big.NewInt(0), DAOForkBlock: big.NewInt(0), DAOForkSupport: true,
		EIP150Block: big.NewInt(0), EIP155Block: big.NewInt(0), EIP158Block: big.NewInt(0),
		ByzantiumBlock: big.NewInt(0), ConstantinopleBlock: big.NewInt(0), PetersburgBlock: big.NewInt(0),
		IstanbulBlock: big.NewInt(0), MuirGlacierBlock: big.NewInt(0), BerlinBlock: big.NewInt(0), LondonBlock: big.NewInt(0),
	}
}

func validateMessageForBlock(msg *core.Message, rules params.Rules) error {
	if msg == nil {
		return fmt.Errorf("transaction validator returned a nil EVM message")
	}
	if msg.Value == nil || msg.GasPrice == nil || msg.GasFeeCap == nil || msg.GasTipCap == nil {
		return fmt.Errorf("transaction contains nil EVM numeric fields")
	}
	if msg.GasLimit > BlockGasLimit {
		return fmt.Errorf("transaction gas limit %d exceeds EVM block gas limit %d", msg.GasLimit, BlockGasLimit)
	}
	if msg.GasLimit > math.MaxInt64 {
		return fmt.Errorf("transaction gas limit %d exceeds ABCI int64 range", msg.GasLimit)
	}
	if len(msg.BlobHashes) > 0 || msg.BlobGasFeeCap != nil {
		return fmt.Errorf("blob transactions are unsupported before Cancun")
	}
	if msg.SetCodeAuthorizations != nil {
		return fmt.Errorf("set-code transactions are unsupported by Pluto's fork schedule")
	}
	intrinsic, err := core.IntrinsicGas(msg.Data, msg.AccessList, nil, msg.To == nil, rules.IsHomestead, rules.IsIstanbul, rules.IsShanghai)
	if err != nil {
		return fmt.Errorf("calculate intrinsic gas: %w", err)
	}
	if msg.GasLimit < intrinsic {
		return fmt.Errorf("%w: have %d, want %d", core.ErrIntrinsicGas, msg.GasLimit, intrinsic)
	}
	return nil
}

func withoutGasFees(msg *core.Message) *core.Message {
	copy := *msg
	copy.GasPrice = new(big.Int)
	copy.GasFeeCap = new(big.Int)
	copy.GasTipCap = new(big.Int)
	return &copy
}

func executionErrorResult(msg *core.Message, err error) *abci.ExecTxResult {
	code := CodeTransactionRejected
	if errors.Is(err, core.ErrNonceTooLow) || errors.Is(err, core.ErrNonceTooHigh) {
		code = CodeNonceMismatch
	}
	result := &abci.ExecTxResult{Code: code, Codespace: Codespace, Log: err.Error()}
	if msg != nil && msg.GasLimit <= math.MaxInt64 {
		result.GasWanted = int64(msg.GasLimit)
	}
	return result
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
