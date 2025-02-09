// Copyright 2016 The go-ethereum Authors
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

package les

import (
	"context"
	"errors"
	"fmt"
	"math/big"

	ethereum "github.com/celo-org/celo-blockchain"
	"github.com/celo-org/celo-blockchain/accounts"
	"github.com/celo-org/celo-blockchain/common"
	"github.com/celo-org/celo-blockchain/consensus"
	"github.com/celo-org/celo-blockchain/contracts/blockchain_parameters"
	"github.com/celo-org/celo-blockchain/core"
	"github.com/celo-org/celo-blockchain/core/bloombits"
	"github.com/celo-org/celo-blockchain/core/rawdb"
	"github.com/celo-org/celo-blockchain/core/state"
	"github.com/celo-org/celo-blockchain/core/types"
	"github.com/celo-org/celo-blockchain/core/vm"
	"github.com/celo-org/celo-blockchain/eth/ethconfig"
	gp "github.com/celo-org/celo-blockchain/eth/gasprice"
	"github.com/celo-org/celo-blockchain/ethdb"
	"github.com/celo-org/celo-blockchain/event"
	"github.com/celo-org/celo-blockchain/light"
	"github.com/celo-org/celo-blockchain/log"
	"github.com/celo-org/celo-blockchain/params"
	"github.com/celo-org/celo-blockchain/rpc"
)

type LesApiBackend struct {
	extRPCEnabled       bool
	allowUnprotectedTxs bool
	eth                 *LightEthereum
}

func (b *LesApiBackend) ChainConfig() *params.ChainConfig {
	return b.eth.chainConfig
}

func (b *LesApiBackend) CurrentBlock() *types.Block {
	return types.NewBlockWithHeader(b.eth.BlockChain().CurrentHeader())
}

func (b *LesApiBackend) SetHead(number uint64) {
	b.eth.handler.downloader.Cancel()
	b.eth.blockchain.SetHead(number)
}

func (b *LesApiBackend) HeaderByNumber(ctx context.Context, number rpc.BlockNumber) (*types.Header, error) {
	// Return the latest current as the pending one since there
	// is no pending notion in the light client. TODO(rjl493456442)
	// unify the behavior of `HeaderByNumber` and `PendingBlockAndReceipts`.
	if number == rpc.PendingBlockNumber {
		return b.eth.blockchain.CurrentHeader(), nil
	}
	if number == rpc.LatestBlockNumber {
		return b.eth.blockchain.CurrentHeader(), nil
	}
	return b.eth.blockchain.GetHeaderByNumberOdr(ctx, uint64(number))
}

func (b *LesApiBackend) HeaderByNumberOrHash(ctx context.Context, blockNrOrHash rpc.BlockNumberOrHash) (*types.Header, error) {
	if blockNr, ok := blockNrOrHash.Number(); ok {
		return b.HeaderByNumber(ctx, blockNr)
	}
	if hash, ok := blockNrOrHash.Hash(); ok {
		header, err := b.HeaderByHash(ctx, hash)
		if err != nil {
			return nil, err
		}
		if header == nil {
			return nil, errors.New("header for hash not found")
		}
		if blockNrOrHash.RequireCanonical && b.eth.blockchain.GetCanonicalHash(header.Number.Uint64()) != hash {
			return nil, errors.New("hash is not currently canonical")
		}
		return header, nil
	}
	return nil, errors.New("invalid arguments; neither block nor hash specified")
}

func (b *LesApiBackend) HeaderByHash(ctx context.Context, hash common.Hash) (*types.Header, error) {
	return b.eth.blockchain.GetHeaderByHash(hash), nil
}

func (b *LesApiBackend) BlockByNumber(ctx context.Context, number rpc.BlockNumber) (*types.Block, error) {
	header, err := b.HeaderByNumber(ctx, number)
	if header == nil || err != nil {
		return nil, err
	}
	return b.BlockByHash(ctx, header.Hash())
}

func (b *LesApiBackend) BlockByHash(ctx context.Context, hash common.Hash) (*types.Block, error) {
	return b.eth.blockchain.GetBlockByHash(ctx, hash)
}

