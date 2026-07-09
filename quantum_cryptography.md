# Toán học Lượng tử & Mật mã Hậu lượng tử cho Pluto Blockchain

> **Tài liệu nghiên cứu kỹ thuật — Pluto/UniCore Blockchain**
> Phiên bản: 1.0 | Ngày: 2026-07-06
> Tác giả: Pluto Research Team

---

## Mục lục

1. [Giới thiệu & Động lực](#1-giới-thiệu--động-lực)
2. [Phần I — Nền tảng Toán học: Lattice & LWE](#2-phần-i--nền-tảng-toán-học-lattice--lwe)
3. [Phần II — Post-Quantum Cryptography (PQC)](#3-phần-ii--post-quantum-cryptography-pqc)
4. [Phần III — Quantum Random Number Generation (QRNG)](#4-phần-iii--quantum-random-number-generation-qrng)
5. [Triển khai trong Pluto Blockchain](#5-triển-khai-trong-pluto-blockchain)
6. [Đánh giá hiệu năng & Trade-offs](#6-đánh-giá-hiệu-năng--trade-offs)
7. [Lộ trình triển khai](#7-lộ-trình-triển-khai)
8. [Tài liệu tham khảo](#8-tài-liệu-tham-khảo)

---

## 1. Giới thiệu & Động lực

### 1.1 Mối đe dọa lượng tử với Blockchain

Máy tính lượng tử đe dọa trực tiếp đến nền tảng mật mã mà mọi blockchain đang sử dụng:

| Thuật toán lượng tử | Mục tiêu tấn công | Ảnh hưởng |
|---|---|---|
| **Shor's Algorithm** | Phân tích số nguyên, Logarithm rời rạc | Phá vỡ RSA, ECDSA, Ed25519 |
| **Grover's Algorithm** | Tìm kiếm brute-force | Giảm 50% security level của hash (SHA-256: 256-bit → 128-bit) |

### 1.2 Phân tích mối đe dọa cụ thể với Pluto

| Thành phần Pluto | Thuật toán hiện tại | Bị đe dọa? | Mức nghiêm trọng |
|---|---|---|---|
| Validator keys (CometBFT) | Ed25519 | ✅ Shor's | **Nghiêm trọng** — giả mạo validator |
| Tx signatures (EVM) | ECDSA secp256k1 | ✅ Shor's | **Nghiêm trọng** — giả mạo giao dịch |
| AppHash | SHA-256 | ⚠️ Grover's | Trung bình — vẫn 128-bit security |
| PebbleDB storage | — | ❌ | Không ảnh hưởng |
| P2P encryption | TLS 1.3 (ECDHE) | ✅ Shor's | Cao — nghe lén traffic |

### 1.3 Mô hình đe dọa "Harvest Now, Decrypt Later" (HNDL)

```
  Hiện tại (2026)              Tương lai (2030-2040?)
  ─────────────                ──────────────────────
  Kẻ tấn công thu thập    →   Máy tính lượng tử đủ mạnh
  tất cả traffic mã hóa       → Giải mã tất cả dữ liệu cũ
  trên blockchain              → Giả mạo chữ ký, chiếm tài sản
```

> **Hệ quả:** Dữ liệu blockchain là **bất biến và công khai**. Mọi giao dịch đã ghi đều có thể bị phân tích ngược khi máy tính lượng tử xuất hiện. Chuẩn bị PQC từ bây giờ là bắt buộc.

---

## 2. Phần I — Nền tảng Toán học: Lattice & LWE

### 2.1 Lattice (Lưới) là gì?

**Lattice** (lưới) là tập hợp các điểm trong không gian Euclid tạo thành cấu trúc tuần hoàn, được sinh bởi tổ hợp tuyến tính nguyên của các vector cơ sở.

#### Định nghĩa hình thức

Cho tập vector cơ sở độc lập tuyến tính **B** = {**b₁**, **b₂**, ..., **bₙ**} ∈ ℝⁿ, lattice **L** được định nghĩa:

```
L(B) = { Σᵢ zᵢ · bᵢ | zᵢ ∈ ℤ }
```

Tức là tất cả tổ hợp tuyến tính nguyên (integer linear combinations) của các vector cơ sở.

#### Ví dụ trực quan (2D)

```
       ↑ b₂
       │  ●     ●     ●     ●
       │     ●     ●     ●
       ●  ●     ●     ●     ●
       │     ●     ●     ●
       ●──●──●──●──●──●──→ b₁
       │     ●     ●     ●
       ●  ●     ●     ●     ●

  ● = Điểm lưới (lattice point)
  b₁, b₂ = Vector cơ sở
```

### 2.2 Các bài toán khó trên Lattice

Ba bài toán nền tảng mà mật mã lattice-based dựa vào:

#### a) Shortest Vector Problem (SVP)

**Bài toán:** Tìm vector **ngắn nhất** (khác 0) trong lattice.

```
Cho lattice L(B), tìm v ∈ L(B) sao cho:
  ‖v‖ = min{ ‖u‖ : u ∈ L(B), u ≠ 0 }
```

- Trong không gian thấp (n ≤ 50): giải được bằng thuật toán LLL
- Trong không gian cao (n ≥ 256): **NP-hard** — kể cả máy tính lượng tử cũng không giải được hiệu quả

#### b) Closest Vector Problem (CVP)

**Bài toán:** Cho vector mục tiêu **t** (không nhất thiết nằm trên lưới), tìm điểm lưới **gần nhất** với **t**.

```
Cho lattice L(B) và vector t, tìm v ∈ L(B) sao cho:
  ‖v - t‖ = min{ ‖u - t‖ : u ∈ L(B) }
```

#### c) Shortest Independent Vectors Problem (SIVP)

**Bài toán:** Tìm n vector độc lập tuyến tính trong lattice sao cho vector dài nhất trong nhóm là ngắn nhất có thể.

### 2.3 Learning With Errors (LWE)

**LWE** do Oded Regev đề xuất năm 2005. Đây là nền tảng trực tiếp của PQC hiện đại.

#### Định nghĩa

Cho:
- n: kích thước bảo mật (security parameter)
- q: modulus (số nguyên tố)
- χ: phân bố lỗi (error distribution), thường là Gaussian rời rạc

**Bài toán LWE:** Phân biệt hai phân bố sau:

```
Phân bố 1 (LWE samples):     (aᵢ, bᵢ = ⟨aᵢ, s⟩ + eᵢ mod q)
Phân bố 2 (Random):          (aᵢ, uᵢ)    với uᵢ ngẫu nhiên

Trong đó:
  aᵢ ∈ ℤqⁿ    — vector ngẫu nhiên (công khai)
  s ∈ ℤqⁿ      — secret vector (bí mật)
  eᵢ ← χ       — lỗi nhỏ (noise)
  bᵢ ∈ ℤq      — giá trị quan sát được
```

**Tại sao khó?** Nếu không có lỗi eᵢ, bài toán trở thành giải hệ phương trình tuyến tính (dễ). Lỗi nhỏ eᵢ biến nó thành tìm vector gần nhất trên lattice (CVP) — NP-hard.

#### Ví dụ số học đơn giản

```
Tham số: n = 2, q = 17

Secret: s = (3, 5)

Sample 1: a₁ = (7, 11)
  b₁ = ⟨(7,11), (3,5)⟩ + e₁ mod 17
     = (21 + 55) + 2 mod 17
     = 78 mod 17 = 10

Sample 2: a₂ = (4, 9)
  b₂ = ⟨(4,9), (3,5)⟩ + e₂ mod 17
     = (12 + 45) + (-1) mod 17
     = 56 mod 17 = 5

Kẻ tấn công thấy:
  (a₁, b₁) = ((7,11), 10)
  (a₂, b₂) = ((4, 9),  5)

Mục tiêu: Tìm s = (3, 5) — KHÔNG KHẢ THI khi n lớn (n ≥ 256)
```

### 2.4 Ring-LWE — Biến thể hiệu quả

**Ring-LWE** hoạt động trên vành đa thức (polynomial ring) thay vì vector, giảm đáng kể kích thước khóa và tăng tốc tính toán.

#### Không gian toán học

```
Rq = ℤq[x] / (xⁿ + 1)

Trong đó:
  ℤq[x] = tập đa thức hệ số nguyên mod q
  xⁿ + 1 = đa thức bất khả quy (irreducible cyclotomic)
  n = power of 2 (thường n = 256, 512, 1024)
```

Mỗi phần tử trong Rq là đa thức bậc ≤ n-1:

```
f(x) = a₀ + a₁x + a₂x² + ... + aₙ₋₁xⁿ⁻¹    (aᵢ ∈ ℤq)
```

#### Bài toán Ring-LWE

```
Cho:
  a(x) ∈ Rq         — đa thức ngẫu nhiên (công khai)
  s(x) ∈ Rq         — đa thức bí mật (hệ số nhỏ)
  e(x) ← χ          — đa thức lỗi (hệ số nhỏ)

Tính:
  b(x) = a(x) · s(x) + e(x)  mod (xⁿ + 1, q)

Bài toán: Cho (a(x), b(x)), tìm s(x) — KHỐNG KHẢ THI
```

#### So sánh LWE vs Ring-LWE

| Tiêu chí | LWE (chuẩn) | Ring-LWE |
|---|---|---|
| Cấu trúc dữ liệu | Ma trận n×n | Đa thức bậc n |
| Kích thước public key | O(n²) | O(n) |
| Tốc độ nhân | O(n²) → O(n^ω) | O(n log n) via NTT |
| Mức an toàn | Dựa trên general lattice | Dựa trên ideal lattice |
| Sử dụng trong | Frodo (conservative) | Kyber, Dilithium |

### 2.5 Module-LWE — Nền tảng của Kyber/Dilithium

**Module-LWE (MLWE)** là trung gian giữa LWE và Ring-LWE, cung cấp sự linh hoạt trong điều chỉnh tham số bảo mật:

```
Thay vì:
  LWE:       A ∈ ℤq^(n×n),  s ∈ ℤqⁿ        (ma trận lớn)
  Ring-LWE:  a ∈ Rq,         s ∈ Rq          (1 phần tử ring)

Module-LWE: A ∈ Rq^(k×k),   s ∈ Rqᵏ         (ma trận nhỏ các phần tử ring)

k = 2: ML-KEM-512   (128-bit security)
k = 3: ML-KEM-768   (192-bit security)  ← Khuyến nghị cho Pluto
k = 4: ML-KEM-1024  (256-bit security)
```

### 2.6 Number Theoretic Transform (NTT)

NTT là "bí mật" để nhân đa thức trong Ring-LWE hiệu quả — tương đương FFT nhưng trên trường hữu hạn.

```
Nhân đa thức thông thường:  O(n²)
Nhân đa thức qua NTT:      O(n log n)

Quy trình:
  f(x), g(x) ∈ Rq
  
  1. NTT(f) → F̂,  NTT(g) → Ĝ           // Forward transform: O(n log n)
  2. Ĥ = F̂ ⊙ Ĝ                          // Pointwise multiply: O(n)
  3. h(x) = NTT⁻¹(Ĥ)                    // Inverse transform: O(n log n)
  
  Tổng: O(n log n) thay vì O(n²)
```

### 2.7 Ứng dụng nâng cao: Fully Homomorphic Encryption (FHE)

Ring-LWE là nền tảng cho **Mã hóa đồng cấu hoàn toàn** — cho phép tính toán trực tiếp trên dữ liệu đã mã hóa:

```
Encrypt(m₁) ⊕ Encrypt(m₂) = Encrypt(m₁ + m₂)
Encrypt(m₁) ⊗ Encrypt(m₂) = Encrypt(m₁ × m₂)
```

**Tiềm năng cho Pluto (tương lai xa):**
- **Private smart contracts**: Thực thi EVM trên dữ liệu mã hóa
- **Confidential voting**: Bỏ phiếu mà không tiết lộ lựa chọn, nhưng vẫn verify kết quả
- **Sealed-bid auctions**: Đấu giá kín trên blockchain

> **Lưu ý:** FHE hiện tại còn quá chậm cho production (10,000x - 1,000,000x overhead). Đây là hướng nghiên cứu dài hạn, không nên triển khai trong Phase 1-5.

---

## 3. Phần II — Post-Quantum Cryptography (PQC)

### 3.1 Chuẩn NIST PQC (Tháng 8/2024)

NIST đã chính thức công bố 3 chuẩn PQC đầu tiên:

| Chuẩn NIST | Tên thuật toán gốc | Tên NIST | Chức năng | Nền tảng toán học |
|---|---|---|---|---|
| **FIPS 203** | CRYSTALS-Kyber | **ML-KEM** | Key Encapsulation | Module-LWE |
| **FIPS 204** | CRYSTALS-Dilithium | **ML-DSA** | Digital Signature | Module-LWE + SIS |
| **FIPS 205** | SPHINCS+ | **SLH-DSA** | Digital Signature | Hash-based |
| *FIPS 206* (draft) | FALCON | *FN-DSA* | Digital Signature | NTRU Lattice |

### 3.2 ML-DSA (Dilithium) — Chi tiết cho Blockchain

ML-DSA là thuật toán chữ ký số phù hợp nhất cho blockchain nhờ cân bằng giữa kích thước, tốc độ và bảo mật.

#### Tham số

| Level | NIST Security | n | q | k | l | Khóa công khai | Chữ ký | Khóa bí mật |
|---|---|---|---|---|---|---|---|---|
| ML-DSA-44 | Level 2 (128-bit) | 256 | 8380417 | 4 | 4 | 1,312 bytes | 2,420 bytes | 2,560 bytes |
| ML-DSA-65 | Level 3 (192-bit) | 256 | 8380417 | 6 | 5 | 1,952 bytes | 3,293 bytes | 4,032 bytes |
| ML-DSA-87 | Level 5 (256-bit) | 256 | 8380417 | 8 | 7 | 2,592 bytes | 4,595 bytes | 4,896 bytes |

#### Quy trình ký (Simplified)

```
KeyGen():
  1. Sinh secret vectors s₁ ∈ Rqˡ, s₂ ∈ Rqᵏ (hệ số nhỏ)
  2. Sinh ma trận A ∈ Rq^(k×l) (từ seed ngẫu nhiên)
  3. Tính t = A · s₁ + s₂
  4. Public key: pk = (A, t)
  5. Secret key: sk = (A, t, s₁, s₂)

Sign(sk, message):
  1. Chọn vector che (masking) y ← uniform trong [-γ₁, γ₁]ˡ
  2. Tính w = A · y
  3. Tính challenge c = H(HighBits(w), message)
  4. Tính z = y + c · s₁
  5. Kiểm tra: ‖z‖∞ < γ₁ - β VÀ ‖hints‖ nhỏ
     Nếu KHÔNG thỏa → quay lại bước 1 (rejection sampling)
  6. Signature: σ = (z, hints, c)

Verify(pk, message, σ):
  1. Tính w' = A · z - c · t
  2. Kiểm tra c == H(HighBits(w'), message)
  3. Kiểm tra ‖z‖∞ < γ₁ - β
  4. Nếu tất cả thỏa → ACCEPT
```

#### So sánh kích thước với thuật toán hiện tại

```
                    Public Key    Signature     Tổng (trên chain)
  ECDSA secp256k1:   33 bytes      64 bytes       97 bytes
  Ed25519:           32 bytes      64 bytes       96 bytes
  ML-DSA-44:      1,312 bytes   2,420 bytes    3,732 bytes  (38x lớn hơn)
  ML-DSA-65:      1,952 bytes   3,293 bytes    5,245 bytes  (54x lớn hơn)
  FALCON-512:       897 bytes     666 bytes    1,563 bytes  (16x lớn hơn)
  SLH-DSA-128s:      32 bytes   7,856 bytes    7,888 bytes  (82x lớn hơn)
```

### 3.3 ML-KEM (Kyber) — Key Encapsulation

Dùng cho trao đổi khóa an toàn (thay thế ECDH):

```
Quy trình trao đổi khóa:

  Alice                                Bob
    │                                   │
    │─── pk (public key) ──────────────→│
    │                                   │
    │                    Encapsulate(pk) → (ciphertext, shared_secret)
    │                                   │
    │←──── ciphertext ─────────────────│
    │                                   │
    │ Decapsulate(sk, ciphertext)       │
    │ → shared_secret                   │
    │                                   │
    │ Cả hai có cùng shared_secret     │
```

**Ứng dụng trong Pluto:**
- **P2P encryption**: Bảo vệ kênh giao tiếp giữa validators
- **Encrypted mempool** (tương lai): Mã hóa tx trước khi broadcast

### 3.4 SLH-DSA (SPHINCS+) — Hash-based Signature

Ưu điểm: **không dựa trên giả thuyết lattice** — bảo mật dựa hoàn toàn trên hash function.

```
Ưu điểm:
  ✅ Conservative — chỉ cần hash an toàn
  ✅ Public key nhỏ nhất (32-64 bytes)
  ✅ Backup plan nếu lattice bị phá

Nhược điểm:
  ❌ Chữ ký KHỔNG LỒ (7,856 - 49,856 bytes)
  ❌ Ký chậm hơn Dilithium ~10x
```

**Vai trò trong Pluto:** Dùng làm **fallback signature** — nếu lattice-based bị phát hiện lỗ hổng, chuyển sang SLH-DSA ngay lập tức mà không cần hard fork.

### 3.5 Hybrid Signature Scheme cho Pluto

#### Thiết kế

```
Pluto Hybrid Signature = ECDSA(secp256k1) ‖ ML-DSA-44(message)

Cấu trúc transaction mới:
┌──────────────────────────────────────────────────────────────┐
│ EVM Transaction (RLP encoded)                                │
├──────────────────────────────────────────────────────────────┤
│ ECDSA Signature (v, r, s)              │    65 bytes         │
├──────────────────────────────────────────────────────────────┤
│ PQC Signature (ML-DSA-44)              │ 2,420 bytes         │
├──────────────────────────────────────────────────────────────┤
│ PQC Public Key (compressed)            │ 1,312 bytes         │
└──────────────────────────────────────────────────────────────┘

Tổng overhead thêm: ~3,732 bytes/tx
```

#### Verify Logic

```go
func VerifyHybridSignature(tx *HybridTransaction) error {
    // 1. Verify ECDSA (backward compatible)
    if err := verifyECDSA(tx); err != nil {
        return fmt.Errorf("ECDSA verification failed: %w", err)
    }

    // 2. Verify ML-DSA (quantum-resistant)
    if tx.PQCSignature != nil {
        if err := verifyMLDSA(tx); err != nil {
            return fmt.Errorf("ML-DSA verification failed: %w", err)
        }
    }

    // Cả hai phải PASS
    return nil
}
```

#### Chiến lược triển khai theo giai đoạn

```
Phase A: "Optional PQC" (hiện tại)
  - ECDSA: BẮT BUỘC
  - ML-DSA: TÙY CHỌN (nếu có → verify, nếu không → bỏ qua)
  - Mục đích: Cho phép thử nghiệm, backward compatible

Phase B: "Recommended PQC" (6 tháng sau)
  - ECDSA: BẮT BUỘC
  - ML-DSA: KHUYẾN KHÍCH (giảm gas 20% nếu có PQC signature)

Phase C: "Mandatory PQC" (12+ tháng sau)
  - ECDSA: TÙY CHỌN
  - ML-DSA: BẮT BUỘC
  - Hard fork: reject tx không có PQC signature
```

### 3.6 PQC Precompile Contract

Thêm precompile tại địa chỉ cố định để smart contracts có thể verify PQC signatures on-chain:

```
Precompile Addresses:
  0x0000000000000000000000000000000000000300  → VerifyMLDSA(pubkey, message, signature) returns bool
  0x0000000000000000000000000000000000000301  → VerifyFALCON(pubkey, message, signature) returns bool
  0x0000000000000000000000000000000000000302  → MLKEMEncapsulate(pubkey) returns (ciphertext, sharedSecret)
```

#### Gas Cost Estimation

```
Operation                    Gas Cost (estimated)
─────────────────────────   ────────────────────
VerifyMLDSA-44               3,000 gas     (benchmark: ~0.14ms)
VerifyMLDSA-65               4,500 gas
VerifyFALCON-512             5,000 gas     (do FFT complexity)
VerifySLHDSA-128s           15,000 gas     (do hash chain depth)
MLKEMEncapsulate-768         2,000 gas
ecrecover (ECDSA, hiện tại) 3,000 gas     (EIP-150)
```

### 3.7 Implementation trong Go

#### Thư viện có sẵn

```
1. Go Standard Library (Go 1.24+):
   crypto/mlkem     — ML-KEM (Kyber) key encapsulation
                      ✅ Production-ready, NIST-compliant

2. Cloudflare CIRCL:
   github.com/cloudflare/circl
   - sign/dilithium  — ML-DSA (Dilithium) tất cả levels
   - kem/kyber       — ML-KEM (Kyber) tất cả levels
   - sign/ed448      — Ed448 (backup classical)
   ✅ Battle-tested (dùng trong Cloudflare production)

3. liboqs-go (Open Quantum Safe):
   github.com/open-quantum-safe/liboqs-go
   - Wrapper cho liboqs C library
   - Hỗ trợ TOÀN BỘ NIST PQC candidates
   ⚠️ CGO dependency — phức tạp hơn khi cross-compile
```

#### Code mẫu: ML-DSA Sign/Verify với CIRCL

```go
package pqc

import (
    "crypto/rand"
    "fmt"

    "github.com/cloudflare/circl/sign/dilithium/mode2" // ML-DSA-44
)

// PQCKeyPair chứa cặp khóa ML-DSA
type PQCKeyPair struct {
    PublicKey  *mode2.PublicKey
    PrivateKey *mode2.PrivateKey
}

// GenerateKeyPair tạo cặp khóa ML-DSA-44 mới
func GenerateKeyPair() (*PQCKeyPair, error) {
    pub, priv, err := mode2.GenerateKey(rand.Reader)
    if err != nil {
        return nil, fmt.Errorf("ML-DSA keygen failed: %w", err)
    }
    return &PQCKeyPair{PublicKey: pub, PrivateKey: priv}, nil
}

// Sign ký message bằng ML-DSA-44
func (kp *PQCKeyPair) Sign(message []byte) []byte {
    signature := mode2.SignTo(nil, kp.PrivateKey, message)
    return signature
}

// Verify xác minh chữ ký ML-DSA-44
func Verify(publicKey *mode2.PublicKey, message, signature []byte) bool {
    return mode2.Verify(publicKey, message, signature)
}

// SerializePublicKey chuyển public key thành bytes để lưu on-chain
func SerializePublicKey(pub *mode2.PublicKey) []byte {
    var buf [mode2.PublicKeySize]byte
    pub.Pack(&buf)
    return buf[:]
}

// DeserializePublicKey khôi phục public key từ bytes
func DeserializePublicKey(data []byte) (*mode2.PublicKey, error) {
    if len(data) != mode2.PublicKeySize {
        return nil, fmt.Errorf("invalid public key size: got %d, want %d",
            len(data), mode2.PublicKeySize)
    }
    var pub mode2.PublicKey
    var buf [mode2.PublicKeySize]byte
    copy(buf[:], data)
    pub.Unpack(&buf)
    return &pub, nil
}
```

#### Code mẫu: ML-KEM Key Exchange với Go Standard Library

```go
package pqc

import (
    "crypto/mlkem"
    "fmt"
)

// QuantumSafeKeyExchange thực hiện trao đổi khóa ML-KEM-768
func QuantumSafeKeyExchange() error {
    // Alice tạo key pair
    decapsKey, err := mlkem.GenerateKeyMLKEM768()
    if err != nil {
        return fmt.Errorf("keygen failed: %w", err)
    }
    encapsKey := decapsKey.EncapsulationKey()

    // Bob encapsulate (tạo shared secret + ciphertext)
    ciphertext, sharedSecretBob := encapsKey.Encapsulate()

    // Alice decapsulate (khôi phục shared secret)
    sharedSecretAlice, err := decapsKey.Decapsulate(ciphertext)
    if err != nil {
        return fmt.Errorf("decapsulate failed: %w", err)
    }

    // Kiểm tra: cả hai phải có cùng shared secret
    if string(sharedSecretAlice) != string(sharedSecretBob) {
        return fmt.Errorf("shared secret mismatch")
    }

    // sharedSecret có thể dùng làm symmetric key (AES-256-GCM)
    fmt.Printf("Shared secret length: %d bytes\n", len(sharedSecretAlice))
    return nil
}
```

---

## 4. Phần III — Quantum Random Number Generation (QRNG)

### 4.1 Tại sao PRNG không đủ cho Blockchain?

```
Pseudo-Random Number Generator (PRNG):
  - Deterministic: cùng seed → cùng output
  - Có thể dự đoán nếu biết state
  - Entropy ban đầu có hạn

Quantum Random Number Generator (QRNG):
  - Dựa trên hiện tượng vật lý lượng tử (quantum vacuum fluctuations)
  - KHÔNG THỂ dự đoán — theo nguyên lý bất định Heisenberg
  - Entropy vô hạn (mỗi phép đo tạo entropy mới)
```

### 4.2 Nguyên lý vật lý

#### Quantum Vacuum Fluctuations (ANU)

```
Beam Splitter Setup:

  Laser ──→ [Beam Splitter] ──→ Detector 1 (D₁)
                    │
                    └──────→ Detector 2 (D₂)

  Mỗi photon có 50/50 xác suất đi lên hoặc xuống
  → Kết quả là TRUE RANDOM (theo cơ học lượng tử)

  D₁ = "0", D₂ = "1"
  → Chuỗi bit: 01101001 11010100 10110011 ...
```

#### Quantum Superposition (IDQuantique)

```
Single Photon Source → [Beam Splitter] → Detector

  |ψ⟩ = α|0⟩ + β|1⟩    (superposition)

  Khi đo: collapse → |0⟩ hoặc |1⟩
  Xác suất |α|² và |β|² là fundamental random
  → Không thuật toán nào dự đoán được
```

### 4.3 Các nguồn QRNG có sẵn

| Provider | Loại | Giao thức | Throughput | Giá |
|---|---|---|---|---|
| **ANU QRNG** (qrng.anu.edu.au) | Cloud API | HTTPS REST | ~5 Mbps | Miễn phí (giới hạn rate) |
| **API3 QRNG** | Oracle (on-chain) | Airnode | Per-request | Chỉ trả gas fee |
| **ID Quantique Quantis** | Hardware (PCIe/USB) | Local driver | 4-240 Mbps | $2,000-$50,000 |
| **QRNG Open API** | Standard API | REST | Depends | Depends |

### 4.4 Vấn đề hiện tại của Pluto với Randomness

Trong [DESIGN.md](DESIGN.md), Section 5 — EVM-on-CometBFT Compatibility:

```go
// Hiện tại: KHÔNG AN TOÀN
blockCtx.Random = &blockHeader.Hash()  // ← Predictable!

// Thiết kế: VRF qua ExtendVote (chưa implement)
// Mỗi validator submit VRF proof → aggregate trong PrepareProposal
```

**Vấn đề:**
1. `blockHeader.Hash()` có thể dự đoán (leader biết trước)
2. VRF truyền thống (dùng ECDSA/Ed25519) bị ảnh hưởng bởi quantum
3. Chưa có cơ chế aggregate randomness từ nhiều validators

### 4.5 Thiết kế QRNG cho Pluto

#### Kiến trúc: QRNG-Seeded VRF qua ExtendVote

```
Flow mỗi block:

  Validator 1                 Validator 2                 Validator 3
      │                           │                           │
  ┌───┴───┐                   ┌───┴───┐                   ┌───┴───┐
  │ QRNG  │                   │ QRNG  │                   │ QRNG  │
  │ Seed  │                   │ Seed  │                   │ Seed  │
  └───┬───┘                   └───┬───┘                   └───┬───┘
      │                           │                           │
  ┌───┴───────┐               ┌───┴───────┐               ┌───┴───────┐
  │ VRF_prove │               │ VRF_prove │               │ VRF_prove │
  │ (sk, seed)│               │ (sk, seed)│               │ (sk, seed)│
  └───┬───────┘               └───┴───────┘               └───┬───────┘
      │                           │                           │
      │  ExtendVote               │  ExtendVote               │  ExtendVote
      │  {vrf_output,             │  {vrf_output,             │  {vrf_output,
      │   vrf_proof,              │   vrf_proof,              │   vrf_proof,
      │   qrng_commitment}        │   qrng_commitment}        │   qrng_commitment}
      │                           │                           │
      └───────────┬───────────────┴───────────────────────────┘
                  │
           ┌──────┴──────┐
           │ Proposer:   │
           │ Aggregate   │
           │ XOR all     │
           │ VRF outputs │
           └──────┬──────┘
                  │
           Block.Random = XOR(vrf₁, vrf₂, vrf₃)
```

#### Tính chất bảo mật

```
1. QRNG seed: Không thể dự đoán (quantum physics)
2. VRF prove: Có thể verify mà không biết secret key
3. XOR aggregate: Chỉ cần 1 validator trung thực → kết quả random
4. Commitment: Ngăn validator thay đổi VRF sau khi thấy kết quả khác
```

#### Implementation: QRNG Client

```go
package qrng

import (
    "crypto/rand"
    "encoding/json"
    "fmt"
    "io"
    "net/http"
    "time"
)

// QRNGSource interface cho các nguồn entropy lượng tử
type QRNGSource interface {
    // GetEntropy trả về n bytes entropy từ nguồn QRNG
    GetEntropy(n int) ([]byte, error)
}

// ANUQuantumSource lấy entropy từ ANU Quantum Random API
type ANUQuantumSource struct {
    client  *http.Client
    baseURL string
}

func NewANUSource() *ANUQuantumSource {
    return &ANUQuantumSource{
        client: &http.Client{Timeout: 5 * time.Second},
        baseURL: "https://qrng.anu.edu.au/API/jsonI.php",
    }
}

type anuResponse struct {
    Type    string   `json:"type"`
    Length  int      `json:"length"`
    Data    []uint8  `json:"data"`
    Success bool     `json:"success"`
}

func (s *ANUQuantumSource) GetEntropy(n int) ([]byte, error) {
    url := fmt.Sprintf("%s?length=%d&type=uint8", s.baseURL, n)

    resp, err := s.client.Get(url)
    if err != nil {
        // Fallback: dùng crypto/rand nếu API không khả dụng
        return fallbackEntropy(n)
    }
    defer resp.Body.Close()

    body, err := io.ReadAll(resp.Body)
    if err != nil {
        return fallbackEntropy(n)
    }

    var result anuResponse
    if err := json.Unmarshal(body, &result); err != nil {
        return fallbackEntropy(n)
    }

    if !result.Success || len(result.Data) < n {
        return fallbackEntropy(n)
    }

    return result.Data[:n], nil
}

// HybridSource kết hợp QRNG với local entropy (defense-in-depth)
type HybridSource struct {
    quantum QRNGSource
}

func NewHybridSource(quantum QRNGSource) *HybridSource {
    return &HybridSource{quantum: quantum}
}

func (s *HybridSource) GetEntropy(n int) ([]byte, error) {
    // Lấy entropy từ cả hai nguồn
    qEntropy, _ := s.quantum.GetEntropy(n)
    lEntropy, err := fallbackEntropy(n)
    if err != nil {
        return nil, fmt.Errorf("local entropy failed: %w", err)
    }

    // XOR hai nguồn: an toàn miễn là 1 trong 2 tốt
    result := make([]byte, n)
    for i := 0; i < n; i++ {
        if qEntropy != nil && i < len(qEntropy) {
            result[i] = lEntropy[i] ^ qEntropy[i]
        } else {
            result[i] = lEntropy[i]
        }
    }
    return result, nil
}

// fallbackEntropy sử dụng OS entropy (crypto/rand)
func fallbackEntropy(n int) ([]byte, error) {
    buf := make([]byte, n)
    _, err := rand.Read(buf)
    return buf, err
}
```

#### Implementation: QRNG-VRF trong ExtendVote

```go
package consensus

import (
    "crypto/sha256"

    abci "github.com/cometbft/cometbft/abci/types"
)

// ExtendVote — CometBFT ABCI++ callback
// Được gọi khi validator chuẩn bị vote cho 1 block
func (app *App) ExtendVote(ctx context.Context,
    req *abci.ExtendVoteRequest) (*abci.ExtendVoteResponse, error) {

    // 1. Lấy QRNG entropy (32 bytes)
    qrngSeed, err := app.qrngSource.GetEntropy(32)
    if err != nil {
        app.logger.Error("QRNG failed, using local entropy", "err", err)
    }

    // 2. Tạo VRF input = H(block_height ‖ qrng_seed)
    vrfInput := sha256.Sum256(append(
        encodeUint64(uint64(req.Height)),
        qrngSeed...,
    ))

    // 3. Tính VRF proof (dùng validator's private key)
    vrfOutput, vrfProof := app.vrfKey.Prove(vrfInput[:])

    // 4. Tạo commitment cho QRNG seed (để verify sau)
    commitment := sha256.Sum256(qrngSeed)

    // 5. Encode vote extension
    extension := VoteExtension{
        VRFOutput:      vrfOutput,
        VRFProof:       vrfProof,
        QRNGCommitment: commitment[:],
    }

    return &abci.ExtendVoteResponse{
        VoteExtension: extension.Marshal(),
    }, nil
}

// VerifyVoteExtension — Verify extension từ validator khác
func (app *App) VerifyVoteExtension(ctx context.Context,
    req *abci.VerifyVoteExtensionRequest) (*abci.VerifyVoteExtensionResponse, error) {

    var ext VoteExtension
    if err := ext.Unmarshal(req.VoteExtension); err != nil {
        return &abci.VerifyVoteExtensionResponse{
            Status: abci.VERIFY_VOTE_EXTENSION_STATUS_REJECT,
        }, nil
    }

    // Verify VRF proof
    validatorPubKey := app.getValidatorPubKey(req.ValidatorAddress)
    if !validatorPubKey.VRFVerify(ext.VRFOutput, ext.VRFProof) {
        return &abci.VerifyVoteExtensionResponse{
            Status: abci.VERIFY_VOTE_EXTENSION_STATUS_REJECT,
        }, nil
    }

    return &abci.VerifyVoteExtensionResponse{
        Status: abci.VERIFY_VOTE_EXTENSION_STATUS_ACCEPT,
    }, nil
}

// PrepareProposal — Aggregate VRF outputs thành block randomness
func (app *App) aggregateRandomness(extensions []VoteExtension) [32]byte {
    var result [32]byte

    for _, ext := range extensions {
        for i := 0; i < 32; i++ {
            result[i] ^= ext.VRFOutput[i]   // XOR aggregate
        }
    }

    return result
}
```

### 4.6 Lưu ý quan trọng khi dùng QRNG trong Blockchain

> **Consensus requires determinism.** QRNG chỉ được dùng làm **seed input** cho VRF, KHÔNG BAO GIỜ dùng trực tiếp trong EVM execution.

```
 ✅ ĐÚNG: QRNG → seed → VRF → deterministic block random
 ❌ SAI:  QRNG → trực tiếp vào EVM → KHÔNG DETERMINISTIC
                                     → Validators có kết quả khác nhau
                                     → Consensus FAIL
```

**Quy tắc:**
1. QRNG entropy chỉ ở tầng **ExtendVote** (pre-consensus)
2. Sau khi aggregate → kết quả là **deterministic** cho tất cả validators
3. Giá trị cuối cùng ghi vào Block Header → EVM đọc từ đó
4. Luôn có **fallback** sang `crypto/rand` nếu QRNG API down

---

## 5. Triển khai trong Pluto Blockchain

### 5.1 Tổng quan kiến trúc PQC + QRNG

```
┌─────────────────────────────────────────────────────────────────┐
│                     PLUTO NODE                                  │
│                                                                 │
│  ┌──────────────┐  ┌──────────────┐  ┌───────────────────────┐ │
│  │  QRNG Client │  │  PQC Module  │  │  Existing Pluto Core  │ │
│  │              │  │              │  │                       │ │
│  │  ANU API ────│  │  ML-DSA ─────│  │  CometBFT ABCI++     │ │
│  │  Hybrid      │  │  ML-KEM      │  │  Parallel EVM        │ │
│  │  Fallback    │  │  SLH-DSA     │  │  PebbleDB            │ │
│  └──────┬───────┘  └──────┬───────┘  └───────────┬───────────┘ │
│         │                 │                       │             │
│  ┌──────┴─────────────────┴───────────────────────┴──────────┐ │
│  │                    Integration Layer                       │ │
│  │                                                           │ │
│  │  ExtendVote:     QRNG seed → VRF → block randomness      │ │
│  │  CheckTx:        Verify hybrid signature (ECDSA + PQC)    │ │
│  │  FinalizeBlock:  PQC precompile execution                 │ │
│  │  PrepareProposal: Aggregate VRF outputs                   │ │
│  └───────────────────────────────────────────────────────────┘ │
└─────────────────────────────────────────────────────────────────┘
```

### 5.2 Thay đổi cấu trúc dự án

```
internal/
├── app/
│   ├── app.go              ← Thêm PQC verify vào CheckTx, FinalizeBlock
│   └── handlers.go
├── evm/
│   ├── state.go
│   ├── decoder.go
│   ├── vm_runner.go
│   └── precompiles/
│       └── pqc_verify.go   ← [NEW] PQC signature verify precompile
├── store/
│   └── pebble.go
├── pqc/                    ← [NEW] Post-Quantum Cryptography module
│   ├── mldsa.go            ← ML-DSA sign/verify
│   ├── mlkem.go            ← ML-KEM key exchange
│   ├── hybrid.go           ← Hybrid signature scheme
│   └── pqc_test.go         ← Benchmark & tests
└── qrng/                   ← [NEW] Quantum Random Number Generation
    ├── source.go           ← QRNG source interface
    ├── anu.go              ← ANU QRNG API client
    ├── hybrid.go           ← Hybrid entropy (QRNG + local)
    └── qrng_test.go        ← Tests
```

### 5.3 Tích hợp vào ABCI++ Methods

#### CheckTx — Verify Hybrid Signature

```go
func (app *App) CheckTx(ctx context.Context,
    req *abci.CheckTxRequest) (*abci.CheckTxResponse, error) {

    ethTx, err := app.txProcessor.DecodeTx(req.Tx)
    if err != nil {
        return &abci.CheckTxResponse{Code: 1, Log: "decode error"}, nil
    }

    // 1. Verify ECDSA (bắt buộc)
    _, err = app.txProcessor.RecoverSender(ethTx)
    if err != nil {
        return &abci.CheckTxResponse{Code: 1, Log: "invalid ECDSA"}, nil
    }

    // 2. Verify PQC (tùy chọn trong Phase A)
    if pqcSig := extractPQCSignature(ethTx); pqcSig != nil {
        if !pqc.VerifyMLDSA(pqcSig.PublicKey, ethTx.Hash().Bytes(), pqcSig.Signature) {
            return &abci.CheckTxResponse{Code: 1, Log: "invalid PQC signature"}, nil
        }
    }

    return &abci.CheckTxResponse{Code: 0}, nil
}
```

#### FinalizeBlock — Sử dụng Block Random

```go
func (app *App) FinalizeBlock(ctx context.Context,
    req *abci.FinalizeBlockRequest) (*abci.FinalizeBlockResponse, error) {

    // Lấy aggregated randomness từ vote extensions
    blockRandom := app.aggregateRandomness(req.DecidedLastCommit)

    blockContext := vm.BlockContext{
        // ... existing fields ...
        Random: &blockRandom,   // ← QRNG-seeded VRF result
    }

    // ... rest of execution ...
}
```

---

## 6. Đánh giá hiệu năng & Trade-offs

### 6.1 So sánh hiệu năng chữ ký

| Thuật toán | KeyGen (μs) | Sign (μs) | Verify (μs) | Sig Size (bytes) | PubKey Size (bytes) |
|---|---|---|---|---|---|
| ECDSA secp256k1 | ~50 | ~45 | ~90 | 64 | 33 |
| Ed25519 | ~15 | ~20 | ~40 | 64 | 32 |
| **ML-DSA-44** | ~150 | ~400 | ~140 | 2,420 | 1,312 |
| **ML-DSA-65** | ~250 | ~600 | ~230 | 3,293 | 1,952 |
| FALCON-512 | ~8,000 | ~400 | ~100 | 666 | 897 |
| SLH-DSA-128s | ~3,000,000 | ~50,000 | ~5,000 | 7,856 | 32 |

### 6.2 Ảnh hưởng đến Transaction Size

```
Current Pluto Tx (ECDSA only):
  RLP data: ~110 bytes (trung bình)
  ECDSA sig: 65 bytes
  Total: ~175 bytes

Hybrid Tx (ECDSA + ML-DSA-44):
  RLP data: ~110 bytes
  ECDSA sig: 65 bytes
  ML-DSA sig: 2,420 bytes
  ML-DSA pubkey: 1,312 bytes
  Total: ~3,907 bytes (22x lớn hơn)

Ảnh hưởng:
  Block size (30M gas, ~150 tx/block):
    Hiện tại: ~26 KB/block
    Hybrid:   ~586 KB/block
    → Vẫn OK cho CometBFT (max 21MB/block mặc định)
```

### 6.3 Ảnh hưởng đến Block Time

```
Verify 150 tx/block:
  ECDSA only:   150 × 90μs  = 13.5ms
  Hybrid:       150 × (90 + 140)μs = 34.5ms
  → Thêm ~21ms/block — CHẤP NHẬN ĐƯỢC (block time 1-6s)

QRNG API call:
  Latency: 50-200ms (ANU API)
  → OK vì chỉ gọi 1 lần/block trong ExtendVote
  → Không block consensus (async với fallback)
```

### 6.4 Trade-off Matrix

| Tiêu chí | Chỉ ECDSA | + ML-DSA-44 | + FALCON-512 | + SLH-DSA |
|---|---|---|---|---|
| Quantum-resistant | ❌ | ✅ | ✅ | ✅ |
| Tx size overhead | 1x | 22x | 9x | 45x |
| Verify speed | Nhanh | Nhanh | Nhanh | Chậm |
| Maturity | Production | NIST 2024 | NIST draft | NIST 2024 |
| **Khuyến nghị** | Hiện tại | **✅ Ưu tiên** | Backup | Fallback |

---

## 7. Lộ trình triển khai

### 7.1 Timeline

```
                                         Pluto Main Phases
                  ─────────────────────────────────────────────────
                  Phase 1-3        Phase 4         Phase 5         Phase 6
                  ABCI++ Core      JSON-RPC        Parallel EVM    Advanced
                  ───────────      ────────        ────────────    ────────

Quantum Module:
  ┌─────────┐
  │ QM-1    │  Thêm internal/pqc package
  │ Research│  Benchmark ML-DSA vs ECDSA trên Pluto hardware
  │ 2 tuần  │  Viết paper/báo cáo KHCN
  └────┬────┘
       │
       ▼
  ┌─────────┐
  │ QM-2    │  Hybrid signature trong CheckTx (optional PQC)
  │ PQC     │  PQC precompile cho smart contracts
  │ 3 tuần  │  Unit tests + integration tests
  └────┬────┘
       │
       ▼
  ┌─────────┐
  │ QM-3    │  QRNG client + ExtendVote integration
  │ QRNG    │  VRF với QRNG seed
  │ 2 tuần  │  Block randomness pipeline
  └────┬────┘
       │
       ▼
  ┌─────────┐
  │ QM-4    │  Mandatory PQC signatures
  │ Harden  │  PQC-protected P2P (ML-KEM)
  │ 3 tuần  │  Security audit, load testing
  └─────────┘
```

### 7.2 Ưu tiên cho sản phẩm KHCN

```
PHẢI CÓ (cho đăng ký)          NÊN CÓ                CÓ THỂ SAU
─────────────────────           ──────                 ──────────
PQC module (ML-DSA)             QRNG integration       FHE research
Hybrid signature scheme         PQC precompile         Quantum-safe P2P
Benchmark report                Block randomness       Cross-chain PQC
Threat analysis paper           Fallback SLH-DSA       ML-KEM encrypted mempool
```

---

## 8. Tài liệu tham khảo

### Chuẩn NIST
1. FIPS 203 — ML-KEM (Module-Lattice-Based Key-Encapsulation Mechanism)
2. FIPS 204 — ML-DSA (Module-Lattice-Based Digital Signature Algorithm)
3. FIPS 205 — SLH-DSA (Stateless Hash-Based Digital Signature Algorithm)

### Nền tảng toán học
4. O. Regev, "On Lattices, Learning with Errors, Random Linear Codes, and Cryptography," STOC 2005
5. V. Lyubashevsky, C. Peikert, O. Regev, "On Ideal Lattices and Learning with Errors Over Rings," Eurocrypt 2010
6. Alfred Menezes, "Mathematics of Lattice-Based Cryptography," cryptography101.ca

### Thư viện Implementation
7. Cloudflare CIRCL — github.com/cloudflare/circl
8. Go Standard Library crypto/mlkem — Go 1.24+
9. Open Quantum Safe liboqs — github.com/open-quantum-safe/liboqs

### QRNG
10. ANU Quantum Random Numbers Server — qrng.anu.edu.au
11. API3 QRNG — docs.api3.org/qrng
12. ID Quantique — idquantique.com

### Blockchain + PQC
13. "Post-Quantum Blockchain Security: A Comprehensive Survey," Frontiers in Blockchain, 2024
14. "Hybrid Post-Quantum Signatures for EVM Blockchains," CEUR Workshop Proceedings
15. Ethereum Foundation, "EIP-7693: Post-Quantum Account Abstraction" (draft)
