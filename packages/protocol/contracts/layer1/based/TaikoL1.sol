// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import "src/shared/common/EssentialContract.sol";
import "./LibData.sol";
import "./LibProposing.sol";
import "./LibProving.sol";
import "./LibVerifying.sol";
import "./TaikoEvents.sol";
import "./ITaikoL1.sol";

/// @title TaikoL1
/// @notice This contract serves as the "base layer contract" of the Taiko protocol, providing
/// functionalities for proposing, proving, and verifying blocks. The term "base layer contract"
/// means that although this is usually deployed on L1, it can also be deployed on L2s to create
/// L3 "inception layers". The contract also handles the deposit and withdrawal of Ether.
/// Additionally, this contract doesn't hold any Ether. Ether deposited to L2 are held
/// by the Bridge contract.
/// @dev Labeled in AddressResolver as "taiko"
/// @custom:security-contact security@taiko.xyz
contract TaikoL1 is EssentialContract, ITaikoL1, TaikoEvents {
    /// @notice The TaikoL1 state.
    TaikoData.State public state;

    uint256[50] private __gap;

    error L1_FORK_ERROR();
    error L1_INVALID_PARAMS();

    modifier whenProvingNotPaused() {
        if (state.slotB.provingPaused) revert LibProving.L1_PROVING_PAUSED();
        _;
    }

    modifier emitEventForClient() {
        _;
        emit StateVariablesUpdated(state.slotB);
    }

    /// @notice Initializes the contract.
    /// @param _owner The owner of this contract. msg.sender will be used if this value is zero.
    /// @param _rollupAddressManager The address of the {AddressManager} contract.
    /// @param _genesisBlockHash The block hash of the genesis block.
    /// @param _toPause true to pause the contract by default.
    function init(
        address _owner,
        address _rollupAddressManager,
        bytes32 _genesisBlockHash,
        bool _toPause
    )
        external
        initializer
    {
        __Essential_init(_owner, _rollupAddressManager);
        LibUtils.init(state, getConfig(), _genesisBlockHash);
        if (_toPause) _pause();
    }

    function init2() external onlyOwner reinitializer(2) {
        // reset some previously used slots for future reuse
        state.slotB.__reservedB1 = 0;
        state.slotB.__reservedB2 = 0;
        state.slotB.__reservedB3 = 0;
        state.__reserve1 = 0;
    }

    /// @inheritdoc ITaikoL1
    function proposeBlock(
        bytes calldata _params,
        bytes calldata _txList
    )
        external
        payable
        whenNotPaused
        nonReentrant
        emitEventForClient
        returns (TaikoData.BlockMetadata memory meta_, TaikoData.EthDeposit[] memory deposits_)
    {
        TaikoData.Config memory config = getConfig();

        TaikoData.BlockMetadataV2 memory metaV2;
        (meta_, metaV2) = LibProposing.proposeBlock(state, config, this, _params, _txList);
        if (metaV2.id >= config.ontakeForkHeight) revert L1_FORK_ERROR();
        deposits_ = new TaikoData.EthDeposit[](0);
    }

    function proposeBlockV2(
        bytes calldata _params,
        bytes calldata _txList
    )
        external
        whenNotPaused
        nonReentrant
        emitEventForClient
        returns (TaikoData.BlockMetadataV2 memory meta_)
    {
        TaikoData.Config memory config = getConfig();
        (, meta_) = LibProposing.proposeBlock(state, config, this, _params, _txList);
        if (meta_.id < config.ontakeForkHeight) revert L1_FORK_ERROR();
    }

    /// @inheritdoc ITaikoL1
    function proposeBlocksV2(
        bytes[] calldata _paramsArr,
        bytes[] calldata _txListArr
    )
        external
        whenNotPaused
        nonReentrant
        emitEventForClient
        returns (TaikoData.BlockMetadataV2[] memory metaArr_)
    {
        TaikoData.Config memory config = getConfig();
        (, metaArr_) = LibProposing.proposeBlocks(state, config, this, _paramsArr, _txListArr);
        for (uint256 i; i < metaArr_.length; ++i) {
            if (metaArr_[i].id < config.ontakeForkHeight) revert L1_FORK_ERROR();
        }
    }

    /// @inheritdoc ITaikoL1
    function proveBlock(
        uint64 _blockId,
        bytes calldata _input
    )
        external
        whenNotPaused
        whenProvingNotPaused
        nonReentrant
        emitEventForClient
    {
        LibProving.proveBlock(state, getConfig(), this, _blockId, _input);
    }

    /// @inheritdoc ITaikoL1
    function proveBlocks(
        uint64[] calldata _blockIds,
        bytes[] calldata _inputs,
        bytes calldata _batchProof
    )
        external
        whenNotPaused
        whenProvingNotPaused
        nonReentrant
        emitEventForClient
    {
        LibProving.proveBlocks(state, getConfig(), this, _blockIds, _inputs, _batchProof);
    }

    /// @inheritdoc ITaikoL1
    function verifyBlocks(uint64 _maxBlocksToVerify)
        external
        whenNotPaused
        whenProvingNotPaused
        nonReentrant
        emitEventForClient
    {
        LibVerifying.verifyBlocks(state, getConfig(), this, _maxBlocksToVerify);
    }

    /// @inheritdoc ITaikoL1
    function pauseProving(bool _pause) external {
        // Surge: disable pausing of proving
        _disable();
        _authorizePause(msg.sender, _pause);
        LibProving.pauseProving(state, _pause);
    }

    /// @inheritdoc ITaikoL1
    function depositBond() external payable whenNotPaused {
        LibBonds.depositBond(state, msg.value);
    }

    /// @inheritdoc ITaikoL1
    function withdrawBond(uint256 _amount) external whenNotPaused {
        LibBonds.withdrawBond(state, _amount);
    }

    /// @notice Gets the current bond balance of a given address.
    /// @return The current bond balance.
    function bondBalanceOf(address _user) external view returns (uint256) {
        return LibBonds.bondBalanceOf(state, _user);
    }

    /// @inheritdoc ITaikoL1
    function getVerifiedBlockProver(uint64 _blockId) external view returns (address prover_) {
        return LibVerifying.getVerifiedBlockProver(state, getConfig(), _blockId);
    }

    /// @notice Gets the details of a block.
    /// @param _blockId Index of the block.
    /// @return blk_ The block.
    function getBlock(uint64 _blockId) external view returns (TaikoData.Block memory blk_) {
        (TaikoData.BlockV2 memory blk,) = LibUtils.getBlock(state, getConfig(), _blockId);
        blk_ = LibData.blockV2toV1(blk);
    }

    /// @inheritdoc ITaikoL1
    function getBlockV2(uint64 _blockId) external view returns (TaikoData.BlockV2 memory blk_) {
        (blk_,) = LibUtils.getBlock(state, getConfig(), _blockId);
    }

    /// @notice This function will revert if the transition is not found. This function will revert
    /// if the transition is not found.
    /// @param _blockId Index of the block.
    /// @param _parentHash Parent hash of the block.
    /// @return The state transition data of the block.
    function getTransition(
        uint64 _blockId,
        bytes32 _parentHash
    )
        external
        view
        returns (TaikoData.TransitionState memory)
    {
        return LibUtils.getTransition(state, getConfig(), _blockId, _parentHash);
    }

    /// @notice Gets the state transitions for a batch of block. For transition that doesn't exist,
    /// the corresponding transition state will be empty.
    /// @param _blockIds Index of the blocks.
    /// @param _parentHashes Parent hashes of the blocks.
    /// @return The state transition array of the blocks.
    function getTransitions(
        uint64[] calldata _blockIds,
        bytes32[] calldata _parentHashes
    )
        external
        view
        returns (TaikoData.TransitionState[] memory)
    {
        return LibUtils.getTransitions(state, getConfig(), _blockIds, _parentHashes);
    }

    /// @inheritdoc ITaikoL1
    function getTransition(
        uint64 _blockId,
        uint32 _tid
    )
        external
        view
        returns (TaikoData.TransitionState memory)
    {
        return LibUtils.getTransition(state, getConfig(), _blockId, _tid);
    }

    /// @notice Returns information about the last verified block.
    /// @return blockId_ The last verified block's ID.
    /// @return blockHash_ The last verified block's blockHash.
    /// @return stateRoot_ The last verified block's stateRoot.
    /// @return verifiedAt_ The timestamp this block is verified at.
    function getLastVerifiedBlock()
        external
        view
        returns (uint64 blockId_, bytes32 blockHash_, bytes32 stateRoot_, uint64 verifiedAt_)
    {
        blockId_ = state.slotB.lastVerifiedBlockId;
        (blockHash_, stateRoot_, verifiedAt_) = LibUtils.getBlockInfo(state, getConfig(), blockId_);
    }

    /// @notice Returns information about the last synchronized block.
    /// @return blockId_ The last verified block's ID.
    /// @return blockHash_ The last verified block's blockHash.
    /// @return stateRoot_ The last verified block's stateRoot.
    /// @return verifiedAt_ The timestamp this block is verified at.
    function getLastSyncedBlock()
        external
        view
        returns (uint64 blockId_, bytes32 blockHash_, bytes32 stateRoot_, uint64 verifiedAt_)
    {
        blockId_ = state.slotA.lastSyncedBlockId;
        (blockHash_, stateRoot_, verifiedAt_) = LibUtils.getBlockInfo(state, getConfig(), blockId_);
    }

    /// @notice Gets the state variables of the TaikoL1 contract.
    /// @dev This method can be deleted once node/client stops using it.
    /// @return State variables stored at SlotA.
    /// @return State variables stored at SlotB.
    function getStateVariables()
        external
        view
        returns (TaikoData.SlotA memory, TaikoData.SlotB memory)
    {
        return (state.slotA, state.slotB);
    }

    /// @inheritdoc EssentialContract
    function unpause() public override {
        super.unpause(); // permission checked inside
        state.slotB.lastUnpausedAt = uint64(block.timestamp);
    }

    /// @inheritdoc ITaikoL1
    // Surge: Stage-2 requirement
    function getVerificationStreakStartAt() external view returns(uint256) {
        if(state.blocks[state.slotB.lastVerifiedBlockId].proposedAt > getConfig().maxLivenessDisruptionPeriod) {
            return block.timestamp;
        } else {
            return state.verificationStreakStartedAt;
        }
    }

    /// @inheritdoc ITaikoL1
    // Surge: switch to `view` to allow for dynamic chainid
    function getConfig() public view virtual returns (TaikoData.Config memory) {
        return TaikoData.Config({
            chainId: LibNetwork.TAIKO_MAINNET,
            blockMaxProposals: 324_000, // = 7200 * 45
            blockRingBufferSize: 360_000, // = 7200 * 50
            maxBlocksToVerify: 16,
            blockMaxGasLimit: 240_000_000,
            livenessBond: 0.0007 ether,
            stateRootSyncInternal: 1,
            maxAnchorHeightOffset: 64,
            baseFeeConfig: LibSharedData.BaseFeeConfig({
                adjustmentQuotient: 8,
                sharingPctg: 75,
                gasIssuancePerSecond: 5_000_000,
                minGasExcess: 1_340_000_000,
                maxGasIssuancePerBlock: 600_000_000 // two minutes
             }),
            ontakeForkHeight: 1,
            // Surge: Default value
            maxLivenessDisruptionPeriod: 7 days
        });
    }

    /// @dev chain_pauser is supposed to be a cold wallet.
    function _authorizePause(
        address,
        bool
    )
        internal
        view
        virtual
        override
        onlyFromOwnerOrNamed(LibStrings.B_CHAIN_WATCHDOG)
    { }
}