func (b *LesApiBackend) BlockByNumberOrHash(ctx context.Context, blockNrOrHash rpc.BlockNumberOrHash) (*types.Block, error) {
	if blockNr, ok := blockNrOrHash.Number(); ok {
		return b.BlockByNumber(ctx, blockNr)
	}
	if hash, ok := blockNrOrHash.Hash(); ok {
		block, err := b.BlockByHash(ctx, hash)
		if err != nil {
			return nil, err
		}
		if block == nil {
			return nil, errors.New("header found, but block body is missing")
		}
		if blockNrOrHash.RequireCanonical && b.eth.blockchain.GetCanonicalHash(block.NumberU64()) != hash {
			return nil, errors.New("hash is not currently canonical")
		}
		return block, nil
	}
	return nil, errors.New("invalid arguments; neither block nor hash specified")
}

func (b *LesApiBackend) PendingBlockAndReceipts() (*types.Block, types.Receipts) {
	return nil, nil
}

func (b *LesApiBackend) StateAndHeaderByNumber(ctx context.Context, number rpc.BlockNumber) (*state.StateDB, *types.Header, error) {
	header, err := b.HeaderByNumber(ctx, number)
	if err != nil {
		return nil, nil, err
	}
	if header == nil {
		return nil, nil, errors.New("header not found")
	}
	return light.NewState(ctx, header, b.eth.odr), header, nil
}

func (b *LesApiBackend) StateAndHeaderByNumberOrHash(ctx context.Context, blockNrOrHash rpc.BlockNumberOrHash) (*state.StateDB, *types.Header, error) {
	if blockNr, ok := blockNrOrHash.Number(); ok {
		return b.StateAndHeaderByNumber(ctx, blockNr)
	}
	if hash, ok := blockNrOrHash.Hash(); ok {
		header := b.eth.blockchain.GetHeaderByHash(hash)
		if header == nil {
			return nil, nil, errors.New("header for hash not found")
		}
		if blockNrOrHash.RequireCanonical && b.eth.blockchain.GetCanonicalHash(header.Number.Uint64()) != hash {
			return nil, nil, errors.New("hash is not currently canonical")
		}
		return light.NewState(ctx, header, b.eth.odr), header, nil
	}
	return nil, nil, errors.New("invalid arguments; neither block nor hash specified")
}

func (b *LesApiBackend) GetReceipts(ctx context.Context, hash common.Hash) (types.Receipts, error) {
	if number := rawdb.ReadHeaderNumber(b.eth.chainDb, hash); number != nil {
		return light.GetBlockReceipts(ctx, b.eth.odr, hash, *number)
	}
	return nil, nil
}

func (b *LesApiBackend) GetLogs(ctx context.Context, hash common.Hash) ([][]*types.Log, error) {
	if number := rawdb.ReadHeaderNumber(b.eth.chainDb, hash); number != nil {
		return light.GetBlockLogs(ctx, b.eth.odr, hash, *number)
	}
	return nil, nil
}

func (b *LesApiBackend) GetTd(ctx context.Context, hash common.Hash) *big.Int {
	if number := rawdb.ReadHeaderNumber(b.eth.chainDb, hash); number != nil {
		return b.eth.blockchain.GetTdOdr(ctx, hash, *number)
	}
	return nil
}

func (b *LesApiBackend) GetEVM(ctx context.Context, msg core.Message, state *state.StateDB, header *types.Header, vmConfig *vm.Config) (*vm.EVM, func() error, error) {
	if vmConfig == nil {
		vmConfig = new(vm.Config)
	}
	txContext := core.NewEVMTxContext(msg)
	context := core.NewEVMBlockContext(header, b.eth.blockchain, nil)
	return vm.NewEVM(context, txContext, state, b.eth.chainConfig, *vmConfig), state.Error, nil
}

func (b *LesApiBackend) SendTx(ctx context.Context, signedTx *types.Transaction) error {
	return b.eth.txPool.Add(ctx, signedTx)
}

