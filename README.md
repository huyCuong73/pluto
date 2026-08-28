# Pluto — blockchain EVM thử nghiệm có module PQC độc lập

Pluto hiện chạy một node CometBFT + EVM tuần tự, có Ethereum JSON-RPC tối thiểu để MetaMask kết nối, đọc số dư, đọc nonce, gửi native transfer và theo dõi receipt. Module PQC dùng ML-DSA-65 được đặt ở biên xác thực transaction; EVM, state và consensus không cần biết chi tiết thuật toán PQC.

> Mức tương thích hiện tại là **MetaMask cho mạng cục bộ và native transfer**, chưa phải Ethereum RPC đầy đủ cho mọi dApp/smart contract. Xem phần giới hạn ở cuối tài liệu.

## 1. Build

Yêu cầu Go 1.24.1 trở lên.

```bash
go mod download
go build -o plutod ./cmd/plutod
```

## 2. Chạy Pluto với MetaMask

Lấy địa chỉ account trong MetaMask rồi cấp số dư genesis cho địa chỉ đó:

```bash
./plutod init \
  --home ./node \
  --chain-id pluto-local-1 \
  --tx-policy ecdsa \
  --alloc 0xDIA_CHI_METAMASK=1000000000000000000

./plutod start --home ./node --eth-rpc 127.0.0.1:8545
```

Thêm mạng thủ công trong MetaMask:

| Trường | Giá trị |
|---|---|
| Network name | Pluto Local |
| RPC URL | `http://127.0.0.1:8545` |
| Chain ID | `700001` |
| Currency symbol | `PLUTO` |

MetaMask vẫn ký secp256k1 như Ethereum bình thường. Private key ví không đi vào node Pluto.

Có thể kiểm tra RPC bằng `curl`:

```bash
curl -s http://127.0.0.1:8545 \
  -H 'content-type: application/json' \
  --data '{"jsonrpc":"2.0","id":1,"method":"eth_chainId","params":[]}'
```

## 3. Chọn chính sách transaction

Pluto cố định policy trong genesis để mọi validator dựng cùng một pipeline xác thực:

| Policy | Account thường qua MetaMask | Account đăng ký PQ key | Mục đích |
|---|---:|---:|---|
| `ecdsa` | ECDSA | ECDSA | Tương thích MetaMask đơn giản |
| `hybrid-mldsa65` | Không áp dụng | Bắt buộc ECDSA + ML-DSA-65 | Mạng nghiên cứu PQC nghiêm ngặt |
| `pqc-opt-in-mldsa65` | ECDSA | Bắt buộc ECDSA + ML-DSA-65 | Chuyển đổi từng account, không phá MetaMask toàn mạng |

Policy opt-in không cho account đã đăng ký PQ key quay lại ECDSA-only. Đây là cơ chế chống hạ cấp: nếu vẫn cho fallback thì kẻ tấn công lượng tử chỉ cần bỏ chữ ký PQC.

## 4. Chạy chế độ PQC opt-in

Tạo ML-DSA-65 key ở phía client:

```bash
./plutod pqc-keygen \
  --public-key alice-pqc-public.key \
  --private-key alice-pqc-private.key
```

Lệnh in ra SHA-256 của public key. Dùng hash đó để bind account tại genesis:

```bash
./plutod init \
  --home ./node-pqc \
  --tx-policy pqc-opt-in-mldsa65 \
  --alloc 0xDIA_CHI_ALICE=1000000000000000000 \
  --pqc-key 0xDIA_CHI_ALICE=0xSHA256_PUBLIC_KEY

./plutod start --home ./node-pqc --eth-rpc 127.0.0.1:8545
```

Để tạo hybrid transaction, lấy raw Ethereum transaction đã được account đó ký ECDSA rồi bọc và ký thêm ML-DSA-65:

```bash
./plutod pqc-wrap \
  --ethereum-tx 0xRAW_SIGNED_ETHEREUM_TX \
  --public-key alice-pqc-public.key \
  --private-key alice-pqc-private.key \
  --out alice.hybrid
```

