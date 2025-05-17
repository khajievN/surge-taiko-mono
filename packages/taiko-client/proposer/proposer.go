package proposer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math/big"
	"math/rand"
	"strings"
	"sync"
	"time"

	"github.com/ethereum-optimism/optimism/op-service/eth"
	"github.com/ethereum-optimism/optimism/op-service/txmgr"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/crypto/kzg4844"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/urfave/cli/v2"
	"golang.org/x/sync/errgroup"

	"github.com/taikoxyz/taiko-mono/packages/taiko-client/bindings"
	"github.com/taikoxyz/taiko-mono/packages/taiko-client/bindings/bridge"
	"github.com/taikoxyz/taiko-mono/packages/taiko-client/bindings/encoding"
	"github.com/taikoxyz/taiko-mono/packages/taiko-client/internal/metrics"
	"github.com/taikoxyz/taiko-mono/packages/taiko-client/internal/utils"
	"github.com/taikoxyz/taiko-mono/packages/taiko-client/pkg/config"
	"github.com/taikoxyz/taiko-mono/packages/taiko-client/pkg/rpc"
	builder "github.com/taikoxyz/taiko-mono/packages/taiko-client/proposer/transaction_builder"
)

// Proposer keep proposing new transactions from L2 execution engine's tx pool at a fixed interval.
type Proposer struct {
	// configurations
	*Config

	// RPC clients
	rpc *rpc.Client

	// Private keys and account addresses
	proposerAddress common.Address

	proposingTimer *time.Timer

	// Transaction builders
	txCallDataBuilder builder.ProposeBlockTransactionBuilder
	txBlobBuilder     builder.ProposeBlockTransactionBuilder
	defaultTxBuilder  builder.ProposeBlockTransactionBuilder

	// Protocol configurations
	protocolConfigs *bindings.TaikoDataConfig

	chainConfig *config.ChainConfig

	lastProposedAt time.Time
	totalEpochs    uint64

	txmgrSelector *utils.TxMgrSelector

	ctx context.Context
	wg  sync.WaitGroup

	checkProfitability bool

	allowEmptyBlocks bool
	initDone         bool
	forceProposeOnce bool

	// Bridge message monitoring
	pendingBridgeMessages map[common.Hash]*types.Transaction
	bridgeMsgMu           sync.RWMutex
}

// InitFromCli initializes the given proposer instance based on the command line flags.
func (p *Proposer) InitFromCli(ctx context.Context, c *cli.Context) error {
	cfg, err := NewConfigFromCliContext(c)
	if err != nil {
		return err
	}

	return p.InitFromConfig(ctx, cfg, nil, nil)
}

// InitFromConfig initializes the proposer instance based on the given configurations.
func (p *Proposer) InitFromConfig(
	ctx context.Context, cfg *Config,
	txMgr *txmgr.SimpleTxManager,
	privateTxMgr *txmgr.SimpleTxManager,
) (err error) {
	p.proposerAddress = crypto.PubkeyToAddress(cfg.L1ProposerPrivKey.PublicKey)
	p.ctx = ctx
	p.Config = cfg
	p.lastProposedAt = time.Now()
	p.checkProfitability = cfg.CheckProfitability
	p.allowEmptyBlocks = cfg.AllowEmptyBlocks
	p.initDone = false

	// RPC clients
	if p.rpc, err = rpc.NewClient(p.ctx, cfg.ClientConfig); err != nil {
		return fmt.Errorf("initialize rpc clients error: %w", err)
	}

	// Check L1 RPC connection
	blockNum, err := p.rpc.L1.BlockNumber(context.Background())
	if err != nil {
		return fmt.Errorf("failed to connect to L1 RPC: %w", err)
	}
	log.Info("Successfully connected to L1 RPC", "currentBlock", blockNum)

	// Protocol configs
	p.protocolConfigs = encoding.GetProtocolConfig(p.rpc.L2.ChainID.Uint64())

	log.Info("Protocol configs", "configs", p.protocolConfigs)

	if txMgr == nil {
		if txMgr, err = txmgr.NewSimpleTxManager(
			"proposer",
			log.Root(),
			&metrics.TxMgrMetrics,
			*cfg.TxmgrConfigs,
		); err != nil {
			return err
		}
	}

	if privateTxMgr == nil && cfg.PrivateTxmgrConfigs != nil && len(cfg.PrivateTxmgrConfigs.L1RPCURL) > 0 {
		if privateTxMgr, err = txmgr.NewSimpleTxManager(
			"privateMempoolProposer",
			log.Root(),
			&metrics.TxMgrMetrics,
			*cfg.PrivateTxmgrConfigs,
		); err != nil {
			return err
		}
	}

	p.txmgrSelector = utils.NewTxMgrSelector(txMgr, privateTxMgr, nil)

	chainConfig := config.NewChainConfig(p.protocolConfigs)
	p.chainConfig = chainConfig

	p.txCallDataBuilder = builder.NewCalldataTransactionBuilder(
		p.rpc,
		p.L1ProposerPrivKey,
		cfg.L2SuggestedFeeRecipient,
		cfg.TaikoL1Address,
		cfg.ProverSetAddress,
		cfg.ProposeBlockTxGasLimit,
		cfg.ExtraData,
		chainConfig,
	)
	if cfg.BlobAllowed {
		p.txBlobBuilder = builder.NewBlobTransactionBuilder(
			p.rpc,
			p.L1ProposerPrivKey,
			cfg.TaikoL1Address,
			cfg.ProverSetAddress,
			cfg.L2SuggestedFeeRecipient,
			cfg.ProposeBlockTxGasLimit,
			cfg.ExtraData,
			chainConfig,
		)
		p.defaultTxBuilder = p.txBlobBuilder
	} else {
		p.txBlobBuilder = nil
		p.defaultTxBuilder = p.txCallDataBuilder
	}
	if (cfg.ClientConfig.InboxAddress != common.Address{}) {
		if err := p.SubscribeToSignalSentEvent(); err != nil {
			return err
		}
	}

	return nil
}

