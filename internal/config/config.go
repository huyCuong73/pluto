package config

const (
	// DefaultCometChainID identifies the local Pluto consensus network.
	DefaultCometChainID = "pluto-local-1"

	// DefaultEVMChainID is intentionally different from Ethereum mainnet (1)
	// to prevent accidental cross-chain transaction replay.
	DefaultEVMChainID int64 = 700001
)
