// Package ethrpc cung cấp lớp tương thích Ethereum JSON-RPC cho Pluto.
// Package chỉ chuyển đổi giao thức; consensus và EVM vẫn nằm ở các module cũ.
package ethrpc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/big"
	"sync"

	abci "github.com/cometbft/cometbft/abci/types"
	cmtbytes "github.com/cometbft/cometbft/libs/bytes"
	ctypes "github.com/cometbft/cometbft/rpc/core/types"
	cmttypes "github.com/cometbft/cometbft/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	gethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rpc"
	plutotx "github.com/huyCuong73/pluto/internal/tx"
)

const (
	// BlockGasLimit phải đồng bộ với BlockContext trong internal/app.
	BlockGasLimit uint64 = 30_000_000
	// Giá trị bảo thủ khi prototype chưa có binary-search gas simulation.
	DefaultContractCallGas uint64 = 1_000_000
)

// ConsensusClient là phần nhỏ của CometBFT RPC mà lớp Ethereum cần dùng.
type ConsensusClient interface {
	Status(context.Context) (*ctypes.ResultStatus, error)
	ABCIQuery(context.Context, string, cmtbytes.HexBytes) (*ctypes.ResultABCIQuery, error)
	BroadcastTxCommit(context.Context, cmttypes.Tx) (*ctypes.ResultBroadcastTxCommit, error)
	Block(context.Context, *int64) (*ctypes.ResultBlock, error)
	BlockByHash(context.Context, []byte) (*ctypes.ResultBlock, error)
	BlockResults(context.Context, *int64) (*ctypes.ResultBlockResults, error)
}

// CallArgs nhận các field phổ biến mà MetaMask/ethers gửi cho eth_call và
// eth_estimateGas. Chưa dùng field nào thì vẫn decode được thay vì trả lỗi RPC.
type CallArgs struct {
	From                 *common.Address       `json:"from,omitempty"`
	To                   *common.Address       `json:"to,omitempty"`
	Gas                  *hexutil.Uint64       `json:"gas,omitempty"`
	GasPrice             *hexutil.Big          `json:"gasPrice,omitempty"`
	MaxFeePerGas         *hexutil.Big          `json:"maxFeePerGas,omitempty"`
	MaxPriorityFeePerGas *hexutil.Big          `json:"maxPriorityFeePerGas,omitempty"`
	Value                *hexutil.Big          `json:"value,omitempty"`
	Data                 *hexutil.Bytes        `json:"data,omitempty"`
	Input                *hexutil.Bytes        `json:"input,omitempty"`
	Nonce                *hexutil.Uint64       `json:"nonce,omitempty"`
	AccessList           *gethtypes.AccessList `json:"accessList,omitempty"`
}

type committedTransaction struct {
	ethereumTx *gethtypes.Transaction
	wireTx     []byte
	height     int64
	result     abci.ExecTxResult
}

// EthAPI triển khai namespace eth_ tối thiểu để MetaMask kết nối, đọc account,
// gửi transfer và theo dõi receipt.
type EthAPI struct {
	client  ConsensusClient
	chainID *big.Int

	mu        sync.RWMutex
	committed map[common.Hash]committedTransaction
}

func NewEthAPI(client ConsensusClient, chainID int64) (*EthAPI, error) {
	if client == nil {
		return nil, errors.New("CometBFT RPC client là bắt buộc")
	}
	if chainID <= 0 {
		return nil, errors.New("EVM chain ID phải là số dương")
	}
	return &EthAPI{client: client, chainID: big.NewInt(chainID), committed: make(map[common.Hash]committedTransaction)}, nil
}

func (api *EthAPI) ChainId() *hexutil.Big {
	return (*hexutil.Big)(new(big.Int).Set(api.chainID))
}

func (api *EthAPI) BlockNumber(ctx context.Context) (hexutil.Uint64, error) {
	status, err := api.client.Status(ctx)
	if err != nil {
		return 0, fmt.Errorf("đọc trạng thái CometBFT: %w", err)
	}
	if status.SyncInfo.LatestBlockHeight < 0 {
		return 0, nil
	}
	return hexutil.Uint64(status.SyncInfo.LatestBlockHeight), nil
}

