// Copyright 2014 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

// Package legacypool implements the normal EVM execution transaction pool.
package filters

import (
	"context"
	"math/big"

	"github.com/Fantom-foundation/go-opera/ethapi"
	"github.com/Fantom-foundation/go-opera/evmcore"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/misc/eip1559"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"

	// "github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rpc"
)

// simulateTx performs a simulated execution of the transaction
func (api *PublicFilterAPI) simulateTx(ctx context.Context, txHash common.Hash) ([]*types.Log, error) {
	parent := api.backend.CurrentBlock().EvmHeader
	blockNumber := new(big.Int).Add(parent.Number, big.NewInt(1))

	timestamp := parent.Time + 1
	coinbase := parent.Coinbase
	gasLimit := parent.GasLimit

	chainConfig := api.backend.ChainConfig()
	var baseFee *big.Int
	if chainConfig.IsLondon(blockNumber) {
		baseFee = eip1559.CalcBaseFee(chainConfig, parent.EthHeader())
	}

	tx, _, _, err := api.backend.GetTransaction(ctx, txHash)
	if err != nil {
		return nil, err
	}

	header := &evmcore.EvmHeader{
		ParentHash: parent.Hash,
		Number:     blockNumber,
		GasLimit:   gasLimit,
		Time:       timestamp,
		Coinbase:   coinbase,
		BaseFee:    baseFee,
	}

	gasPool := new(core.GasPool).AddGas(tx.Gas())
	state, _, err := api.backend.StateAndHeaderByNumberOrHash(ctx, rpc.BlockNumberOrHashWithNumber(rpc.BlockNumber(parent.Number.Int64())))
	if err != nil {
		return nil, err
	}
	state.SetTxContext(tx.Hash(), 0)
	evm, msg, err := ethapi.GetEVM(ctx, api.backend, state, header, tx)
	if err != nil {
		return nil, err
	}
	receipt, _, err := evmcore.ApplyTransactionWithResult(msg, gasPool, state, header, tx, &header.GasUsed, evm)
	if err != nil {
		return nil, err
	}

	// Process and print logs (events)
	// log.Info("New Captured event: %+v\n", receipt.Logs)
	return receipt.Logs, nil
}