// subscribe to SignalSent event on eth l1 RPC
func (p *Proposer) SubscribeToSignalSentEvent() error {
	logChan := make(chan types.Log)
	sub, err := p.rpc.L1.SubscribeFilterLogs(p.ctx, ethereum.FilterQuery{
		Addresses: []common.Address{p.Config.InboxAddress},
		Topics:    [][]common.Hash{{common.HexToHash("0x0ad2d108660a211f47bf7fb43a0443cae181624995d3d42b88ee6879d200e973")}},
	}, logChan)
	if err != nil {
		return fmt.Errorf("subscribe error: %w", err)
	}

	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		defer sub.Unsubscribe()

		for {
			select {
			case <-p.ctx.Done():
				return
			case err := <-sub.Err():
				log.Error("subscription error", "err", err)
				return
			case vLog := <-logChan:
				log.Info("SignalSent event received", "log", vLog)
				p.forceProposeOnce = true
			}
		}
	}()
	return nil
}

// Start starts the proposer's main loop.
func (p *Proposer) Start() error {
	p.wg.Add(1)
	go p.eventLoop()

	// Start monitoring L1 Bridge messages
	p.wg.Add(1)
	go p.monitorBridgeMessages()

	return nil
}

// eventLoop starts the main loop of Taiko proposer.
func (p *Proposer) eventLoop() {
	defer func() {
		p.proposingTimer.Stop()
		p.wg.Done()
	}()

	for {
		p.updateProposingTicker()

		select {
		case <-p.ctx.Done():
			return
		// proposing interval timer has been reached
		case <-p.proposingTimer.C:
			metrics.ProposerProposeEpochCounter.Add(1)
			p.totalEpochs++

			// Attempt a proposing operation
			if err := p.ProposeOp(p.ctx); err != nil {
				log.Error("Proposing operation error", "error", err)
				continue
			}
		}
	}
}