func (api *EthAPI) GetBalance(ctx context.Context, address common.Address, _ rpc.BlockNumberOrHash) (*hexutil.Big, error) {
	value, err := api.queryAccount(ctx, "/balance", address)
	if err != nil {
		return nil, err
	}
	balance, ok := new(big.Int).SetString(string(value), 10)
	if !ok {
		return nil, fmt.Errorf("Pluto trả về balance không hợp lệ %q", value)
	}
	return (*hexutil.Big)(balance), nil
}

func (api *EthAPI) GetTransactionCount(ctx context.Context, address common.Address, _ rpc.BlockNumberOrHash) (hexutil.Uint64, error) {
	value, err := api.queryAccount(ctx, "/nonce", address)
	if err != nil {
		return 0, err
	}
	nonce, ok := new(big.Int).SetString(string(value), 10)
	if !ok || !nonce.IsUint64() {
		return 0, fmt.Errorf("Pluto trả về nonce không hợp lệ %q", value)
	}
	return hexutil.Uint64(nonce.Uint64()), nil
}

func (api *EthAPI) GetCode(ctx context.Context, address common.Address, _ rpc.BlockNumberOrHash) (hexutil.Bytes, error) {
	value, err := api.queryAccount(ctx, "/code", address)
	if err != nil {
		return nil, err
	}
	return hexutil.Bytes(value), nil
}

func (*EthAPI) GasPrice(context.Context) (*hexutil.Big, error) {
	return (*hexutil.Big)(new(big.Int)), nil
}

func (*EthAPI) MaxPriorityFeePerGas(context.Context) (*hexutil.Big, error) {
	return (*hexutil.Big)(new(big.Int)), nil
}

func (api *EthAPI) Syncing(ctx context.Context) (any, error) {
	status, err := api.client.Status(ctx)
	if err != nil {
		return nil, err
	}
	if !status.SyncInfo.CatchingUp {
		return false, nil
	}
	return map[string]any{
		"startingBlock": hexutil.Uint64(status.SyncInfo.EarliestBlockHeight),
		"currentBlock":  hexutil.Uint64(status.SyncInfo.LatestBlockHeight),
		"highestBlock":  hexutil.Uint64(status.SyncInfo.LatestBlockHeight),
	}, nil
}

func (*EthAPI) Accounts() []common.Address { return []common.Address{} }

// SendRawTransaction nhận đúng raw transaction do MetaMask ký. Node không ký
// thay người dùng và không giữ private key ví.
func (api *EthAPI) SendRawTransaction(ctx context.Context, raw hexutil.Bytes) (common.Hash, error) {
	ethereumTx, err := decodeEthereumTransaction(raw, api.chainID)
	if err != nil {
		return common.Hash{}, err
	}
	return api.broadcastAndRemember(ctx, ethereumTx, raw)
}

func (api *EthAPI) GetTransactionReceipt(ctx context.Context, hash common.Hash) (map[string]any, error) {
	record, found := api.committedTransaction(hash)
	if !found {
		return nil, nil
	}
	block, err := api.client.Block(ctx, &record.height)
	if err != nil {
		return nil, fmt.Errorf("đọc block chứa transaction: %w", err)
	}
	index := transactionIndex(block, record.wireTx)
	status := hexutil.Uint64(1)
	if record.result.Code != 0 {
		status = 0
	}
	gasUsed := positiveInt64(record.result.GasUsed)
	return map[string]any{
		"transactionHash":   hash,
		"transactionIndex":  hexutil.Uint64(index),
		"blockHash":         common.BytesToHash(block.BlockID.Hash),
		"blockNumber":       hexutil.Uint64(record.height),
		"from":              senderOf(record.ethereumTx, api.chainID),
		"to":                record.ethereumTx.To(),
		"cumulativeGasUsed": hexutil.Uint64(gasUsed),
		"gasUsed":           hexutil.Uint64(gasUsed),
		"contractAddress":   nil,
		"logs":              []any{},
		"logsBloom":         hexutil.Bytes(make([]byte, gethtypes.BloomByteLength)),
		"status":            status,
		"type":              hexutil.Uint64(record.ethereumTx.Type()),
		"effectiveGasPrice": (*hexutil.Big)(record.ethereumTx.GasPrice()),
	}, nil
}