func (b *LesApiBackend) RemoveTx(txHash common.Hash) {
	b.eth.txPool.RemoveTx(txHash)
}

func (b *LesApiBackend) GetPoolTransactions() (types.Transactions, error) {
	return b.eth.txPool.GetTransactions()
}

func (b *LesApiBackend) GetPoolTransaction(txHash common.Hash) *types.Transaction {
	return b.eth.txPool.GetTransaction(txHash)
}

func (b *LesApiBackend) GetTransaction(ctx context.Context, txHash common.Hash) (*types.Transaction, common.Hash, uint64, uint64, error) {
	return light.GetTransaction(ctx, b.eth.odr, txHash)
}

func (b *LesApiBackend) GetPoolNonce(ctx context.Context, addr common.Address) (uint64, error) {
	return b.eth.txPool.GetNonce(ctx, addr)
}

func (b *LesApiBackend) Stats() (pending int, queued int) {
	return b.eth.txPool.Stats(), 0
}

func (b *LesApiBackend) TxPoolContent() (map[common.Address]types.Transactions, map[common.Address]types.Transactions) {
	return b.eth.txPool.Content()
}

func (b *LesApiBackend) TxPoolContentFrom(addr common.Address) (types.Transactions, types.Transactions) {
	return b.eth.txPool.ContentFrom(addr)
}

func (b *LesApiBackend) SubscribeNewTxsEvent(ch chan<- core.NewTxsEvent) event.Subscription {
	return b.eth.txPool.SubscribeNewTxsEvent(ch)
}

func (b *LesApiBackend) SubscribeChainEvent(ch chan<- core.ChainEvent) event.Subscription {
	return b.eth.blockchain.SubscribeChainEvent(ch)
}

func (b *LesApiBackend) SubscribeChainHeadEvent(ch chan<- core.ChainHeadEvent) event.Subscription {
	return b.eth.blockchain.SubscribeChainHeadEvent(ch)
}

func (b *LesApiBackend) SubscribeChainSideEvent(ch chan<- core.ChainSideEvent) event.Subscription {
	return b.eth.blockchain.SubscribeChainSideEvent(ch)
}

func (b *LesApiBackend) SubscribeLogsEvent(ch chan<- []*types.Log) event.Subscription {
	return b.eth.blockchain.SubscribeLogsEvent(ch)
}

func (b *LesApiBackend) SubscribePendingLogsEvent(ch chan<- []*types.Log) event.Subscription {
	return event.NewSubscription(func(quit <-chan struct{}) error {
		<-quit
		return nil
	})
}

func (b *LesApiBackend) SubscribeRemovedLogsEvent(ch chan<- core.RemovedLogsEvent) event.Subscription {
	return b.eth.blockchain.SubscribeRemovedLogsEvent(ch)
}

func (b *LesApiBackend) SyncProgress() ethereum.SyncProgress {
	return b.eth.Downloader().Progress()
}

func (b *LesApiBackend) ProtocolVersion() int {
	return b.eth.LesVersion() + 10000
}

func (b *LesApiBackend) SuggestPrice(ctx context.Context, currencyAddress *common.Address) (*big.Int, error) {
	vmRunner, err := b.eth.BlockChain().NewEVMRunnerForCurrentBlock()
	if err != nil {
		return nil, err
	}
	return gp.GetGasPriceSuggestion(vmRunner, currencyAddress, b.CurrentHeader().BaseFee, b.eth.config.RPCGasPriceMultiplier)
}

func (b *LesApiBackend) GetIntrinsicGasForAlternativeFeeCurrency(ctx context.Context) uint64 {
	vmRunner, err := b.eth.BlockChain().NewEVMRunnerForCurrentBlock()
	if err != nil {
		log.Warn("Cannot read intrinsic gas for alternative fee currency", "err", err)
		return blockchain_parameters.DefaultIntrinsicGasForAlternativeFeeCurrency
	}
	return blockchain_parameters.GetIntrinsicGasForAlternativeFeeCurrencyOrDefault(vmRunner)
}