// monitorBridgeMessages monitors L1 transaction pool for Bridge sendMessage calls
func (p *Proposer) monitorBridgeMessages() {
	defer p.wg.Done()

	// Create a channel for new pending transactions
	pendingTxs := make(chan common.Hash)

	// Subscribe to new pending transactions using RPC client
	sub, err := p.rpc.L1.Client.Subscribe(p.ctx, "eth", pendingTxs, "newPendingTransactions")
	if err != nil {
		log.Error("Failed to subscribe to pending transactions", "error", err)
		return
	}
	defer sub.Unsubscribe()

	// Initialize pending messages map
	p.pendingBridgeMessages = make(map[common.Hash]*types.Transaction)

	// Get the Bridge contract ABI
	bridgeABI, err := bridge.BridgeMetaData.GetAbi()
	if err != nil {
		log.Error("Failed to get Bridge ABI", "error", err)
		return
	}

	// Get the sendMessage method
	sendMessageMethod := bridgeABI.Methods["sendMessage"]
	if sendMessageMethod.ID == nil {
		log.Error("Failed to get sendMessage method ID")
		return
	}

	log.Debug("Starting Bridge message monitoring",
		"bridgeAddress", p.Config.ClientConfig.BridgeAddress.Hex(),
		"sendMessageSelector", common.BytesToHash(sendMessageMethod.ID).Hex())

	for {
		select {
		case <-p.ctx.Done():
			return
		case err := <-sub.Err():
			log.Error("Subscription error", "error", err)
			return
		case txHash := <-pendingTxs:
			log.Debug("New pending transaction detected", "hash", txHash.Hex())

			// Skip if we already have this transaction
			p.bridgeMsgMu.RLock()
			if _, exists := p.pendingBridgeMessages[txHash]; exists {
				p.bridgeMsgMu.RUnlock()
				continue
			}
			p.bridgeMsgMu.RUnlock()

			// Get transaction details
			tx, isPending, err := p.rpc.L1.TransactionByHash(p.ctx, txHash)
			if err != nil {
				log.Error("Failed to get transaction details", "hash", txHash, "error", err)
				continue
			}

			// Skip if transaction is no longer pending (as in, has been mined already) because with the fast
			// L1-to-L2 bridging, proposer will propose the sendMessage transactions as part of its block
			if !isPending {
				log.Debug("Transaction is no longer pending", "hash", txHash.Hex())
				continue
			}

			// Check if transaction is to Bridge contract
			if tx.To() == nil || *tx.To() != p.Config.ClientConfig.BridgeAddress {
				log.Debug("Transaction is not to Bridge contract", "hash", txHash.Hex())
				continue
			}

			// Check if transaction data starts with sendMessage selector
			if len(tx.Data()) < 4 || !bytes.Equal(tx.Data()[:4], sendMessageMethod.ID) {
				log.Debug("Transaction data does not start with sendMessage selector", "hash", txHash.Hex())
				log.Debug("Transaction data comparison",
					"hash", txHash.Hex(),
					"actual", common.BytesToHash(tx.Data()[:4]).Hex(),
					"expected", common.BytesToHash(sendMessageMethod.ID).Hex())
				continue
			}

			// Add to pending messages
			p.bridgeMsgMu.Lock()
			p.pendingBridgeMessages[txHash] = tx
			log.Info("New Bridge sendMessage transaction detected in mempool", "hash", txHash)
			p.bridgeMsgMu.Unlock()
		}
	}
}

// Close closes the proposer instance.
func (p *Proposer) Close(_ context.Context) {
	p.wg.Wait()
}

// fetchPoolContent fetches the transaction pool content from L2 execution engine.
func (p *Proposer) fetchPoolContent(filterPoolContent bool) ([]types.Transactions, error) {
	log.Debug("fetchPoolContent")
	var (
		minTip  = p.MinTip
		startAt = time.Now()
	)
	// If `--epoch.allowZeroInterval` flag is set, allow proposing zero tip transactions once when
	// the total epochs number is divisible by the flag value.
	if p.AllowZeroInterval > 0 && p.totalEpochs%p.AllowZeroInterval == 0 {
		minTip = 0
	}

	// Fetch the pool content.
	preBuiltTxList, err := p.rpc.GetPoolContent(
		p.ctx,
		p.proposerAddress,
		p.protocolConfigs.BlockMaxGasLimit,
		rpc.BlockMaxTxListBytes,
		p.LocalAddresses,
		p.MaxProposedTxListsPerEpoch,
		minTip,
		p.chainConfig,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch transaction pool content from rpc: %w", err)
	}

	metrics.ProposerPoolContentFetchTime.Set(time.Since(startAt).Seconds())

	txLists := []types.Transactions{}

	if !p.initDone || p.forceProposeOnce {
		log.Debug("Initializing proposer or force proposing once")
		lastL2Header, err := p.rpc.L2.HeaderByNumber(p.ctx, nil)
		if err != nil {
			return nil, fmt.Errorf("failed to get last L2 header: %w", err)
		}
		if lastL2Header.Number.Cmp(common.Big0) == 0 || p.forceProposeOnce {
			log.Info("Proposing empty block if there are no other txs")
			txLists = append(txLists, types.Transactions{})
			return txLists, nil
		}
		p.initDone = true
	}

	for i, txs := range preBuiltTxList {
		// Filter the pool content if the filterPoolContent flag is set.
		if txs.EstimatedGasUsed < p.MinGasUsed && txs.BytesLength < p.MinTxListBytes && filterPoolContent {
			log.Info(
				"Pool content skipped",
				"index", i,
				"estimatedGasUsed", txs.EstimatedGasUsed,
				"minGasUsed", p.MinGasUsed,
				"bytesLength", txs.BytesLength,
				"minBytesLength", p.MinTxListBytes,
			)
			break
		}
		txLists = append(txLists, txs.TxList)
	}
	// If the pool is empty and we're not filtering or checking profitability and proposing empty
	// blocks is allowed, propose an empty block
	if !filterPoolContent && !p.checkProfitability && p.allowEmptyBlocks && len(txLists) == 0 {
		log.Info(
			"Pool content is empty, proposing an empty block",
			"lastProposedAt", p.lastProposedAt,
			"minProposingInternal", p.MinProposingInternal,
		)
		txLists = append(txLists, types.Transactions{})
	}

	// If LocalAddressesOnly is set, filter the transactions by the local addresses.
	if p.LocalAddressesOnly {
		var (
			localTxsLists []types.Transactions
			signer        = types.LatestSignerForChainID(p.rpc.L2.ChainID)
		)
		for _, txs := range txLists {
			var filtered types.Transactions
			for _, tx := range txs {
				sender, err := types.Sender(signer, tx)
				if err != nil {
					return nil, fmt.Errorf("failed to get sender: %w", err)
				}

				for _, localAddress := range p.LocalAddresses {
					if sender == localAddress {
						filtered = append(filtered, tx)
					}
				}
			}

			if filtered.Len() != 0 {
				localTxsLists = append(localTxsLists, filtered)
			}
		}
		txLists = localTxsLists
	}

	log.Info("Transactions lists count", "count", len(txLists))

	return txLists, nil
}

