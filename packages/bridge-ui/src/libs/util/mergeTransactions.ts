import type { BridgeTransaction } from '$libs/bridge';

type MergeResult = {
  mergedTransactions: BridgeTransaction[];
  outdatedLocalTransactions: BridgeTransaction[];
};

export const mergeAndCaptureOutdatedTransactions = (
  localTxs: BridgeTransaction[],
  relayerTx: BridgeTransaction[],
): MergeResult => {
  const relayerTxMap: Map<string, BridgeTransaction> = new Map();
  relayerTx.forEach((tx) => relayerTxMap.set(tx.srcTxHash, tx));
  const addedRelayerTxSet = new Set<string>();

  const outdatedLocalTransactions: BridgeTransaction[] = [];
  const mergedTransactions: BridgeTransaction[] = [];

  for (const tx of localTxs) {
    if (!relayerTxMap.has(tx.srcTxHash)) {
      mergedTransactions.push(tx);
    } else {
      outdatedLocalTransactions.push(tx);
    }
  }

  for (const tx of relayerTx) {
    if (addedRelayerTxSet.has(tx.srcTxHash)) {
      continue;
    }
    mergedTransactions.push(tx);
    addedRelayerTxSet.add(tx.srcTxHash);
  }

  return { mergedTransactions, outdatedLocalTransactions };
};