func (b *LesApiBackend) GetBlockGasLimit(ctx context.Context, blockNrOrHash rpc.BlockNumberOrHash) uint64 {
	header, err := b.HeaderByNumberOrHash(ctx, blockNrOrHash)
	if err != nil {
		log.Warn("Cannot retrieve the header for blockGasLimit", "err", err)
		return params.DefaultGasLimit
	}
	if header.GasLimit > 0 {
		return header.GasLimit
	}
	// The gasLimit of a specific block, is the one at the beginning of the block,
	// not the end of it (the state_root of the header is the a state resulted of applying the block). So, the state to
	// be used, MUST be the state result of the parent block
	state, parent, err := b.StateAndHeaderByNumberOrHash(ctx, rpc.BlockNumberOrHash{BlockHash: &header.ParentHash})
	if err != nil {
		log.Warn("Cannot create evmCaller to get blockGasLimit", "err", err)
		return params.DefaultGasLimit
	}
	vmRunner := b.eth.BlockChain().NewEVMRunner(parent, state)
	return blockchain_parameters.GetBlockGasLimitOrDefault(vmRunner)
}

func (b *LesApiBackend) GetRealBlockGasLimit(ctx context.Context, blockNrOrHash rpc.BlockNumberOrHash) (uint64, error) {
	header, err := b.HeaderByNumberOrHash(ctx, blockNrOrHash)
	if err != nil {
		return 0, fmt.Errorf("LesApiBackend failed to retrieve header for gas limit %v: %w", blockNrOrHash, err)
	}
	if header.GasLimit > 0 {
		return header.GasLimit, nil
	}
	// The gasLimit of a specific block, is the one at the beginning of the block,
	// not the end of it (the state_root of the header is the a state resulted of applying the block). So, the state to
	// be used, MUST be the state result of the parent block
	state, parent, err := b.StateAndHeaderByNumberOrHash(ctx, rpc.BlockNumberOrHash{BlockHash: &header.ParentHash})
	if err != nil {
		return 0, fmt.Errorf("LesApiBackend failed to retrieve state for block gas limit for block %v: %w", blockNrOrHash, err)
	}
	vmRunner := b.eth.BlockChain().NewEVMRunner(parent, state)
	limit, err := blockchain_parameters.GetBlockGasLimit(vmRunner)
	if err != nil {
		return 0, fmt.Errorf("LesApiBackend failed to retrieve block gas limit from blockchain parameters constract for block %v: %w", blockNrOrHash, err)
	}
	return limit, nil
}

func (b *LesApiBackend) NewEVMRunner(header *types.Header, state vm.StateDB) vm.EVMRunner {
	return b.eth.BlockChain().NewEVMRunner(header, state)
}

func (b *LesApiBackend) SuggestGasTipCap(ctx context.Context, currencyAddress *common.Address) (*big.Int, error) {
	vmRunner, err := b.eth.BlockChain().NewEVMRunnerForCurrentBlock()
	if err != nil {
		return nil, err
	}
	return gp.GetGasTipCapSuggestion(vmRunner, currencyAddress)
}

func (b *LesApiBackend) CurrentGasPriceMinimum(ctx context.Context, currencyAddress *common.Address) (*big.Int, error) {
	header := b.CurrentHeader()
	if header.BaseFee != nil && currencyAddress == nil {
		return header.BaseFee, nil
	}
	vmRunner, err := b.eth.BlockChain().NewEVMRunnerForCurrentBlock()
	if err != nil {
		return nil, err
	}
	return gp.GetBaseFeeForCurrency(vmRunner, currencyAddress, header.BaseFee)
}

func (b *LesApiBackend) GasPriceMinimumForHeader(ctx context.Context, currencyAddress *common.Address, header *types.Header) (*big.Int, error) {
	if header.BaseFee != nil && currencyAddress == nil {
		return header.BaseFee, nil
	}
	// The gasPriceMinimum (celo or alternative currency) of a specific block, is the one at the beginning of the block,
	// not the end of it (the state_root of the header is the a state resulted of applying the block). So, the state to
	// be used, MUST be the state result of the parent block
	state, parent, err := b.StateAndHeaderByNumberOrHash(ctx, rpc.BlockNumberOrHash{BlockHash: &header.ParentHash})
	if err != nil {
		return nil, err
	}
	vmRunner := b.eth.BlockChain().NewEVMRunner(parent, state)
	return gp.GetBaseFeeForCurrency(vmRunner, currencyAddress, header.BaseFee)
}

