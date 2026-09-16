package tx

import "errors"

var (
	ErrNilEnvelope                = errors.New("hybrid envelope is nil")
	ErrEmptyEnvelope              = errors.New("hybrid envelope is empty")
	ErrEnvelopeTooLarge           = errors.New("hybrid envelope exceeds maximum size")
	ErrMalformedEnvelope          = errors.New("malformed hybrid envelope")
	ErrNonCanonicalEnvelope       = errors.New("hybrid envelope is not canonical RLP")
	ErrUnsupportedEnvelopeVersion = errors.New("unsupported hybrid envelope version")
	ErrUnsupportedAlgorithm       = errors.New("unsupported post-quantum algorithm")
	ErrInvalidEthereumTxSize      = errors.New("invalid Ethereum transaction size")
	ErrInvalidPQPublicKeySize     = errors.New("invalid post-quantum public key size")
	ErrInvalidPQSignatureSize     = errors.New("invalid post-quantum signature size")
	ErrInvalidChainID             = errors.New("invalid EVM chain ID")
	ErrInvalidPQKeyHash           = errors.New("invalid post-quantum public key hash")
	ErrPQKeyNotRegistered         = errors.New("post-quantum public key is not registered for sender")
	ErrPQKeyBindingMismatch       = errors.New("post-quantum public key does not match sender registration")
	ErrPQRequiredForSender        = errors.New("sender registered a post-quantum key and must use a hybrid transaction")
	ErrNilPQKeyResolver           = errors.New("post-quantum key resolver is nil")
	ErrNilPQScheme                = errors.New("post-quantum signature scheme is nil")
	ErrNilEthereumValidator       = errors.New("Ethereum transaction validator is nil")
	ErrInvalidPQKeyAddress        = errors.New("invalid address in post-quantum key registry")
	ErrDuplicatePQKeyBinding      = errors.New("duplicate post-quantum key binding")
)