// ProposeOp performs a proposing operation, fetching transactions
// from L2 execution engine's tx pool, splitting them by proposing constraints,
// and then proposing them to TaikoL1 contract.
func (p *Proposer) ProposeOp(ctx context.Context) error {
	// Check if it's time to propose unfiltered pool content.
	filterPoolContent := time.Now().Before(p.lastProposedAt.Add(p.MinProposingInternal))

	// Wait until L2 execution engine is synced at first.
	if err := p.rpc.WaitTillL2ExecutionEngineSynced(ctx); err != nil {
		return fmt.Errorf("failed to wait until L2 execution engine synced: %w", err)
	}

	// Add pending Bridge messages to the transaction list
	txList := types.Transactions{}
	p.bridgeMsgMu.Lock()
	if len(p.pendingBridgeMessages) > 0 {
		log.Info("Pending Bridge sendMessage transactions", "count", len(p.pendingBridgeMessages))
		for _, tx := range p.pendingBridgeMessages {
			txList = append(txList, tx)
		}
		p.pendingBridgeMessages = make(map[common.Hash]*types.Transaction) // Clear processed messages
	}
	p.bridgeMsgMu.Unlock()

	// TODO(@jmadibekov): Add a check that the transaction is valid and hasn't been mined already (whether by relayer or some other way) and include it in the proposed block

	log.Info(
		"Start fetching L2 execution engine's transaction pool content",
		"filterPoolContent", filterPoolContent,
		"lastProposedAt", p.lastProposedAt,
	)

	txLists, err := p.fetchPoolContent(filterPoolContent)
	if err != nil {
		return fmt.Errorf("ProposeOp: failed to fetch pool content: %w", err)
	}

	if len(txLists) == 0 {
		log.Debug("No transactions to propose")
		return nil
	}

	// Propose the profitable transactions lists
	return p.ProposeTxLists(ctx, txLists)
}

// ProposeTxLists proposes the given transactions lists to TaikoL1 smart contract.
func (p *Proposer) ProposeTxLists(ctx context.Context, txLists []types.Transactions) error {
	// Check if the current L2 chain is after ontake fork.
	state, err := rpc.GetProtocolStateVariables(p.rpc.TaikoL1, &bind.CallOpts{Context: ctx})
	if err != nil {
		return err
	}

	// If the current L2 chain is before ontake fork, propose the transactions lists one by one.
	if !p.chainConfig.IsOntake(new(big.Int).SetUint64(state.B.NumBlocks)) {
		g, gCtx := errgroup.WithContext(ctx)
		for _, txs := range p.getTxListsToPropose(txLists) {
			nonce, err := p.rpc.L1.PendingNonceAt(ctx, p.proposerAddress)
			if err != nil {
				log.Error("Failed to get proposer nonce", "error", err)
				break
			}

			log.Info("Proposer current pending nonce", "nonce", nonce)

			g.Go(func() error {
				if err := p.ProposeTxListLegacy(gCtx, txs); err != nil {
					return err
				}
				p.lastProposedAt = time.Now()
				return nil
			})

			if err := p.rpc.WaitL1NewPendingTransaction(ctx, p.proposerAddress, nonce); err != nil {
				log.Error("Failed to wait for new pending transaction", "error", err)
			}
		}

		return g.Wait()
	}

	// If the current L2 chain is after ontake fork, batch propose all L2 transactions lists.
	if err := p.ProposeTxListOntake(ctx, txLists); err != nil {
		return err
	}
	p.lastProposedAt = time.Now()
	return nil
}