func (b *LesApiBackend) RealGasPriceMinimumForHeader(ctx context.Context, currencyAddress *common.Address, header *types.Header) (*big.Int, error) {
	if header.BaseFee != nil && currencyAddress == nil {
		return header.BaseFee, nil
	}
	// The gasPriceMinimum (celo or alternative currency) of a specific block, is the one at the beginning of the block,
	// not the end of it (the state_root of the header is the a state resulted of applying the block). So, the state to
	// be used, MUST be the state result of the parent block
	state, parent, err := b.StateAndHeaderByNumberOrHash(ctx, rpc.BlockNumberOrHash{BlockHash: &header.ParentHash})
	if err != nil {
		return nil, err
	}
	vmRunner := b.eth.BlockChain().NewEVMRunner(parent, state)
	return gp.GetRealBaseFeeForCurrency(vmRunner, currencyAddress, header.BaseFee)
}

func (b *LesApiBackend) ChainDb() ethdb.Database {
	return b.eth.chainDb
}

func (b *LesApiBackend) AccountManager() *accounts.Manager {
	return b.eth.accountManager
}

func (b *LesApiBackend) ExtRPCEnabled() bool {
	return b.extRPCEnabled
}

func (b *LesApiBackend) UnprotectedAllowed() bool {
	return b.allowUnprotectedTxs
}

func (b *LesApiBackend) RPCGasInflationRate() float64 {
	return b.eth.config.RPCGasInflationRate
}

func (b *LesApiBackend) RPCGasCap() uint64 {
	return b.eth.config.RPCGasCap
}

func (b *LesApiBackend) RPCTxFeeCap() float64 {
	return b.eth.config.RPCTxFeeCap
}

func (b *LesApiBackend) RPCEthCompatibility() bool {
	return b.eth.config.RPCEthCompatibility
}

func (b *LesApiBackend) BloomStatus() (uint64, uint64) {
	if b.eth.bloomIndexer == nil {
		return 0, 0
	}
	sections, _, _ := b.eth.bloomIndexer.Sections()
	return params.BloomBitsBlocksClient, sections
}

func (b *LesApiBackend) ServiceFilter(ctx context.Context, session *bloombits.MatcherSession) {
	for i := 0; i < bloomFilterThreads; i++ {
		go session.Multiplex(bloomRetrievalBatch, bloomRetrievalWait, b.eth.bloomRequests)
	}
}

func (b *LesApiBackend) GatewayFeeRecipient() common.Address {
	if b.ChainConfig().IsGingerbread(b.CurrentHeader().Number) {
		return common.Address{}
	}
	return b.eth.GetRandomPeerEtherbase()
}

func (b *LesApiBackend) GatewayFee() *big.Int {
	// TODO(nategraf): Create a method to fetch the gateway fee values of peers along with the coinbase.
	return ethconfig.Defaults.GatewayFee
}

func (b *LesApiBackend) Engine() consensus.Engine {
	return b.eth.engine
}

func (b *LesApiBackend) CurrentHeader() *types.Header {
	return b.eth.blockchain.CurrentHeader()
}

func (b *LesApiBackend) StateAtBlock(ctx context.Context, block *types.Block, reexec uint64, base *state.StateDB, checkLive bool, preferDisk bool, commitRandomness bool) (*state.StateDB, error) {
	return b.eth.stateAtBlock(ctx, block, reexec)
}

func (b *LesApiBackend) StateAtTransaction(ctx context.Context, block *types.Block, txIndex int, reexec uint64) (core.Message, vm.BlockContext, vm.EVMRunner, *state.StateDB, error) {
	return b.eth.stateAtTransaction(ctx, block, txIndex, reexec)
}
