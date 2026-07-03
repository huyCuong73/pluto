package evm

import (

	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// các methods tính toán thuần tuý của CPU

type TxProcessor struct {
	chainID *big.Int
}

func NewTxProcessor(chainID int64) *TxProcessor {
	return &TxProcessor{
		chainID: big.NewInt(chainID),
	}
}


// giải mã RLP bytes thành ETH transaction
func (tp *TxProcessor) DecodeTx(txBytes []byte) (*types.Transaction, error) {
	tx := &types.Transaction{}
	if err := tx.UnmarshalBinary(txBytes); err != nil {
		return nil, fmt.Errorf("failed to decode rlp: %v", err)
	}
	return tx, nil
}


//lấy địa chỉ người gửi từ transaction (msg.sender)
func (tp *TxProcessor) RecoverSender(tx *types.Transaction) (common.Address,error){

	// Sử dụng EIP-155 Signer nếu tx có bảo vệ replay attack
    // Hoặc dùng HomesteadSigner cho tx cũ (ít dùng)
	signer := types.LatestSignerForChainID(tp.chainID)

	sender, err := types.Sender(signer, tx)

	if err != nil {
		return common.Address{}, fmt.Errorf("Invalid signature: %w", err)
	}

	return sender, nil
}