// getTxListsToPropose returns the transaction lists to propose based on configuration limits
func (p *Proposer) getTxListsToPropose(txLists []types.Transactions) []types.Transactions {
	maxTxLists := utils.Min(p.MaxProposedTxListsPerEpoch, uint64(len(txLists)))
	return txLists[:maxTxLists]
}

// ProposeTxListLegacy proposes the given transactions list to TaikoL1 smart contract.
func (p *Proposer) ProposeTxListLegacy(
	ctx context.Context,
	txList types.Transactions,
) error {
	txListBytes, err := rlp.EncodeToBytes(txList)
	if err != nil {
		return fmt.Errorf("failed to encode transactions: %w", err)
	}

	compressedTxListBytes, err := utils.Compress(txListBytes)
	if err != nil {
		return err
	}

	proverAddress := p.proposerAddress
	if p.Config.ClientConfig.ProverSetAddress != rpc.ZeroAddress {
		proverAddress = p.Config.ClientConfig.ProverSetAddress
	}

	ok, err := rpc.CheckProverBalance(
		ctx,
		p.rpc,
		proverAddress,
		p.TaikoL1Address,
		p.protocolConfigs.LivenessBond,
	)

	if err != nil {
		log.Warn("Failed to check prover balance", "error", err)
		return err
	}

	if !ok {
		return errors.New("insufficient prover balance")
	}

	txCandidate, err := p.defaultTxBuilder.BuildLegacy(
		ctx,
		p.IncludeParentMetaHash,
		compressedTxListBytes,
	)
	if err != nil {
		log.Warn("Failed to build TaikoL1.proposeBlock transaction", "error", encoding.TryParsingCustomError(err))
		return err
	}

	if err := p.sendTx(ctx, txCandidate); err != nil {
		return err
	}

	log.Info("📝 Propose transactions succeeded", "txs", len(txList))

	metrics.ProposerProposedTxListsCounter.Add(1)
	metrics.ProposerProposedTxsCounter.Add(float64(len(txList)))

	return nil
}

// ProposeTxListOntake proposes the given transactions lists to TaikoL1 smart contract.
func (p *Proposer) ProposeTxListOntake(
	ctx context.Context,
	txLists []types.Transactions,
) error {
	totalTransactionFees, err := p.calculateTotalL2TransactionsFees(txLists)
	if err != nil {
		return err
	}

	txListsBytesArray, totalTxs, err := p.compressTxLists(txLists)
	if err != nil {
		return err
	}

	var proverAddress = p.proposerAddress

	if p.Config.ClientConfig.ProverSetAddress != rpc.ZeroAddress {
		proverAddress = p.Config.ClientConfig.ProverSetAddress
	}

	ok, err := rpc.CheckProverBalance(
		ctx,
		p.rpc,
		proverAddress,
		p.TaikoL1Address,
		new(big.Int).Mul(p.protocolConfigs.LivenessBond, new(big.Int).SetUint64(uint64(len(txLists)))),
	)

	if err != nil {
		log.Warn("Failed to check prover balance", "error", err)
		return err
	}

	if !ok {
		return errors.New("insufficient prover balance")
	}

	var txCandidate *txmgr.TxCandidate
	var cost *big.Int

	if p.initDone && !p.forceProposeOnce {
		txCandidate, cost, err = p.buildCheaperOnTakeTransaction(ctx, txListsBytesArray, isEmptyBlock(txLists))
		if err != nil {
			log.Warn("Failed to build TaikoL1.proposeBlocksV2 transaction", "error", encoding.TryParsingCustomError(err))
			return err
		}

		if p.checkProfitability {
			profitable, err := p.isProfitable(totalTransactionFees, cost)
			if err != nil {
				return err
			}
			if !profitable {
				log.Info("Proposing transaction is not profitable")
				return nil
			}
		}
	} else {
		txCandidate, err = p.txCallDataBuilder.BuildOntake(ctx, txListsBytesArray)
		if err != nil {
			return err
		}
	}

	err = RetryOnError(
		func() error {
			return p.sendTx(ctx, txCandidate)
		},
		"nonce too low",
		3,
		1*time.Second)
	if err != nil {
		return err
	}
	p.initDone = true
	p.forceProposeOnce = false

	log.Info("📝 Batch propose transactions succeeded", "totalTxs", totalTxs)

	metrics.ProposerProposedTxListsCounter.Add(float64(len(txLists)))
	metrics.ProposerProposedTxsCounter.Add(float64(totalTxs))

	return nil
}

