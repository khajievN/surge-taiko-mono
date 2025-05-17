package processor

import (
	"context"
	"log/slog"
	"time"

	"github.com/taikoxyz/taiko-mono/packages/relayer"
)

// waitHeaderSynced waits for a event to appear in the database from the indexer
// for the type "ChainDataSynced" to be greater or less than the given blockNum.
// this is used to make sure a valid proof can be generated and verified on chain.
func (p *Processor) waitHeaderSynced(
	ctx context.Context,
	ethClient ethClient,
	hopChainId uint64,
	blockNum uint64,
) (*relayer.Event, error) {
	slog.Info("Starting waitHeaderSynced", "hopChainId", hopChainId, "blockNum", blockNum)

	chainId, err := ethClient.ChainID(ctx)
	if err != nil {
		slog.Error("Failed to get chain ID", "error", err)
		return nil, err
	}
	slog.Info("Retrieved chain ID", "chainId", chainId.Uint64())

	// check once before ticker interval
	slog.Info("Checking for ChainDataSynced event before starting ticker")
	event, err := p.eventRepo.ChainDataSyncedEventByBlockNumberOrGreater(
		ctx,
		hopChainId,
		chainId.Uint64(),
		blockNum,
	)
	if err != nil {
		slog.Error("Error querying ChainDataSyncedEventByBlockNumberOrGreater", "error", err)
		return nil, err
	}

	if event != nil {
		slog.Info("ChainDataSynced event found",
			"syncedBlockID", event.BlockID,
			"blockIDWaitingFor", blockNum,
		)
		return event, nil
	}

	slog.Info("No ChainDataSynced event found, starting ticker loop")
	ticker := time.NewTicker(time.Duration(p.headerSyncIntervalSeconds) * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Warn("Context canceled while waiting for ChainDataSynced event")
			return nil, ctx.Err()
		case <-ticker.C:
			slog.Info("Ticker triggered, checking for ChainDataSynced event")
			slog.Info("Calling ChainDataSyncedEventByBlockNumberOrGreater",
            	"ctx", ctx,
            	"hopChainId", hopChainId,
            	"syncedChainId", chainId.Uint64(),
            	"blockNum", blockNum,
            )
			event, err := p.eventRepo.ChainDataSyncedEventByBlockNumberOrGreater(
				ctx,
				hopChainId,
				chainId.Uint64(),
				blockNum,
			)
			if err != nil {
				slog.Error("Error querying ChainDataSyncedEventByBlockNumberOrGreater", "error", err)
				return nil, err
			}

			if event != nil {
				slog.Info("ChainDataSynced event found",
					"syncedBlockID", event.BlockID,
					"blockIDWaitingFor", blockNum,
				)
				return event, nil
			}
			slog.Info("No ChainDataSynced event found, continuing to wait")
		}
	}
}
