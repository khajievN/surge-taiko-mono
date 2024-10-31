package builder

import (
	"context"
	"math/big"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"

	"github.com/taikoxyz/taiko-mono/packages/taiko-client/pkg/rpc"
)

func (s *TransactionBuilderTestSuite) TestBuildCalldata() {
	state, err := rpc.GetProtocolStateVariables(s.RPCClient.TaikoL1, &bind.CallOpts{Context: context.Background()})
	s.Nil(err)

	if s.chainConfig.IsOntake(new(big.Int).SetUint64(state.B.NumBlocks)) {
		tx, err := s.calldataTxBuilder.BuildOntake(context.Background(), [][]byte{{1}, {2}})
		s.Nil(err)
		s.Nil(tx.Blobs)

		_, err = s.calldataTxBuilder.BuildLegacy(context.Background(), false, []byte{1})
		s.Error(err, "legacy transaction builder is not supported after ontake fork")
	} else {
		tx, err := s.calldataTxBuilder.BuildLegacy(context.Background(), false, []byte{1})
		s.Nil(err)
		s.Nil(tx.Blobs)

		_, err = s.calldataTxBuilder.BuildOntake(context.Background(), [][]byte{{1}, {2}})
		s.Error(err, "ontake transaction builder is not supported before ontake fork")
	}
}