func (api *EthAPI) GetTransactionByHash(ctx context.Context, hash common.Hash) (map[string]any, error) {
	record, found := api.committedTransaction(hash)
	if !found {
		return nil, nil
	}
	block, err := api.client.Block(ctx, &record.height)
	if err != nil {
		return nil, err
	}
	return api.rpcTransaction(record.ethereumTx, common.BytesToHash(block.BlockID.Hash), record.height, transactionIndex(block, record.wireTx)), nil
}

func (api *EthAPI) GetBlockByNumber(ctx context.Context, number rpc.BlockNumber, full bool) (map[string]any, error) {
	height, err := api.resolveBlockNumber(ctx, number)
	if err != nil {
		return nil, err
	}
	result, err := api.client.Block(ctx, &height)
	if err != nil {
		return nil, nil
	}
	return api.formatBlock(ctx, result, full)
}

func (api *EthAPI) GetBlockByHash(ctx context.Context, hash common.Hash, full bool) (map[string]any, error) {
	result, err := api.client.BlockByHash(ctx, hash.Bytes())
	if err != nil {
		return nil, nil
	}
	return api.formatBlock(ctx, result, full)
}

func (api *EthAPI) GetBlockTransactionCountByNumber(ctx context.Context, number rpc.BlockNumber) (*hexutil.Uint, error) {
	height, err := api.resolveBlockNumber(ctx, number)
	if err != nil {
		return nil, err
	}
	result, err := api.client.Block(ctx, &height)
	if err != nil || result == nil || result.Block == nil {
		return nil, nil
	}
	count := hexutil.Uint(len(result.Block.Data.Txs))
	return &count, nil
}

// EstimateGas đủ chính xác cho native transfer. Contract call tạm dùng trần
// bảo thủ cho tới khi có EVM snapshot simulator.
func (*EthAPI) EstimateGas(_ context.Context, args CallArgs, _ *rpc.BlockNumberOrHash) (hexutil.Uint64, error) {
	if args.Gas != nil {
		return *args.Gas, nil
	}
	if args.To != nil && len(args.callData()) == 0 {
		return hexutil.Uint64(21_000), nil
	}
	return hexutil.Uint64(DefaultContractCallGas), nil
}

// Call chưa mô phỏng contract; trả bytes rỗng và tuyệt đối không đổi state.
func (*EthAPI) Call(context.Context, CallArgs, *rpc.BlockNumberOrHash) (hexutil.Bytes, error) {
	return hexutil.Bytes{}, nil
}

func (api *EthAPI) FeeHistory(ctx context.Context, count hexutil.Uint64, newest rpc.BlockNumber, _ []float64) (map[string]any, error) {
	height, err := api.resolveBlockNumber(ctx, newest)
	if err != nil {
		return nil, err
	}
	blocks := uint64(count)
	if blocks == 0 {
		blocks = 1
	}
	oldest := int64(1)
	if candidate := height - int64(blocks) + 1; candidate > oldest {
		oldest = candidate
	}
	baseFees := make([]*hexutil.Big, blocks+1)
	for i := range baseFees {
		baseFees[i] = (*hexutil.Big)(new(big.Int))
	}
	return map[string]any{
		"oldestBlock":   hexutil.Uint64(oldest),
		"baseFeePerGas": baseFees,
		"gasUsedRatio":  make([]float64, blocks),
		"reward":        [][]*hexutil.Big{},
	}, nil
}

func (api *EthAPI) broadcastAndRemember(ctx context.Context, ethereumTx *gethtypes.Transaction, wire []byte) (common.Hash, error) {
	result, err := api.client.BroadcastTxCommit(ctx, cmttypes.Tx(wire))
	if err != nil {
		return common.Hash{}, fmt.Errorf("broadcast transaction qua CometBFT: %w", err)
	}
	if result.CheckTx.Code != 0 {
		return common.Hash{}, fmt.Errorf("Pluto CheckTx từ chối transaction: code=%d log=%s", result.CheckTx.Code, result.CheckTx.Log)
	}
	hash := ethereumTx.Hash()
	api.mu.Lock()
	api.committed[hash] = committedTransaction{
		ethereumTx: ethereumTx,
		wireTx:     bytes.Clone(wire),
		height:     result.Height,
		result:     result.TxResult,
	}
	api.mu.Unlock()
	return hash, nil
}

