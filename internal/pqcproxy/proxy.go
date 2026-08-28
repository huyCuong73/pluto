// Package pqcproxy cung cấp companion proxy chạy ở phía ví.
// Proxy nhận raw Ethereum transaction do MetaMask ký, bổ sung chữ ký
// ML-DSA-65 rồi gửi HybridEnvelope tới Pluto node.
package pqcproxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	gethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/huyCuong73/pluto/internal/pqc"
	plutotx "github.com/huyCuong73/pluto/internal/tx"
)

const (
	// RequestLimit đủ cho raw Ethereum transaction giới hạn của Pluto khi được
	// mã hóa hex, đồng thời chặn request quá lớn trước khi JSON decode.
	RequestLimit = 2*plutotx.MaxEthereumTxSize + 64*1024
	// ResponseLimit ngăn upstream lỗi trả một response không giới hạn vào proxy.
	ResponseLimit = 8 * 1024 * 1024
)

var (
	ErrInvalidUpstream      = errors.New("upstream Ethereum JSON-RPC không hợp lệ")
	ErrInvalidAccount       = errors.New("account PQC không hợp lệ")
	ErrKeyPairMismatch      = errors.New("ML-DSA-65 public/private key không cùng một cặp")
	ErrInvalidRPCRequest    = errors.New("Ethereum JSON-RPC request không hợp lệ")
	ErrUnexpectedSender     = errors.New("không thể khôi phục sender từ raw Ethereum transaction")
	ErrUnexpectedChainID    = errors.New("EVM chain ID không khớp")
	ErrUpstreamResponseSize = errors.New("upstream JSON-RPC response vượt giới hạn")
)

// Config chỉ chứa dữ liệu client-side. PrivateKey không bao giờ được gửi tới
// validator node; New sao chép key để caller có thể xóa buffer nguồn ngay.
type Config struct {
	UpstreamURL string
	Account     common.Address
	ChainID     int64
	PublicKey   []byte
	PrivateKey  []byte
	HTTPClient  *http.Client
}

// Proxy là HTTP handler tương thích MetaMask. Chỉ eth_sendRawTransaction của
// account đã cấu hình mới được đổi thành pluto_sendHybridTransaction.
type Proxy struct {
	upstream   *url.URL
	account    common.Address
	chainID    *big.Int
	signer     gethtypes.Signer
	publicKey  []byte
	privateKey []byte
	scheme     pqc.MLDSA65
	client     *http.Client
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

func New(config Config) (*Proxy, error) {
	upstream, err := url.Parse(strings.TrimSpace(config.UpstreamURL))
	if err != nil || upstream.Host == "" || (upstream.Scheme != "http" && upstream.Scheme != "https") || upstream.User != nil {
		return nil, ErrInvalidUpstream
	}
	if config.Account == (common.Address{}) {
		return nil, ErrInvalidAccount
	}
	if config.ChainID <= 0 {
		return nil, plutotx.ErrInvalidChainID
	}

	scheme := pqc.NewMLDSA65()
	// Ký một domain cố định rồi verify ngay để phát hiện nhầm key file trước khi
	// MetaMask gửi giao dịch thật. Chữ ký probe không rời khỏi process.
	probe := []byte("PLUTO_PQC_PROXY_KEY_CHECK_V1")
	contextBytes := []byte("PLUTO-PROXY-KEY-CHECK-V1")
	signature, err := scheme.Sign(config.PrivateKey, probe, contextBytes)
	if err != nil {
		return nil, fmt.Errorf("kiểm tra ML-DSA-65 private key: %w", err)
	}
	defer clear(signature)
	if err := scheme.Verify(config.PublicKey, probe, contextBytes, signature); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrKeyPairMismatch, err)
	}

	client := config.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 45 * time.Second}
	}
	chainID := big.NewInt(config.ChainID)
	return &Proxy{
		upstream:   upstream,
		account:    config.Account,
		chainID:    chainID,
		signer:     gethtypes.LatestSignerForChainID(chainID),
		publicKey:  bytes.Clone(config.PublicKey),
		privateKey: bytes.Clone(config.PrivateKey),
		scheme:     scheme,
		client:     client,
	}, nil
}

func (proxy *Proxy) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	setCORS(writer)
	if request.Method == http.MethodOptions {
		writer.WriteHeader(http.StatusNoContent)
		return
	}
	if request.Method != http.MethodPost {
		writeRPCError(writer, nil, http.StatusMethodNotAllowed, -32600, "chỉ hỗ trợ JSON-RPC qua HTTP POST")
		return
	}

	body, err := readLimited(request.Body, RequestLimit)
	if err != nil {
		writeRPCError(writer, nil, http.StatusBadRequest, -32600, err.Error())
		return
	}
	transformed, id, err := proxy.transform(body)
	if err != nil {
		writeRPCError(writer, id, http.StatusOK, -32602, err.Error())
		return
	}

	upstreamRequest, err := http.NewRequestWithContext(request.Context(), http.MethodPost, proxy.upstream.String(), bytes.NewReader(transformed))
	if err != nil {
		writeRPCError(writer, id, http.StatusBadGateway, -32000, "không thể tạo upstream request")
		return
	}
	upstreamRequest.Header.Set("Content-Type", "application/json")
	upstreamRequest.Header.Set("Accept", "application/json")
	response, err := proxy.client.Do(upstreamRequest)
	if err != nil {
		writeRPCError(writer, id, http.StatusBadGateway, -32000, "không thể kết nối Pluto node: "+err.Error())
		return
	}
	defer response.Body.Close()

	responseBody, err := readLimited(response.Body, ResponseLimit)
	if err != nil {
		writeRPCError(writer, id, http.StatusBadGateway, -32000, ErrUpstreamResponseSize.Error())
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(response.StatusCode)
	_, _ = writer.Write(responseBody)
}

