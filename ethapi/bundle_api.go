package ethapi

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"math"
	"math/big"
	"time"

	"github.com/Fantom-foundation/go-opera/evmcore"
	"github.com/Fantom-foundation/go-opera/inter"
	"github.com/Fantom-foundation/go-opera/inter/state"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/consensus/misc/eip1559"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/holiman/uint256"
	"golang.org/x/crypto/sha3"
)

// CallBundleArgs 表示调用的参数
type CallBundleArgs struct {
	Txs                    []hexutil.Bytes       `json:"txs"`
	BlockNumber            rpc.BlockNumber       `json:"blockNumber"`
	StateBlockNumberOrHash rpc.BlockNumberOrHash `json:"stateBlockNumber"`
	Coinbase               *string               `json:"coinbase"`
	Timestamp              *inter.Timestamp      `json:"timestamp"`
	Timeout                *int64                `json:"timeout"`
	GasLimit               *uint64               `json:"gasLimit"`
	Difficulty             *big.Int              `json:"difficulty"`
	BaseFee                *big.Int              `json:"baseFee"`
}

// BundleAPI 提供 bundle 相关的 API
type BundleAPI struct {
	b Backend
}

// NewBundleAPI 创建一个新的 BundleAPI 实例
func NewBundleAPI(b Backend) *BundleAPI {
	return &BundleAPI{
		b: b,
	}
}

// CallBundle 将在给定区块号顶部模拟一组交易
func (s *BundleAPI) CallBundle(ctx context.Context, args CallBundleArgs) (map[string]interface{}, error) {
	if len(args.Txs) == 0 {
		return nil, errors.New("bundle missing txs")
	}
	if args.BlockNumber == 0 {
		return nil, errors.New("bundle missing blockNumber")
	}

	var txs types.Transactions
	for _, encodedTx := range args.Txs {
		tx := new(types.Transaction)
		if err := tx.UnmarshalBinary(encodedTx); err != nil {
			return nil, err
		}
		txs = append(txs, tx)
	}

	defer func(start time.Time) {
		log.Debug("Executing EVM call finished", "runtime", time.Since(start))
	}(time.Now())

	timeoutMilliSeconds := int64(5000)
	if args.Timeout != nil {
		timeoutMilliSeconds = *args.Timeout
	}
	timeout := time.Millisecond * time.Duration(timeoutMilliSeconds)

	var cancel context.CancelFunc
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, timeout)
	} else {
		ctx, cancel = context.WithCancel(ctx)
	}
	defer cancel()

	state, parent, err := s.b.StateAndHeaderByNumberOrHash(ctx, args.StateBlockNumberOrHash)
	if state == nil || err != nil {
		return nil, err
	}

	blockNumber := big.NewInt(int64(args.BlockNumber))

	timestamp := parent.Time + 1
	if args.Timestamp != nil {
		timestamp = *args.Timestamp
	}

	coinbase := parent.Coinbase
	if args.Coinbase != nil {
		coinbase = common.HexToAddress(*args.Coinbase)
	}

	gasLimit := parent.GasLimit
	if args.GasLimit != nil {
		gasLimit = *args.GasLimit
	}

	var baseFee *big.Int
	if args.BaseFee != nil {
		baseFee = args.BaseFee
	} else if s.b.ChainConfig().IsLondon(big.NewInt(args.BlockNumber.Int64())) {
		baseFee = eip1559.CalcBaseFee(s.b.ChainConfig(), parent.EthHeader())
	}

	header := &evmcore.EvmHeader{
		// 下面一行有错误
		ParentHash: parent.Hash,
		Number:     blockNumber,
		GasLimit:   gasLimit,
		Time:       timestamp,
		Coinbase:   coinbase,
		BaseFee:    baseFee,
	}

	gp := new(core.GasPool).AddGas(math.MaxUint64)

	results := []map[string]interface{}{}
	coinbaseBalanceBefore := state.GetBalance(coinbase)

	bundleHash := sha3.NewLegacyKeccak256()
	signer := types.MakeSigner(s.b.ChainConfig(), blockNumber, header.EthHeader().Time)
	var totalGasUsed uint64
	gasFees := new(big.Int)

	for i, tx := range txs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		coinbaseBalanceBeforeTx := state.GetBalance(coinbase)
		state.SetTxContext(tx.Hash(), i)

		evm, msg, err := GetEVM(ctx, s.b, state, header, tx)
		if err != nil {
			return nil, err
		}
		receipt, result, err := evmcore.ApplyTransactionWithResult(msg, gp, state, header, tx, &header.GasUsed, evm)
		if err != nil {
			return nil, fmt.Errorf("err: %w; txhash %s", err, tx.Hash())
		}

		// 处理交易结果
		jsonResult := s.processTransactionResult(tx, receipt, result, signer, header.EthHeader(), coinbaseBalanceBeforeTx, state)
		totalGasUsed += receipt.GasUsed

		gasPrice, err := tx.EffectiveGasTip(header.BaseFee)
		if err != nil {
			return nil, fmt.Errorf("err: %w; txhash %s", err, tx.Hash())
		}

		gasFeesTx := new(big.Int).Mul(big.NewInt(int64(receipt.GasUsed)), gasPrice)
		gasFees.Add(gasFees, gasFeesTx)
		bundleHash.Write(tx.Hash().Bytes())

		results = append(results, jsonResult)
	}

	return s.prepareFinalResult(results, state, coinbaseBalanceBefore, coinbase, gasFees, totalGasUsed, parent.EthHeader(), bundleHash), nil
}