func (api *EthAPI) committedTransaction(hash common.Hash) (committedTransaction, bool) {
	api.mu.RLock()
	defer api.mu.RUnlock()
	record, found := api.committed[hash]
	return record, found
}

func (api *EthAPI) queryAccount(ctx context.Context, path string, address common.Address) ([]byte, error) {
	result, err := api.client.ABCIQuery(ctx, path, cmtbytes.HexBytes(address.Hex()))
	if err != nil {
		return nil, fmt.Errorf("ABCI query %s: %w", path, err)
	}
	if result.Response.Code != 0 {
		return nil, fmt.Errorf("ABCI query %s thất bại: %s", path, result.Response.Log)
	}
	return bytes.Clone(result.Response.Value), nil
}

func (api *EthAPI) resolveBlockNumber(ctx context.Context, number rpc.BlockNumber) (int64, error) {
	switch number {
	case rpc.LatestBlockNumber, rpc.PendingBlockNumber, rpc.SafeBlockNumber, rpc.FinalizedBlockNumber:
		status, err := api.client.Status(ctx)
		if err != nil {
			return 0, err
		}
		return status.SyncInfo.LatestBlockHeight, nil
	case rpc.EarliestBlockNumber:
		return 1, nil
	default:
		if number < 0 {
			return 0, fmt.Errorf("block tag không được hỗ trợ: %s", number)
		}
		return number.Int64(), nil
	}
}

func (api *EthAPI) formatBlock(ctx context.Context, result *ctypes.ResultBlock, full bool) (map[string]any, error) {
	if result == nil || result.Block == nil {
		return nil, nil
	}
	height := result.Block.Height
	blockResults, _ := api.client.BlockResults(ctx, &height)
	gasUsed := uint64(0)
	if blockResults != nil {
		for _, txResult := range blockResults.TxResults {
			if txResult != nil {
				gasUsed += positiveInt64(txResult.GasUsed)
			}
		}
	}
	transactions := make([]any, 0, len(result.Block.Data.Txs))
	blockSize := 0
	for index, wire := range result.Block.Data.Txs {
		blockSize += len(wire)
		ethereumTx, err := ethereumTransactionFromWire(wire)
		if err != nil {
			continue
		}
		if full {
			transactions = append(transactions, api.rpcTransaction(ethereumTx, common.BytesToHash(result.BlockID.Hash), height, index))
		} else {
			transactions = append(transactions, ethereumTx.Hash())
		}
	}
	return map[string]any{
		"number":           hexutil.Uint64(height),
		"hash":             common.BytesToHash(result.BlockID.Hash),
		"parentHash":       common.BytesToHash(result.Block.LastBlockID.Hash),
		"nonce":            hexutil.Bytes(make([]byte, 8)),
		"sha3Uncles":       gethtypes.EmptyUncleHash,
		"logsBloom":        hexutil.Bytes(make([]byte, gethtypes.BloomByteLength)),
		"transactionsRoot": common.BytesToHash(result.Block.Data.Txs.Hash()),
		"stateRoot":        common.BytesToHash(result.Block.AppHash),
		"receiptsRoot":     common.Hash{},
		"miner":            common.Address{},
		"difficulty":       (*hexutil.Big)(new(big.Int)),
		"totalDifficulty":  (*hexutil.Big)(new(big.Int)),
		"extraData":        hexutil.Bytes{},
		"size":             hexutil.Uint64(blockSize),
		"gasLimit":         hexutil.Uint64(BlockGasLimit),
		"gasUsed":          hexutil.Uint64(gasUsed),
		"timestamp":        hexutil.Uint64(result.Block.Time.Unix()),
		"transactions":     transactions,
		"uncles":           []common.Hash{},
		"baseFeePerGas":    (*hexutil.Big)(new(big.Int)),
	}, nil
}

