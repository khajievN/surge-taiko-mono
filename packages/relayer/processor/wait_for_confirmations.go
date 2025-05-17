package processor

import (
	"context"
	"time"
	"log/slog"
	"github.com/ethereum/go-ethereum/common"
	"github.com/taikoxyz/taiko-mono/packages/relayer"
)

// waitForConfirmations waits for the given transaction to reach N confs
// before returning
func (p *Processor) waitForConfirmations(ctx context.Context, txHash common.Hash) error {
	slog.Info("Starting waitForConfirmations", "txHash", txHash.Hex())

	// Log the timeout duration
	timeoutDuration := time.Duration(p.confTimeoutInSeconds) * time.Second
	slog.Info("Setting context timeout", "timeoutDuration", timeoutDuration)

	ctx, cancelFunc := context.WithTimeout(ctx, timeoutDuration)
	defer cancelFunc()

	// Log the number of confirmations required
	slog.Info("Waiting for confirmations", "confirmations", p.confirmations)

	// Log the Ethereum client being used
	slog.Info("Using Ethereum client", "client", p.srcEthClient)

	// Call WaitConfirmations and log the result
	err := relayer.WaitConfirmations(
		ctx,
		p.srcEthClient,
		p.confirmations,
		txHash,
	)
	if err != nil {
		slog.Error("Error while waiting for confirmations", "error", err)
		return err
	}

	slog.Info("Transaction confirmed", "txHash", txHash.Hex())
	return nil
}