// Close xóa bản sao private key khỏi buffer do proxy quản lý. GC/runtime vẫn
// có thể đã tạo bản sao nội bộ; đây là hardening best-effort, không phải HSM.
func (proxy *Proxy) Close() {
	if proxy == nil {
		return
	}
	clear(proxy.privateKey)
	proxy.privateKey = nil
}

func (proxy *Proxy) transform(body []byte) ([]byte, json.RawMessage, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil, nil, fmt.Errorf("%w: body rỗng", ErrInvalidRPCRequest)
	}
	if trimmed[0] == '[' {
		var requests []rpcRequest
		if err := json.Unmarshal(trimmed, &requests); err != nil || len(requests) == 0 {
			return nil, nil, fmt.Errorf("%w: JSON-RPC batch", ErrInvalidRPCRequest)
		}
		for index := range requests {
			if err := proxy.transformRequest(&requests[index]); err != nil {
				return nil, requests[index].ID, err
			}
		}
		encoded, err := json.Marshal(requests)
		return encoded, nil, err
	}

	var rpcCall rpcRequest
	if err := json.Unmarshal(trimmed, &rpcCall); err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrInvalidRPCRequest, err)
	}
	if err := proxy.transformRequest(&rpcCall); err != nil {
		return nil, rpcCall.ID, err
	}
	encoded, err := json.Marshal(rpcCall)
	return encoded, rpcCall.ID, err
}

func (proxy *Proxy) transformRequest(request *rpcRequest) error {
	if request == nil || request.JSONRPC != "2.0" || strings.TrimSpace(request.Method) == "" {
		return ErrInvalidRPCRequest
	}
	if request.Method != "eth_sendRawTransaction" {
		return nil
	}

	var params []json.RawMessage
	if err := json.Unmarshal(request.Params, &params); err != nil || len(params) != 1 {
		return fmt.Errorf("%w: eth_sendRawTransaction cần đúng một tham số", ErrInvalidRPCRequest)
	}
	var encodedRaw string
	if err := json.Unmarshal(params[0], &encodedRaw); err != nil {
		return fmt.Errorf("%w: raw transaction phải là chuỗi 0x-hex", ErrInvalidRPCRequest)
	}
	raw, err := hexutil.Decode(encodedRaw)
	if err != nil {
		return fmt.Errorf("%w: decode raw transaction: %v", ErrInvalidRPCRequest, err)
	}

	ethereumTx := new(gethtypes.Transaction)
	if err := ethereumTx.UnmarshalBinary(raw); err != nil {
		return fmt.Errorf("%w: decode Ethereum transaction: %v", ErrInvalidRPCRequest, err)
	}
	if !ethereumTx.Protected() || ethereumTx.ChainId().Cmp(proxy.chainID) != 0 {
		return fmt.Errorf("%w: got %s, want %s", ErrUnexpectedChainID, ethereumTx.ChainId(), proxy.chainID)
	}
	sender, err := gethtypes.Sender(proxy.signer, ethereumTx)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnexpectedSender, err)
	}
	// Account chưa đăng ký cho proxy vẫn đi đúng endpoint Ethereum chuẩn.
	if sender != proxy.account {
		return nil
	}

	envelope, err := plutotx.CreateSignedHybridEnvelopeV1(raw, proxy.publicKey, proxy.privateKey, proxy.chainID, proxy.scheme)
	if err != nil {
		return fmt.Errorf("tạo hybrid transaction cho %s: %w", sender.Hex(), err)
	}
	request.Method = "pluto_sendHybridTransaction"
	request.Params, err = json.Marshal([]string{hexutil.Encode(envelope)})
	return err
}

func readLimited(reader io.Reader, limit int64) ([]byte, error) {
	limited := io.LimitReader(reader, limit+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("JSON-RPC body vượt giới hạn %d bytes", limit)
	}
	return data, nil
}

func setCORS(writer http.ResponseWriter) {
	writer.Header().Set("Access-Control-Allow-Origin", "*")
	writer.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	writer.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
}

func writeRPCError(writer http.ResponseWriter, id json.RawMessage, status, code int, message string) {
	setCORS(writer)
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	_ = json.NewEncoder(writer).Encode(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Error   struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}{
		JSONRPC: "2.0",
		ID:      id,
		Error: struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		}{Code: code, Message: message},
	})
}