func RetryOnError(operation func() error, retryon string, maxRetries int, delay time.Duration) error {
	for i := 0; i < maxRetries; i++ {
		err := operation()
		if err == nil {
			return nil // Success
		}
		if !strings.Contains(err.Error(), retryon) {
			return err // Stop retrying on unexpected errors
		}

		fmt.Printf("Retrying due to: %v (attempt %d/%d)\n", err, i+1, maxRetries)
		time.Sleep(delay)
	}
	return fmt.Errorf("operation failed after %d retries", maxRetries)
}

func (p *Proposer) buildCheaperOnTakeTransaction(ctx context.Context,
	txListsBytesArray [][]byte, isEmptyBlock bool) (*txmgr.TxCandidate, *big.Int, error) {
	txCallData, err := p.txCallDataBuilder.BuildOntake(ctx, txListsBytesArray)
	if err != nil {
		return nil, nil, err
	}

	var tx *txmgr.TxCandidate
	var cost *big.Int

	if p.txBlobBuilder != nil && !isEmptyBlock {
		txBlob, err := p.txBlobBuilder.BuildOntake(ctx, txListsBytesArray)
		if err != nil {
			return nil, nil, err
		}

		tx, cost, err = p.chooseCheaperTransaction(txCallData, txBlob)
		if err != nil {
			return nil, nil, err
		}
	} else {
		cost, err = p.getTransactionCost(txCallData, nil)
		if err != nil {
			return nil, nil, err
		}
		tx = txCallData
	}

	return tx, cost, nil
}

func isEmptyBlock(txLists []types.Transactions) bool {
	for _, txs := range txLists {
		for _, tx := range txs {
			if tx.To() != nil {
				return false
			}
		}
	}
	return true
}

func (p *Proposer) chooseCheaperTransaction(
	txCallData *txmgr.TxCandidate,
	txBlob *txmgr.TxCandidate,
) (*txmgr.TxCandidate, *big.Int, error) {
	log.Debug("Choosing cheaper transaction")
	calldataTxCost, err := p.getTransactionCost(txCallData, nil)
	if err != nil {
		return nil, nil, err
	}

	totalBlobCost, err := p.getBlobTxCost(txBlob)
	if err != nil {
		return nil, nil, err
	}

	if calldataTxCost.Cmp(totalBlobCost) > 0 {
		log.Info("Using blob tx", "totalBlobCost", totalBlobCost)
		return txBlob, totalBlobCost, nil
	}

	log.Info("Using calldata tx", "calldataTxCost", calldataTxCost)
	return txCallData, calldataTxCost, nil
}

// compressTxLists compresses transaction lists and returns compressed bytes array and transaction counts
func (p *Proposer) compressTxLists(txLists []types.Transactions) ([][]byte, int, error) {
	var (
		txListsBytesArray [][]byte
		txNums            []int
		totalTxs          int
	)

	for _, txs := range txLists {
		txListBytes, err := rlp.EncodeToBytes(txs)
		if err != nil {
			return nil, 0, fmt.Errorf("failed to encode transactions: %w", err)
		}

		compressedTxListBytes, err := utils.Compress(txListBytes)
		if err != nil {
			return nil, 0, err
		}

		txListsBytesArray = append(txListsBytesArray, compressedTxListBytes)
		txNums = append(txNums, len(txs))
		totalTxs += len(txs)
	}

	log.Debug("Compressed transaction lists", "txs", txNums)

	return txListsBytesArray, totalTxs, nil
}

// updateProposingTicker updates the internal proposing timer.
func (p *Proposer) updateProposingTicker() {
	if p.proposingTimer != nil {
		p.proposingTimer.Stop()
	}

	var duration time.Duration
	if p.ProposeInterval != 0 {
		duration = p.ProposeInterval
	} else {
		// Random number between 12 - 120
		randomSeconds := rand.Intn(120-11) + 12 // nolint: gosec
		duration = time.Duration(randomSeconds) * time.Second
	}

	p.proposingTimer = time.NewTimer(duration)
}

