#!/bin/sh

set -e

export PRIVATE_KEY={}
#export FORK_URL="https://frequent-chaotic-energy.ethereum-hoodi.quiknode.pro/1638b088524738ffa6c8356f7d9f5e49b0d5b176"
#export FORK_URL="https://warmhearted-convincing-crater.ethereum-holesky.quiknode.pro/528958ae83ded939ad04c8af6306415e17ea202e/"
export FORK_URL="http://3.107.178.1:5052"

L2_CHAINID=763375 \
L2_GENESIS_HASH=0xbd0c22e1b7f46e950b53a072996073ec5321cae8fdd56e0a607ab95daeddf587 \
OWNER_MULTISIG="0xF10bAd9d0c9226DE6595c4Aaca49b60b06F390f8" \
OWNER_MULTISIG_SIGNERS="0xF10bAd9d0c9226DE6595c4Aaca49b60b06F390f8" \
MAX_LIVENESS_DISRUPTION_PERIOD=604800 \
MIN_LIVENESS_STREAK=604800 \
VERIFIER_OWNER="0xF10bAd9d0c9226DE6595c4Aaca49b60b06F390f8" \
FOUNDRY_PROFILE="layer1" \
TIMELOCK_PERIOD=0 \
NETWORK="devnet" \
forge script ./script/layer1/surge/DeploySurgeOnL1.s.sol:DeploySurgeOnL1 \
    --fork-url $FORK_URL \
    --broadcast \
    --ffi \
    -vv \
    --private-key $PRIVATE_KEY \
    --block-gas-limit 100000000