### Gửi trực tiếp từ MetaMask bằng companion proxy

Node vẫn chạy ở `127.0.0.1:8545`. Mở terminal thứ hai và chạy signer proxy ở phía client:

```bash
./plutod pqc-proxy \
  --listen 127.0.0.1:8546 \
  --upstream http://127.0.0.1:8545 \
  --account 0xDIA_CHI_ALICE \
  --public-key alice-pqc-public.key \
  --private-key /home/USER/.pluto-keys/alice-pqc-private.key
```

Sửa RPC URL của mạng Pluto trong MetaMask thành `http://127.0.0.1:8546`. MetaMask vẫn ký ECDSA và gọi `eth_sendRawTransaction`; proxy kiểm tra sender, ký thêm ML-DSA-65, tạo HybridEnvelope rồi gọi `pluto_sendHybridTransaction` tới node. Ethereum transaction hash được giữ nguyên nên MetaMask theo dõi receipt như giao dịch thường.

Account chưa đăng ký PQC vẫn đi xuyên qua proxy bằng Ethereum transaction chuẩn. Account đã cấu hình proxy đi bằng hybrid và không còn bị node từ chối. Private key PQC chỉ nằm trong companion process phía client, không đi vào MetaMask, JSON-RPC request hay validator node.

`pqc-wrap` vẫn được giữ cho client/script nâng cao muốn tự tạo envelope thay vì chạy proxy.

## 5. Các method JSON-RPC chính

- MetaMask/ứng dụng Ethereum: `eth_chainId`, `eth_blockNumber`, `eth_getBalance`, `eth_getTransactionCount`, `eth_getCode`, `eth_estimateGas`, `eth_sendRawTransaction`, `eth_getTransactionReceipt`, `eth_getTransactionByHash`, `eth_getBlockByNumber`, `eth_getBlockByHash`, `eth_feeHistory`.
- Pluto PQC: `pluto_sendHybridTransaction`, `pluto_supportedPQCAlgorithms`; CLI `pqc-proxy` chuyển request MetaMask sang hybrid ở phía client.
- Thông tin mạng: `net_version`, `net_listening`, `net_peerCount`, `web3_clientVersion`.

## 6. Kiểm thử

```bash
go test ./... -count=1
go vet ./...
go build -o /tmp/plutod ./cmd/plutod
```

Test end-to-end dựng node thật và kiểm tra bốn đường đi: raw transaction kiểu MetaMask, strict hybrid, opt-in mixed mode, và account PQC gửi `eth_sendRawTransaction` trực tiếp qua companion proxy.

## 7. Giới hạn cần nói rõ

- `eth_call` hiện chỉ trả dữ liệu rỗng; `eth_estimateGas` mới chính xác cho native transfer. DApp contract phức tạp chưa được hỗ trợ đầy đủ.
- Receipt/log index đang giữ trong RAM của RPC process; restart node sẽ không tra lại được receipt cũ qua hash.
- Prototype chưa tính intrinsic gas, fee market và log/receipt root chuẩn Ethereum; `gasUsed` của transfer hiện có thể bằng 0.
- Header/block RPC là ánh xạ từ block CometBFT, không phải Ethereum header canonical.
- PQ key registry đang nằm trong genesis, chưa có rotation/revocation/on-chain registration.
- Mỗi process `pqc-proxy` hiện quản lý một account PQC; nhiều account cần nhiều listen port/process hoặc phiên bản keyring tiếp theo.
- Companion proxy là phần mềm client giữ private key trong RAM khi chạy. Nó chỉ cho bind loopback và yêu cầu file private key có quyền `0600`, nhưng chưa thay thế hardware wallet/HSM hoặc MetaMask Snap được audit.
- PQC hiện bảo vệ chữ ký transaction của account đã đăng ký; validator key và P2P vẫn dùng mật mã cổ điển. Không nên tuyên bố toàn blockchain đã “quantum-safe”.

Đánh giá thiết kế cũ/mới và lộ trình tiếp theo nằm tại [tailieu/PQC_REVIEW_V2.md](tailieu/PQC_REVIEW_V2.md).