// sendTx is the internal function to send a transaction with a selected tx manager.
func (p *Proposer) sendTx(ctx context.Context, txCandidate *txmgr.TxCandidate) error {
	txMgr, isPrivate := p.txmgrSelector.Select()
	receipt, err := txMgr.Send(ctx, *txCandidate)
	if err != nil {
		log.Warn(
			"Failed to send TaikoL1.proposeBlock / TaikoL1.proposeBlocksV2 transaction by tx manager",
			"isPrivateMempool", isPrivate,
			"error", encoding.TryParsingCustomError(err),
		)
		if isPrivate {
			p.txmgrSelector.RecordPrivateTxMgrFailed()
		}
		return err
	}

	if receipt.Status != types.ReceiptStatusSuccessful {
		return fmt.Errorf("failed to propose block: %s", receipt.TxHash.Hex())
	}
	return nil
}

// Name returns the application name.
func (p *Proposer) Name() string {
	return "proposer"
}

// isProfitable checks if a transaction list is profitable to propose

// Profitability is determined by comparing the revenue from transaction fees
// to the costs of proposing and proving the block. Specifically:
func (p *Proposer) isProfitable(totalTransactionFees *big.Int, proposingCosts *big.Int) (bool, error) {
	costs, err := p.estimateTotalCosts(proposingCosts)
	if err != nil {
		return false, err
	}

	log.Debug("isProfitable", "total L2 fees", totalTransactionFees, "total L1 costs", costs)

	return totalTransactionFees.Cmp(costs) > 0, nil
}

func (p *Proposer) calculateTotalL2TransactionsFees(txLists []types.Transactions) (*big.Int, error) {
	totalFeesCollected := new(big.Int)
	previousHeader, err := p.rpc.L2.HeaderByNumber(p.ctx, nil)
	if err != nil {
		return nil, err
	}
	// Calculate baseFee for the next block using 1559 rules
	gasUsed := big.NewInt(int64(previousHeader.GasUsed))
	gasLimit := big.NewInt(int64(previousHeader.GasLimit))
	previousBlockBaseFee := previousHeader.BaseFee

	targetGasLimit := new(big.Int).Div(gasLimit, big.NewInt(2))
	gasDelta := new(big.Int).Sub(gasUsed, targetGasLimit)

	scalingFactor := new(big.Int).Mul(gasDelta, big.NewInt(100))
	scalingFactor.Div(scalingFactor, targetGasLimit)
	scalingFactor.Div(scalingFactor, big.NewInt(8))

	// Apply scaling factor to previous base fee
	newBaseFee := new(big.Int).Mul(previousBlockBaseFee, big.NewInt(100)) // Multiply by 100 for precision
	newBaseFee.Mul(newBaseFee, big.NewInt(100+scalingFactor.Int64()))     // Multiply by (1 + scaling factor)
	newBaseFee.Div(newBaseFee, big.NewInt(10000))

	for i, txs := range txLists {
		var filteredTxs types.Transactions

		for _, tx := range txs {
			effectiveTip, err := tx.EffectiveGasTip(newBaseFee)
			if err != nil {
				continue
			}
			baseFeeForProposer := p.getPercentageFromBaseFeeToTheProposer(newBaseFee)
			tipFeeWithBaseFee := new(big.Int).Add(effectiveTip, baseFeeForProposer)
			gasConsumed := big.NewInt(int64(tx.Gas()))
			feesFromTx := new(big.Int).Mul(gasConsumed, tipFeeWithBaseFee)
			totalFeesCollected.Add(totalFeesCollected, feesFromTx)
			filteredTxs = append(filteredTxs, tx)
		}

		txLists[i] = filteredTxs
	}
	return totalFeesCollected, nil
}

func (p *Proposer) getPercentageFromBaseFeeToTheProposer(num *big.Int) *big.Int {
	if p.chainConfig.ProtocolConfigs.BaseFeeConfig.SharingPctg == 0 {
		return big.NewInt(0)
	}
	result := new(big.Int).Mul(num, big.NewInt(int64(p.chainConfig.ProtocolConfigs.BaseFeeConfig.SharingPctg)))
	return new(big.Int).Div(result, big.NewInt(100))
}

func (p *Proposer) getBlobTxCost(txCandidate *txmgr.TxCandidate) (*big.Int, error) {
	// Get current blob base fee
	blobBaseFee, err := p.rpc.L1.BlobBaseFee(p.ctx)
	if err != nil {
		return nil, err
	}

	blobTxCost, err := p.getTransactionCost(txCandidate, blobBaseFee)
	if err != nil {
		return nil, err
	}
	blobCost, err := p.getBlobCost(txCandidate.Blobs, blobBaseFee)
	if err != nil {
		return nil, err
	}
	return new(big.Int).Add(blobTxCost, blobCost), nil
}

