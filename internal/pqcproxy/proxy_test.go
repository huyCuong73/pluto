package pqcproxy

import (
	"bytes"
	"crypto/ecdsa"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	gethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	projectconfig "github.com/huyCuong73/pluto/internal/config"
	"github.com/huyCuong73/pluto/internal/pqc"
	plutotx "github.com/huyCuong73/pluto/internal/tx"
)

func TestProxyWrapsConfiguredAccountAndPreservesEthereumHash(t *testing.T) {
	chainID := projectconfig.DefaultEVMChainID
	ecdsaKey, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	sender := crypto.PubkeyToAddress(ecdsaKey.PublicKey)
	raw := signedTransfer(t, ecdsaKey, common.HexToAddress("0xf100000000000000000000000000000000000001"), chainID)
	originalTx := new(gethtypes.Transaction)
	if err := originalTx.UnmarshalBinary(raw); err != nil {
		t.Fatal(err)
	}

	scheme := pqc.NewMLDSA65()
	publicKey, privateKey, err := scheme.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	captured := make(chan rpcRequest, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		var call rpcRequest
		if err := json.Unmarshal(body, &call); err != nil {
			t.Errorf("decode upstream request: %v", err)
		}
		captured <- call
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"jsonrpc":"2.0","id":7,"result":"` + originalTx.Hash().Hex() + `"}`))
	}))
	defer upstream.Close()

	proxy, err := New(Config{
		UpstreamURL: upstream.URL,
		Account:     sender,
		ChainID:     chainID,
		PublicKey:   publicKey,
		PrivateKey:  privateKey,
	})
	if err != nil {
		t.Fatalf("create proxy: %v", err)
	}
	defer proxy.Close()
	server := httptest.NewServer(proxy)
	defer server.Close()

	response := postRPC(t, server.URL, `{"jsonrpc":"2.0","id":7,"method":"eth_sendRawTransaction","params":["`+hexutil.Encode(raw)+`"]}`)
	if !bytes.Contains(response, []byte(originalTx.Hash().Hex())) {
		t.Fatalf("proxy response %s không giữ Ethereum tx hash", response)
	}
	call := <-captured
	if call.Method != "pluto_sendHybridTransaction" {
		t.Fatalf("upstream method = %s", call.Method)
	}
	var params []string
	if err := json.Unmarshal(call.Params, &params); err != nil || len(params) != 1 {
		t.Fatalf("decode hybrid params: %v %#v", err, params)
	}
	hybridRaw, err := hexutil.Decode(params[0])
	if err != nil {
		t.Fatal(err)
	}
	ecdsaValidator, _ := plutotx.NewECDSAValidator(chainID)
	registry := plutotx.NewStaticPQKeyRegistry(map[common.Address]plutotx.PQKeyHash{
		sender: plutotx.HashPQPublicKey(publicKey),
	})
	hybridValidator, _ := plutotx.NewHybridValidator(chainID, ecdsaValidator, scheme, registry)
	if _, err := hybridValidator.Validate(hybridRaw); err != nil {
		t.Fatalf("proxy-created hybrid envelope invalid: %v", err)
	}
}

func TestProxyLeavesOtherAccountAsEthereumTransaction(t *testing.T) {
	chainID := projectconfig.DefaultEVMChainID
	configuredKey, _ := crypto.GenerateKey()
	otherKey, _ := crypto.GenerateKey()
	raw := signedTransfer(t, otherKey, common.HexToAddress("0xf200000000000000000000000000000000000002"), chainID)
	scheme := pqc.NewMLDSA65()
	publicKey, privateKey, _ := scheme.GenerateKey(nil)
	captured := make(chan rpcRequest, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		var call rpcRequest
		_ = json.Unmarshal(body, &call)
		captured <- call
		_, _ = writer.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x0"}`))
	}))
	defer upstream.Close()
	proxy, err := New(Config{
		UpstreamURL: upstream.URL,
		Account:     crypto.PubkeyToAddress(configuredKey.PublicKey),
		ChainID:     chainID,
		PublicKey:   publicKey,
		PrivateKey:  privateKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	server := httptest.NewServer(proxy)
	defer server.Close()
	_ = postRPC(t, server.URL, `{"jsonrpc":"2.0","id":1,"method":"eth_sendRawTransaction","params":["`+hexutil.Encode(raw)+`"]}`)
	if call := <-captured; call.Method != "eth_sendRawTransaction" {
		t.Fatalf("other account method = %s", call.Method)
	}
}

func TestNewRejectsMismatchedKeyPair(t *testing.T) {
	scheme := pqc.NewMLDSA65()
	publicKey, _, _ := scheme.GenerateKey(nil)
	_, otherPrivateKey, _ := scheme.GenerateKey(nil)
	_, err := New(Config{
		UpstreamURL: "http://127.0.0.1:8545",
		Account:     common.HexToAddress("0xf300000000000000000000000000000000000003"),
		ChainID:     projectconfig.DefaultEVMChainID,
		PublicKey:   publicKey,
		PrivateKey:  otherPrivateKey,
	})
	if err == nil {
		t.Fatal("mismatched ML-DSA key pair unexpectedly accepted")
	}
}

func TestServerRejectsNonLoopbackListen(t *testing.T) {
	scheme := pqc.NewMLDSA65()
	publicKey, privateKey, _ := scheme.GenerateKey(nil)
	server, err := NewServer(Config{
		UpstreamURL: "http://127.0.0.1:8545",
		Account:     common.HexToAddress("0xf400000000000000000000000000000000000004"),
		ChainID:     projectconfig.DefaultEVMChainID,
		PublicKey:   publicKey,
		PrivateKey:  privateKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer server.proxy.Close()
	if err := server.Start("0.0.0.0:0"); err == nil {
		t.Fatal("proxy unexpectedly listened on non-loopback address")
	}
}

func signedTransfer(t *testing.T, key *ecdsa.PrivateKey, receiver common.Address, chainID int64) []byte {
	t.Helper()
	unsigned := gethtypes.NewTx(&gethtypes.LegacyTx{
		Nonce: 0, To: &receiver, Value: big.NewInt(1), Gas: 21_000, GasPrice: big.NewInt(0),
	})
	signed, err := gethtypes.SignTx(unsigned, gethtypes.LatestSignerForChainID(big.NewInt(chainID)), key)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := signed.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func postRPC(t *testing.T, endpoint, body string) []byte {
	t.Helper()
	response, err := http.Post(endpoint, "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
