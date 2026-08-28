package tx

// OptInHybridValidator cho phép migration từng account mà không phá khả năng
// tương thích MetaMask của toàn chain:
//
//   - sender chưa đăng ký PQ key: chấp nhận raw Ethereum transaction;
//   - sender đã đăng ký PQ key: bắt buộc HybridEnvelope + ML-DSA-65.
//
// Cách này an toàn hơn policy "PQC optional" đơn thuần. Nếu account đã opt-in
// mà vẫn cho phép fallback ECDSA, quantum attacker có thể bỏ PQ signature và
// làm mất toàn bộ lợi ích của việc đăng ký key.
type OptInHybridValidator struct {
	ethereumValidator TransactionValidator
	hybridValidator   *HybridValidator
	keyResolver       PQKeyResolver
}

func NewOptInHybridValidator(
	ethereumValidator TransactionValidator,
	hybridValidator *HybridValidator,
	keyResolver PQKeyResolver,
) (*OptInHybridValidator, error) {
	if ethereumValidator == nil {
		return nil, ErrNilEthereumValidator
	}
	if hybridValidator == nil {
		return nil, ErrNilPQScheme
	}
	if keyResolver == nil {
		return nil, ErrNilPQKeyResolver
	}
	return &OptInHybridValidator{
		ethereumValidator: ethereumValidator,
		hybridValidator:   hybridValidator,
		keyResolver:       keyResolver,
	}, nil
}

func (v *OptInHybridValidator) Validate(raw []byte) (*ValidatedTransaction, error) {
	if v == nil {
		return nil, newValidationError(FailurePQC, "opt-in hybrid validator is nil")
	}

	// Thử format Ethereum chuẩn trước để MetaMask không phải hiểu envelope.
	validated, ethereumErr := v.ethereumValidator.Validate(raw)
	if ethereumErr == nil {
		_, registered, err := v.keyResolver.ExpectedKeyHash(validated.Sender)
		if err != nil {
			return nil, newValidationError(FailurePQC, "resolve post-quantum policy for %s: %w", validated.Sender.Hex(), err)
		}
		if registered {
			return nil, newValidationError(FailurePQC, "%w: %s", ErrPQRequiredForSender, validated.Sender.Hex())
		}
		return validated, nil
	}

	// Nếu không phải raw Ethereum transaction hợp lệ, chỉ còn đường hybrid.
	// HybridValidator tự kiểm tra ECDSA, key binding và ML-DSA đầy đủ.
	return v.hybridValidator.Validate(raw)
}
