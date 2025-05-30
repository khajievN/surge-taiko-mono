package proposer

import (
	"crypto/ecdsa"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/ethereum-optimism/optimism/op-service/txmgr"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/urfave/cli/v2"

	"github.com/taikoxyz/taiko-mono/packages/taiko-client/cmd/flags"
	"github.com/taikoxyz/taiko-mono/packages/taiko-client/internal/utils"
	"github.com/taikoxyz/taiko-mono/packages/taiko-client/pkg/jwt"
	"github.com/taikoxyz/taiko-mono/packages/taiko-client/pkg/rpc"

	pkgFlags "github.com/taikoxyz/taiko-mono/packages/taiko-client/pkg/flags"
)

// Config contains all configurations to initialize a Taiko proposer.
type Config struct {
	*rpc.ClientConfig
	L1ProposerPrivKey          *ecdsa.PrivateKey
	L2SuggestedFeeRecipient    common.Address
	ExtraData                  string
	ProposeInterval            time.Duration
	LocalAddresses             []common.Address
	LocalAddressesOnly         bool
	MinGasUsed                 uint64
	MinTxListBytes             uint64
	MinTip                     uint64
	MinProposingInternal       time.Duration
	AllowZeroInterval          uint64
	MaxProposedTxListsPerEpoch uint64
	ProposeBlockTxGasLimit     uint64
	IncludeParentMetaHash      bool
	BlobAllowed                bool
	TxmgrConfigs               *txmgr.CLIConfig
	PrivateTxmgrConfigs        *txmgr.CLIConfig
	CheckProfitability         bool
	AllowEmptyBlocks           bool
	GasNeededForProvingBlock   uint64
	PriceFluctuationModifier   uint64
	OffChainCosts              *big.Int
}

// NewConfigFromCliContext initializes a Config instance from
// command line flags.
func NewConfigFromCliContext(c *cli.Context) (*Config, error) {
	jwtSecret, err := jwt.ParseSecretFromFile(c.String(flags.JWTSecret.Name))
	if err != nil {
		return nil, fmt.Errorf("invalid JWT secret file: %w", err)
	}

	l1ProposerPrivKey, err := crypto.ToECDSA(common.FromHex(c.String(flags.L1ProposerPrivKey.Name)))
	if err != nil {
		return nil, fmt.Errorf("invalid L1 proposer private key: %w", err)
	}

	l2SuggestedFeeRecipient := c.String(flags.L2SuggestedFeeRecipient.Name)
	if !common.IsHexAddress(l2SuggestedFeeRecipient) {
		return nil, fmt.Errorf("invalid L2 suggested fee recipient address: %s", l2SuggestedFeeRecipient)
	}

	var localAddresses []common.Address
	if c.IsSet(flags.TxPoolLocals.Name) {
		for _, account := range strings.Split(c.String(flags.TxPoolLocals.Name), ",") {
			if trimmed := strings.TrimSpace(account); !common.IsHexAddress(trimmed) {
				return nil, fmt.Errorf("invalid account in --txpool.locals: %s", trimmed)
			}
			localAddresses = append(localAddresses, common.HexToAddress(account))
		}
	}

	minTip, err := utils.GWeiToWei(c.Float64(flags.MinTip.Name))
	if err != nil {
		return nil, err
	}

	maxProposedTxListsPerEpoch := c.Uint64(flags.MaxProposedTxListsPerEpoch.Name)
	if maxProposedTxListsPerEpoch > 2 {
		return nil, fmt.Errorf("max proposed tx lists per epoch should not exceed 2, got: %d", maxProposedTxListsPerEpoch)
	}

	checkProfitability := c.Bool(flags.CheckProfitability.Name)
	allowEmptyBlocks := c.Bool(flags.AllowEmptyBlocks.Name)
	gasNeededForProvingBlock := c.Uint64(flags.GasNeededForProvingBlock.Name)
	priceFluctuationModifier := c.Uint64(flags.PriceFluctuationModifier.Name)

	offChainCosts, ok := new(big.Int).SetString(c.String(flags.OffChainCosts.Name), 10)
	if !ok {
		return nil, fmt.Errorf("invalid off-chain costs: %s", c.String(flags.OffChainCosts.Name))
	}

	if offChainCosts.Cmp(abi.MaxUint256) == 1 {
		return nil, fmt.Errorf("off-chain costs value larger than max uint256")
	}

	return &Config{
		ClientConfig: &rpc.ClientConfig{
			L1Endpoint:        c.String(flags.L1WSEndpoint.Name),
			L2Endpoint:        c.String(flags.L2HTTPEndpoint.Name),
			TaikoL1Address:    common.HexToAddress(c.String(flags.TaikoL1Address.Name)),
			TaikoL2Address:    common.HexToAddress(c.String(flags.TaikoL2Address.Name)),
			L2EngineEndpoint:  c.String(flags.L2AuthEndpoint.Name),
			JwtSecret:         string(jwtSecret),
			TaikoTokenAddress: common.HexToAddress(c.String(flags.TaikoTokenAddress.Name)),
			Timeout:           c.Duration(flags.RPCTimeout.Name),
			ProverSetAddress:  common.HexToAddress(c.String(flags.ProverSetAddress.Name)),
			InboxAddress:      common.HexToAddress(c.String(flags.InboxAddress.Name)),
			BridgeAddress:     common.HexToAddress(c.String(flags.BridgeAddress.Name)),
		},
		L1ProposerPrivKey:          l1ProposerPrivKey,
		L2SuggestedFeeRecipient:    common.HexToAddress(l2SuggestedFeeRecipient),
		ExtraData:                  c.String(flags.ExtraData.Name),
		ProposeInterval:            c.Duration(flags.ProposeInterval.Name),
		LocalAddresses:             localAddresses,
		LocalAddressesOnly:         c.Bool(flags.TxPoolLocalsOnly.Name),
		MinGasUsed:                 c.Uint64(flags.MinGasUsed.Name),
		MinTxListBytes:             c.Uint64(flags.MinTxListBytes.Name),
		MinTip:                     minTip.Uint64(),
		MinProposingInternal:       c.Duration(flags.MinProposingInternal.Name),
		MaxProposedTxListsPerEpoch: maxProposedTxListsPerEpoch,
		AllowZeroInterval:          c.Uint64(flags.AllowZeroInterval.Name),
		ProposeBlockTxGasLimit:     c.Uint64(flags.TxGasLimit.Name),
		IncludeParentMetaHash:      c.Bool(flags.ProposeBlockIncludeParentMetaHash.Name),
		BlobAllowed:                c.Bool(flags.BlobAllowed.Name),
		TxmgrConfigs: pkgFlags.InitTxmgrConfigsFromCli(
			c.String(flags.L1WSEndpoint.Name),
			l1ProposerPrivKey,
			c,
		),
		PrivateTxmgrConfigs: pkgFlags.InitTxmgrConfigsFromCli(
			c.String(flags.L1PrivateEndpoint.Name),
			l1ProposerPrivKey,
			c,
		),
		CheckProfitability:       checkProfitability,
		AllowEmptyBlocks:         allowEmptyBlocks,
		GasNeededForProvingBlock: gasNeededForProvingBlock,
		PriceFluctuationModifier: priceFluctuationModifier,
		OffChainCosts:            offChainCosts,
	}, nil
}
