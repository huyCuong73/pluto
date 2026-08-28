package ethrpc

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/rpc"
	plutotx "github.com/huyCuong73/pluto/internal/tx"
)

// Server quản lý vòng đời HTTP JSON-RPC. Tách khỏi cmd/plutod để có thể test
// handler độc lập mà không phải mở node thật.
type Server struct {
	rpcServer  *rpc.Server
	httpServer *http.Server
	listener   net.Listener
	done       chan error
}

func NewServer(client ConsensusClient, chainID int64) (*Server, error) {
	ethAPI, err := NewEthAPI(client, chainID)
	if err != nil {
		return nil, err
	}
	rpcServer := rpc.NewServer()
	// Hybrid envelope có thể lớn hơn Ethereum tx nhiều lần, vì JSON hex làm
	// kích thước request gần gấp đôi binary.
	rpcServer.SetHTTPBodyLimit(2*plutotx.MaxHybridEnvelopeSize + 64*1024)
	if err := rpcServer.RegisterName("eth", ethAPI); err != nil {
		return nil, err
	}
	if err := rpcServer.RegisterName("net", NewNetAPI(chainID)); err != nil {
		return nil, err
	}
	if err := rpcServer.RegisterName("web3", new(Web3API)); err != nil {
		return nil, err
	}
	if err := rpcServer.RegisterName("pluto", NewPlutoAPI(ethAPI)); err != nil {
		return nil, err
	}
	return &Server{rpcServer: rpcServer, done: make(chan error, 1)}, nil
}

func (server *Server) Handler() http.Handler {
	return corsHandler(server.rpcServer)
}

// Start bind socket trước khi trả về để lỗi port được báo đồng bộ cho caller.
func (server *Server) Start(address string) error {
	if strings.TrimSpace(address) == "" {
		return errors.New("địa chỉ Ethereum JSON-RPC không được rỗng")
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	server.listener = listener
	server.httpServer = &http.Server{
		Handler:           server.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	go func() {
		err := server.httpServer.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		server.done <- err
	}()
	return nil
}

func (server *Server) Addr() net.Addr {
	if server.listener == nil {
		return nil
	}
	return server.listener.Addr()
}

func (server *Server) Done() <-chan error { return server.done }

func (server *Server) Close(ctx context.Context) error {
	server.rpcServer.Stop()
	if server.httpServer == nil {
		return nil
	}
	return server.httpServer.Shutdown(ctx)
}

// MetaMask chạy trong browser extension; CORS rộng chỉ áp dụng cho public RPC
// data, không cấp quyền đọc file/key và server không có signing endpoint.
func corsHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Access-Control-Allow-Origin", "*")
		writer.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		writer.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		if request.Method == http.MethodOptions {
			writer.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(writer, request)
	})
}