func (p *Proposer) getTransactionCost(txCandidate *txmgr.TxCandidate, blobBaseFee *big.Int) (*big.Int, error) {
	log.Debug("getTransactionCost", "blobBaseFee", blobBaseFee)
	// Get the current L1 gas price
	gasPrice, err := p.rpc.L1.SuggestGasPrice(p.ctx)
	if err != nil {
		return nil, fmt.Errorf("getTransactionCost: failed to get gas price: %w", err)
	}

	var msg ethereum.CallMsg
	if blobBaseFee != nil {
		blobHashes, err := calculateBlobHashes(txCandidate.Blobs)
		if err != nil {
			return nil, fmt.Errorf("getTransactionCost: failed to calculate blob hashes: %w", err)
		}
		msg = ethereum.CallMsg{
			From:          p.proposerAddress,
			To:            txCandidate.To,
			Data:          txCandidate.TxData,
			Gas:           0,
			Value:         nil,
			BlobGasFeeCap: blobBaseFee,
			BlobHashes:    blobHashes,
		}
	} else {
		msg = ethereum.CallMsg{
			From:  p.proposerAddress,
			To:    txCandidate.To,
			Data:  txCandidate.TxData,
			Gas:   0,
			Value: nil,
		}
	}

	estimatedGasUsage, err := p.rpc.L1.EstimateGas(p.ctx, msg)
	if err != nil {
		return nil, fmt.Errorf("getTransactionCost: failed to estimate gas: %w", err)
	}

	log.Debug("getTransactionCost", "estimatedGasUsage", estimatedGasUsage)

	return new(big.Int).Mul(gasPrice, new(big.Int).SetUint64(estimatedGasUsage)), nil
}

func calculateBlobHashes(blobs []*eth.Blob) ([]common.Hash, error) {
	log.Debug("Calculating blob hashes")

	var blobHashes []common.Hash
	for _, blob := range blobs {
		commitment, err := blob.ComputeKZGCommitment()
		if err != nil {
			return nil, err
		}
		blobHash := kzg4844.CalcBlobHashV1(sha256.New(), &commitment)
		blobHashes = append(blobHashes, blobHash)
	}
	return blobHashes, nil
}

func (p *Proposer) getBlobCost(blobs []*eth.Blob, blobBaseFee *big.Int) (*big.Int, error) {
	// Each blob costs 1 blob gas
	totalBlobGas := uint64(len(blobs))

	// Total cost is blob gas * blob base fee
	return new(big.Int).Mul(
		new(big.Int).SetUint64(totalBlobGas),
		blobBaseFee,
	), nil
}

func adjustForPriceFluctuation(gasPrice *big.Int, percentage uint64) *big.Int {
	temp := new(big.Int).Mul(gasPrice, new(big.Int).SetUint64(uint64(100)+percentage))
	return new(big.Int).Div(temp, big.NewInt(100))
}

// Total Costs =
// gas needed for proof verification * (150% of gas price on L1) +
// 150% of block proposal costs +
// off chain proving costs (estimated with a margin for the provers' revenue)
func (p *Proposer) estimateTotalCosts(proposingCosts *big.Int) (*big.Int, error) {
	if p.OffChainCosts == nil {
		log.Warn("Off-chain costs is not set, using 0")
		p.OffChainCosts = big.NewInt(0)
	}

	log.Debug(
		"Proposing block costs details",
		"proposingCosts", proposingCosts,
		"gasNeededForProving", p.GasNeededForProvingBlock,
		"priceFluctuation", p.PriceFluctuationModifier,
		"offChainCosts", p.OffChainCosts,
	)

	l1GasPrice, err := p.rpc.L1.SuggestGasPrice(p.ctx)
	if err != nil {
		return nil, err
	}

	adjustedL1GasPrice := adjustForPriceFluctuation(l1GasPrice, p.PriceFluctuationModifier)
	adjustedProposingCosts := adjustForPriceFluctuation(proposingCosts, p.PriceFluctuationModifier)
	l1Costs := new(big.Int).Mul(new(big.Int).SetUint64(p.GasNeededForProvingBlock), adjustedL1GasPrice)
	l1Costs = new(big.Int).Add(l1Costs, adjustedProposingCosts)

	totalCosts := new(big.Int).Add(l1Costs, p.OffChainCosts)

	return totalCosts, nil
}