func (api *EthAPI) rpcTransaction(tx *gethtypes.Transaction, blockHash common.Hash, height int64, index int) map[string]any {
	result := map[string]any{
		"hash": tx.Hash(), "nonce": hexutil.Uint64(tx.Nonce()),
		"blockHash": blockHash, "blockNumber": hexutil.Uint64(height),
		"transactionIndex": hexutil.Uint64(index), "from": senderOf(tx, api.chainID),
		"to": tx.To(), "value": (*hexutil.Big)(tx.Value()), "gas": hexutil.Uint64(tx.Gas()),
		"gasPrice": (*hexutil.Big)(tx.GasPrice()), "input": hexutil.Bytes(tx.Data()),
		"type": hexutil.Uint64(tx.Type()), "chainId": (*hexutil.Big)(tx.ChainId()),
	}
	v, r, s := tx.RawSignatureValues()
	result["v"], result["r"], result["s"] = (*hexutil.Big)(v), (*hexutil.Big)(r), (*hexutil.Big)(s)
	return result
}

// PlutoAPI giữ PQC ở namespace riêng. MetaMask chuẩn dùng eth_sendRawTransaction;
// client PQC dùng pluto_sendHybridTransaction.
type PlutoAPI struct{ eth *EthAPI }

func NewPlutoAPI(eth *EthAPI) *PlutoAPI { return &PlutoAPI{eth: eth} }

func (api *PlutoAPI) SendHybridTransaction(ctx context.Context, raw hexutil.Bytes) (common.Hash, error) {
	envelope, err := plutotx.DecodeHybridEnvelopeV1(raw)
	if err != nil {
		return common.Hash{}, fmt.Errorf("hybrid envelope không hợp lệ: %w", err)
	}
	ethereumTx, err := decodeEthereumTransaction(envelope.EthereumTx, api.eth.chainID)
	if err != nil {
		return common.Hash{}, err
	}
	return api.eth.broadcastAndRemember(ctx, ethereumTx, raw)
}

func (*PlutoAPI) SupportedPQCAlgorithms() []string { return []string{"ML-DSA-65"} }

type NetAPI struct{ chainID *big.Int }

func NewNetAPI(chainID int64) *NetAPI   { return &NetAPI{chainID: big.NewInt(chainID)} }
func (api *NetAPI) Version() string     { return api.chainID.String() }
func (*NetAPI) Listening() bool         { return true }
func (*NetAPI) PeerCount() hexutil.Uint { return 0 }

type Web3API struct{}

func (*Web3API) ClientVersion() string { return "pluto/1.0/go" }

func decodeEthereumTransaction(raw []byte, chainID *big.Int) (*gethtypes.Transaction, error) {
	ethereumTx := new(gethtypes.Transaction)
	if err := ethereumTx.UnmarshalBinary(raw); err != nil {
		return nil, fmt.Errorf("decode raw Ethereum transaction: %w", err)
	}
	if !ethereumTx.Protected() || ethereumTx.ChainId().Cmp(chainID) != 0 {
		return nil, fmt.Errorf("EVM chain ID không khớp: got %s, want %s", ethereumTx.ChainId(), chainID)
	}
	return ethereumTx, nil
}

func ethereumTransactionFromWire(wire []byte) (*gethtypes.Transaction, error) {
	ethereumTx := new(gethtypes.Transaction)
	if err := ethereumTx.UnmarshalBinary(wire); err == nil {
		return ethereumTx, nil
	}
	envelope, err := plutotx.DecodeHybridEnvelopeV1(wire)
	if err != nil {
		return nil, err
	}
	if err := ethereumTx.UnmarshalBinary(envelope.EthereumTx); err != nil {
		return nil, err
	}
	return ethereumTx, nil
}

func senderOf(tx *gethtypes.Transaction, chainID *big.Int) common.Address {
	sender, err := gethtypes.Sender(gethtypes.LatestSignerForChainID(chainID), tx)
	if err != nil {
		return common.Address{}
	}
	return sender
}

func transactionIndex(block *ctypes.ResultBlock, wire []byte) int {
	if block == nil || block.Block == nil {
		return 0
	}
	for index, candidate := range block.Block.Data.Txs {
		if bytes.Equal(candidate, wire) {
			return index
		}
	}
	return 0
}

func positiveInt64(value int64) uint64 {
	if value <= 0 {
		return 0
	}
	return uint64(value)
}

func (args CallArgs) callData() []byte {
	if args.Input != nil {
		return *args.Input
	}
	if args.Data != nil {
		return *args.Data
	}
	return nil
}
