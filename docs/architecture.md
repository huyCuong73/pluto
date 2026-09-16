# Kiến trúc Pluto

Pluto được tổ chức theo hướng module hóa: binary chỉ lắp ráp các module qua
contract nhỏ, còn logic nghiệp vụ không nằm trong `cmd`.

## Cấu trúc thư mục

```text
pluto/
├── cmd/
│   └── plutod/                 # Composition root và CLI
├── internal/
│   ├── node/
│   │   ├── app/                # ABCI lifecycle và genesis state
│   │   └── config/             # Cấu hình chung của Pluto node
│   └── platform/
│       └── storage/            # Adapter PebbleDB
├── modules/
│   ├── transaction/            # Contract validation + ECDSA mặc định
│   ├── evm/                    # State transition và EVM state
│   ├── ethereumrpc/            # Adapter Ethereum JSON-RPC
│   └── pqc/                    # Module PQC độc lập
│       ├── tx/                 # Hybrid envelope, registry và policy
│       └── proxy/              # Companion signer cho MetaMask
├── docs/
│   └── research/               # Tài liệu nghiên cứu đã được version hóa
├── go.mod
└── README.md
```

Các thư mục `node-*`, key sinh cục bộ, database, binary và `tailieu/` là dữ
liệu làm việc/runtime, không phải source tree và được giữ ngoài Git.

## Hướng phụ thuộc

```text
cmd/plutod (composition root)
├── internal/node/app ──> modules/transaction + modules/pqc/tx (genesis)
│                     └─> modules/evm ──> internal/platform/storage
├── modules/ethereumrpc ──> modules/pqc/tx
└── modules/pqc
    ├── tx ──> modules/transaction
    └── proxy ──> modules/pqc + modules/pqc/tx
```

Quy tắc chính:

1. `cmd/plutod` được phép biết mọi module để lắp ráp dependency theo policy
   trong genesis.
2. `internal/node/app` chỉ thực thi transaction thông qua
   `transaction.TransactionValidator`; nó không tự giải mã hay xác thực chữ ký.
   App chỉ gọi `pqc/tx` để kiểm tra cấu hình key binding trong genesis.
3. Module không được import `cmd` hoặc phụ thuộc vào vòng đời CLI.
4. `modules/transaction` là contract nền và không phụ thuộc PQC. Vì vậy ECDSA
   vẫn chạy độc lập khi module PQC không được chọn.
5. `modules/pqc` sở hữu toàn bộ primitive ML-DSA-65. Phần `tx` và `proxy` là
   adapter của chính module này; private key chỉ tồn tại ở phía client/proxy.
6. Hạ tầng lưu trữ nằm trong `internal/platform`, tránh để module nghiệp vụ phụ
   thuộc trực tiếp vào chi tiết khởi động node.

## Cách lắp ráp transaction policy

- `ecdsa`: `transaction.ECDSAValidator` được đưa trực tiếp vào ABCI app.
- `hybrid-mldsa65`: composition root tạo ECDSA validator, PQ key registry và
  `pqc/tx.HybridValidator`, sau đó đưa validator hybrid vào app.
- `pqc-opt-in-mldsa65`: composition root bọc hai validator trên bằng
  `pqc/tx.OptInHybridValidator`.

Muốn thêm module chữ ký mới, hãy triển khai `transaction.TransactionValidator`
trong một package riêng rồi đăng ký việc lắp ráp tại `cmd/plutod`. Không thêm
nhánh thuật toán mới vào ABCI app hoặc EVM.

## Kiểm tra ranh giới

```bash
go test ./... -count=1
go vet ./...
go build ./cmd/plutod
```

Ngoài test toàn repo, có thể kiểm tra riêng PQC bằng:

```bash
go test ./modules/pqc/... -count=1
```