// processTransactionResult 处理单个交易的结果
func (s *BundleAPI) processTransactionResult(tx *types.Transaction, receipt *types.Receipt, result *core.ExecutionResult, signer types.Signer, header *types.Header, coinbaseBalanceBeforeTx *uint256.Int, state vm.StateDB) map[string]interface{} {
	txHash := tx.Hash().String()
	from, _ := types.Sender(signer, tx)
	to := "0x"
	if tx.To() != nil {
		to = tx.To().String()
	}

	jsonResult := map[string]interface{}{
		"txHash":      txHash,
		"gasUsed":     receipt.GasUsed,
		"fromAddress": from.String(),
		"toAddress":   to,
	}

	if result.Err != nil {
		jsonResult["error"] = result.Err.Error()
		if revert := result.Revert(); len(revert) > 0 {
			jsonResult["revert"] = string(revert)
		}
	} else {
		dst := make([]byte, hex.EncodedLen(len(result.Return())))
		hex.Encode(dst, result.Return())
		jsonResult["value"] = "0x" + string(dst)
	}

	coinbaseDiffTx := new(big.Int).Sub(state.GetBalance(header.Coinbase).ToBig(), coinbaseBalanceBeforeTx.ToBig())
	gasPrice, _ := tx.EffectiveGasTip(header.BaseFee)
	gasFeesTx := new(big.Int).Mul(big.NewInt(int64(receipt.GasUsed)), gasPrice)

	jsonResult["coinbaseDiff"] = coinbaseDiffTx.String()
	jsonResult["gasFees"] = gasFeesTx.String()
	jsonResult["ethSentToCoinbase"] = new(big.Int).Sub(coinbaseDiffTx, gasFeesTx).String()
	jsonResult["gasPrice"] = new(big.Int).Div(coinbaseDiffTx, big.NewInt(int64(receipt.GasUsed))).String()

	return jsonResult
}

// prepareFinalResult 准备最终的返回结果
func (s *BundleAPI) prepareFinalResult(results []map[string]interface{}, state vm.StateDB, coinbaseBalanceBefore *uint256.Int, coinbase common.Address, gasFees *big.Int, totalGasUsed uint64, parent *types.Header, bundleHash hash.Hash) map[string]interface{} {
	ret := map[string]interface{}{
		"results":          results,
		"stateBlockNumber": parent.Number.Int64(),
		"totalGasUsed":     totalGasUsed,
	}

	coinbaseDiff := new(big.Int).Sub(state.GetBalance(coinbase).ToBig(), coinbaseBalanceBefore.ToBig())
	ret["coinbaseDiff"] = coinbaseDiff.String()
	ret["gasFees"] = gasFees.String()
	ret["ethSentToCoinbase"] = new(big.Int).Sub(coinbaseDiff, gasFees).String()
	ret["bundleGasPrice"] = new(big.Int).Div(coinbaseDiff, big.NewInt(int64(totalGasUsed))).String()
	ret["bundleHash"] = "0x" + common.Bytes2Hex(bundleHash.Sum(nil))

	return ret
}

type EBackend interface {
	ChainConfig() *params.ChainConfig
	GetEVM(ctx context.Context, msg *core.Message, state vm.StateDB, header *evmcore.EvmHeader, vmConfig *vm.Config) (*vm.EVM, func() error, error)
}

// apply transaction returning result, for callBundle
func GetEVM(ctx context.Context, b EBackend, statedb state.StateDB, header *evmcore.EvmHeader, tx *types.Transaction) (*vm.EVM, *core.Message, error) {
	config := b.ChainConfig()
	msg, err := core.TransactionToMessage(tx, types.MakeSigner(config, header.Number, uint64(header.Time)), header.BaseFee)
	if err != nil {
		return nil, nil, err
	}
	vmconfig := vm.Config{}
	evm, _, err := b.GetEVM(ctx, msg, statedb, header, &vmconfig)
	return evm, msg, err
}
