# Module PQC

Đây là module hậu lượng tử độc lập của Pluto. Node chỉ sử dụng module khi
composition root chọn policy PQC; module transaction nền không import ngược
PQC.

- Package gốc `modules/pqc` chứa contract `Scheme` và ML-DSA-65.
- `modules/pqc/tx` chứa wire format hybrid, sign bytes, key registry và các
  validator strict/opt-in.
- `modules/pqc/proxy` là adapter client-side cho MetaMask; private key không đi
  vào validator node.

Ranh giới phụ thuộc: `pqc/tx` có thể dùng contract của `modules/transaction`,
nhưng `modules/transaction` không được phụ thuộc `modules/pqc`.
