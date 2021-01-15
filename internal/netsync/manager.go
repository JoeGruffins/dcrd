// Copyright (c) 2013-2016 The btcsuite developers
// Copyright (c) 2015-2021 The Decred developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package netsync

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
	"time"

	"github.com/decred/dcrd/blockchain/stake/v4"
	"github.com/decred/dcrd/blockchain/v4"
	"github.com/decred/dcrd/chaincfg/chainhash"
	"github.com/decred/dcrd/chaincfg/v3"
	"github.com/decred/dcrd/database/v2"
	"github.com/decred/dcrd/dcrutil/v4"
	"github.com/decred/dcrd/internal/mempool"
	"github.com/decred/dcrd/internal/progresslog"
	"github.com/decred/dcrd/internal/rpcserver"
	"github.com/decred/dcrd/lru"
	peerpkg "github.com/decred/dcrd/peer/v2"
	"github.com/decred/dcrd/wire"
)

const (
	// minInFlightBlocks is the minimum number of blocks that should be
	// in the request queue for headers-first mode before requesting
	// more.
	minInFlightBlocks = 10

	// maxOrphanBlocks is the maximum number of orphan blocks that can be
	// queued.
	maxOrphanBlocks = 500

	// maxRejectedTxns is the maximum number of rejected transactions
	// hashes to store in memory.
	maxRejectedTxns = 1000

	// maxRequestedBlocks is the maximum number of requested block
	// hashes to store in memory.
	maxRequestedBlocks = wire.MaxInvPerMsg

	// maxRequestedTxns is the maximum number of requested transactions
	// hashes to store in memory.
	maxRequestedTxns = wire.MaxInvPerMsg
)

// zeroHash is the zero value hash (all zeros).  It is defined as a convenience.
var zeroHash chainhash.Hash

// newPeerMsg signifies a newly connected peer to the event handler.
type newPeerMsg struct {
	peer *peerpkg.Peer
}

// blockMsg packages a Decred block message and the peer it came from together
// so the event handler has access to that information.
type blockMsg struct {
	block *dcrutil.Block
	peer  *peerpkg.Peer
	reply chan struct{}
}

// invMsg packages a Decred inv message and the peer it came from together
// so the event handler has access to that information.
type invMsg struct {
	inv  *wire.MsgInv
	peer *peerpkg.Peer
}

// headersMsg packages a Decred headers message and the peer it came from
// together so the event handler has access to that information.
type headersMsg struct {
	headers *wire.MsgHeaders
	peer    *peerpkg.Peer
}

// notFoundMsg packages a Decred notfound message and the peer it came from
// together so the event handler has access to that information.
type notFoundMsg struct {
	notFound *wire.MsgNotFound
	peer     *peerpkg.Peer
}

// donePeerMsg signifies a newly disconnected peer to the event handler.
type donePeerMsg struct {
	peer *peerpkg.Peer
}

// txMsg packages a Decred tx message and the peer it came from together
// so the event handler has access to that information.
type txMsg struct {
	tx    *dcrutil.Tx
	peer  *peerpkg.Peer
	reply chan struct{}
}

// getSyncPeerMsg is a message type to be sent across the message channel for
// retrieving the current sync peer.
type getSyncPeerMsg struct {
	reply chan int32
}

// requestFromPeerMsg is a message type to be sent across the message channel
// for requesting either blocks or transactions from a given peer. It routes
// this through the sync manager so the sync manager doesn't ban the peer
// when it sends this information back.
type requestFromPeerMsg struct {
	peer         *peerpkg.Peer
	blocks       []*chainhash.Hash
	voteHashes   []*chainhash.Hash
	tSpendHashes []*chainhash.Hash
	reply        chan requestFromPeerResponse
}

// requestFromPeerResponse is a response sent to the reply channel of a
// requestFromPeerMsg query.
type requestFromPeerResponse struct {
	err error
}

// processBlockResponse is a response sent to the reply channel of a
// processBlockMsg.
type processBlockResponse struct {
	forkLen  int64
	isOrphan bool
	err      error
}

// processBlockMsg is a message type to be sent across the message channel
// for requested a block is processed.  Note this call differs from blockMsg
// above in that blockMsg is intended for blocks that came from peers and have
// extra handling whereas this message essentially is just a concurrent safe
// way to call ProcessBlock on the internal block chain instance.
type processBlockMsg struct {
	block *dcrutil.Block
	flags blockchain.BehaviorFlags
	reply chan processBlockResponse
}

// processTransactionResponse is a response sent to the reply channel of a
// processTransactionMsg.
type processTransactionResponse struct {
	acceptedTxs []*dcrutil.Tx
	err         error
}

// processTransactionMsg is a message type to be sent across the message
// channel for requesting a transaction to be processed through the block
// manager.
type processTransactionMsg struct {
	tx            *dcrutil.Tx
	allowOrphans  bool
	rateLimit     bool
	allowHighFees bool
	tag           mempool.Tag
	reply         chan processTransactionResponse
}

// headerNode is used as a node in a list of headers that are linked together
// between checkpoints.
type headerNode struct {
	height int64
	hash   *chainhash.Hash
}

// syncMgrPeer extends a peer to maintain additional state maintained by the
// sync manager.
type syncMgrPeer struct {
	*peerpkg.Peer

	syncCandidate   bool
	requestedTxns   map[chainhash.Hash]struct{}
	requestedBlocks map[chainhash.Hash]struct{}
}

// orphanBlock represents a block for which the parent is not yet available.  It
// is a normal block plus an expiration time to prevent caching the orphan
// forever.
type orphanBlock struct {
	block      *dcrutil.Block
	expiration time.Time
}

// SyncManager provides a concurrency safe sync manager for handling all
// incoming blocks.
type SyncManager struct {
	// The following fields are used for lifecycle management of the sync
	// manager.
	wg   sync.WaitGroup
	quit chan struct{}

	// cfg specifies the configuration of the sync manager and is set at
	// creation time and treated as immutable after that.
	cfg Config

	rejectedTxns    map[chainhash.Hash]struct{}
	requestedTxns   map[chainhash.Hash]struct{}
	requestedBlocks map[chainhash.Hash]struct{}
	progressLogger  *progresslog.BlockLogger
	syncPeer        *syncMgrPeer
	msgChan         chan interface{}
	peers           map[*peerpkg.Peer]*syncMgrPeer

	// The following fields are used for headers-first mode.
	headersFirstMode bool
	headerList       *list.List
	startHeader      *list.Element
	nextCheckpoint   *chaincfg.Checkpoint

	// These fields are related to handling of orphan blocks.  They are
	// protected by the orphan lock.
	orphanLock   sync.RWMutex
	orphans      map[chainhash.Hash]*orphanBlock
	prevOrphans  map[chainhash.Hash][]*orphanBlock
	oldestOrphan *orphanBlock

	// The following fields are used to track the height being synced to from
	// peers.
	syncHeightMtx sync.Mutex
	syncHeight    int64

	// The following fields are used to track whether or not the manager
	// believes it is fully synced to the network.
	isCurrentMtx sync.Mutex
	isCurrent    bool
}

// lookupPeer returns the sync manager peer that maintains additional state for
// a given base peer.  In the event the mapping does not exist, a warning is
// logged and nil is returned.
func lookupPeer(peer *peerpkg.Peer, peers map[*peerpkg.Peer]*syncMgrPeer) *syncMgrPeer {
	sp, ok := peers[peer]
	if !ok {
		log.Warnf("Attempt to lookup unknown peer %s\nStake: %v", peer,
			string(debug.Stack()))
		return nil
	}
	return sp
}

// resetHeaderState sets the headers-first mode state to values appropriate for
// syncing from a new peer.
func (m *SyncManager) resetHeaderState(newestHash *chainhash.Hash, newestHeight int64) {
	m.headersFirstMode = false
	m.headerList.Init()
	m.startHeader = nil

	// When there is a next checkpoint, add an entry for the latest known
	// block into the header pool.  This allows the next downloaded header
	// to prove it links to the chain properly.
	if m.nextCheckpoint != nil {
		node := headerNode{height: newestHeight, hash: newestHash}
		m.headerList.PushBack(&node)
	}
}

// SyncHeight returns latest known block being synced to.
func (m *SyncManager) SyncHeight() int64 {
	m.syncHeightMtx.Lock()
	defer m.syncHeightMtx.Unlock()
	return m.syncHeight
}

// findNextHeaderCheckpoint returns the next checkpoint after the passed height.
// It returns nil when there is not one either because the height is already
// later than the final checkpoint or some other reason such as disabled
// checkpoints.
func (m *SyncManager) findNextHeaderCheckpoint(height int64) *chaincfg.Checkpoint {
	checkpoints := m.cfg.Chain.Checkpoints()
	if len(checkpoints) == 0 {
		return nil
	}

	// There is no next checkpoint if the height is already after the final
	// checkpoint.
	finalCheckpoint := &checkpoints[len(checkpoints)-1]
	if height >= finalCheckpoint.Height {
		return nil
	}

	// Find the next checkpoint.
	nextCheckpoint := finalCheckpoint
	for i := len(checkpoints) - 2; i >= 0; i-- {
		if height >= checkpoints[i].Height {
			break
		}
		nextCheckpoint = &checkpoints[i]
	}
	return nextCheckpoint
}

// chainBlockLocatorToHashes converts a block locator from chain to a slice
// of hashes.
func chainBlockLocatorToHashes(locator blockchain.BlockLocator) []chainhash.Hash {
	if len(locator) == 0 {
		return nil
	}

	result := make([]chainhash.Hash, 0, len(locator))
	for _, hash := range locator {
		result = append(result, *hash)
	}
	return result
}

// startSync will choose the best peer among the available candidate peers to
// download/sync the blockchain from.  When syncing is already running, it
// simply returns.  It also examines the candidates for any which are no longer
// candidates and removes them as needed.
func (m *SyncManager) startSync() {
	// Return now if we're already syncing.
	if m.syncPeer != nil {
		return
	}

	best := m.cfg.Chain.BestSnapshot()
	var bestPeer *syncMgrPeer
	for _, peer := range m.peers {
		if !peer.syncCandidate {
			continue
		}

		// Remove sync candidate peers that are no longer candidates due
		// to passing their latest known block.  NOTE: The < is
		// intentional as opposed to <=.  While technically the peer
		// doesn't have a later block when it's equal, it will likely
		// have one soon so it is a reasonable choice.  It also allows
		// the case where both are at 0 such as during regression test.
		if peer.LastBlock() < best.Height {
			peer.syncCandidate = false
			continue
		}

		// the best sync candidate is the most updated peer
		if bestPeer == nil {
			bestPeer = peer
		}
		if bestPeer.LastBlock() < peer.LastBlock() {
			bestPeer = peer
		}
	}

	// Update the state of whether or not the manager believes the chain is
	// fully synced to whatever the chain believes when there is no candidate
	// for a sync peer.
	if bestPeer == nil {
		m.isCurrentMtx.Lock()
		m.isCurrent = m.cfg.Chain.IsCurrent()
		m.isCurrentMtx.Unlock()
	}

	// Start syncing from the best peer if one was selected.
	if bestPeer != nil {
		// Clear the requestedBlocks if the sync peer changes, otherwise
		// we may ignore blocks we need that the last sync peer failed
		// to send.
		m.requestedBlocks = make(map[chainhash.Hash]struct{})

		blkLocator := m.cfg.Chain.LatestBlockLocator()
		locator := chainBlockLocatorToHashes(blkLocator)

		log.Infof("Syncing to block height %d from peer %v",
			bestPeer.LastBlock(), bestPeer.Addr())

		// The chain is not synced whenever the current best height is less than
		// the height to sync to.
		if best.Height < bestPeer.LastBlock() {
			m.isCurrentMtx.Lock()
			m.isCurrent = false
			m.isCurrentMtx.Unlock()
		}

		// When the current height is less than a known checkpoint we
		// can use block headers to learn about which blocks comprise
		// the chain up to the checkpoint and perform less validation
		// for them.  This is possible since each header contains the
		// hash of the previous header and a merkle root.  Therefore if
		// we validate all of the received headers link together
		// properly and the checkpoint hashes match, we can be sure the
		// hashes for the blocks in between are accurate.  Further, once
		// the full blocks are downloaded, the merkle root is computed
		// and compared against the value in the header which proves the
		// full block hasn't been tampered with.
		//
		// Once we have passed the final checkpoint, or checkpoints are
		// disabled, use standard inv messages learn about the blocks
		// and fully validate them.  Finally, regression test mode does
		// not support the headers-first approach so do normal block
		// downloads when in regression test mode.
		if m.nextCheckpoint != nil &&
			best.Height < m.nextCheckpoint.Height &&
			!m.cfg.DisableCheckpoints {

			err := bestPeer.PushGetHeadersMsg(locator, m.nextCheckpoint.Hash)
			if err != nil {
				log.Errorf("Failed to push getheadermsg for the latest "+
					"blocks: %v", err)
				return
			}
			m.headersFirstMode = true
			log.Infof("Downloading headers for blocks %d to %d from peer %s",
				best.Height+1, m.nextCheckpoint.Height, bestPeer.Addr())
		} else {
			err := bestPeer.PushGetBlocksMsg(locator, &zeroHash)
			if err != nil {
				log.Errorf("Failed to push getblocksmsg for the latest "+
					"blocks: %v", err)
				return
			}
		}
		m.syncPeer = bestPeer
		m.syncHeightMtx.Lock()
		m.syncHeight = bestPeer.LastBlock()
		m.syncHeightMtx.Unlock()
	} else {
		log.Warnf("No sync peer candidates available")
	}
}

// isSyncCandidate returns whether or not the peer is a candidate to consider
// syncing from.
func (m *SyncManager) isSyncCandidate(peer *peerpkg.Peer) bool {
	// The peer is not a candidate for sync if it's not a full node.
	return peer.Services()&wire.SFNodeNetwork == wire.SFNodeNetwork
}

// syncMiningStateAfterSync polls the sync manager for the current sync state
// and once the manager believes the chain is fully synced, it executes a call
// to the peer to sync the mining state.
func (m *SyncManager) syncMiningStateAfterSync(peer *peerpkg.Peer) {
	go func() {
		for {
			select {
			case <-time.After(3 * time.Second):
			case <-m.quit:
				return
			}

			if !peer.Connected() {
				return
			}
			if !m.IsCurrent() {
				continue
			}

			pver := peer.ProtocolVersion()
			var msg wire.Message

			switch {
			case pver < wire.InitStateVersion:
				// Before protocol version InitStateVersion
				// nodes use the GetMiningState/MiningState
				// pair of p2p messages.
				msg = wire.NewMsgGetMiningState()

			default:
				// On the most recent protocol version, nodes
				// use the GetInitState/InitState pair of p2p
				// messages.
				m := wire.NewMsgGetInitState()
				err := m.AddTypes(wire.InitStateHeadBlocks,
					wire.InitStateHeadBlockVotes,
					wire.InitStateTSpends)
				if err != nil {
					log.Errorf("Unexpected error while building getinitstate "+
						"msg: %v", err)
					return
				}
				msg = m
			}

			peer.QueueMessage(msg, nil)
			return
		}
	}()
}

// handleNewPeerMsg deals with new peers that have signalled they may
// be considered as a sync peer (they have already successfully negotiated).  It
// also starts syncing if needed.  It is invoked from the syncHandler goroutine.
func (m *SyncManager) handleNewPeerMsg(ctx context.Context, peer *peerpkg.Peer) {
	select {
	case <-ctx.Done():
	default:
	}

	log.Infof("New valid peer %s (%s)", peer, peer.UserAgent())

	// Initialize the peer state
	isSyncCandidate := m.isSyncCandidate(peer)
	m.peers[peer] = &syncMgrPeer{
		Peer:            peer,
		syncCandidate:   isSyncCandidate,
		requestedTxns:   make(map[chainhash.Hash]struct{}),
		requestedBlocks: make(map[chainhash.Hash]struct{}),
	}

	// Start syncing by choosing the best candidate if needed.
	if isSyncCandidate && m.syncPeer == nil {
		m.startSync()
	}

	// Grab the mining state from this peer once synced when enabled.
	if !m.cfg.NoMiningStateSync {
		m.syncMiningStateAfterSync(peer)
	}
}

// handleDonePeerMsg deals with peers that have signalled they are done.  It
// removes the peer as a candidate for syncing and in the case where it was
// the current sync peer, attempts to select a new best peer to sync from.  It
// is invoked from the syncHandler goroutine.
func (m *SyncManager) handleDonePeerMsg(p *peerpkg.Peer) {
	peer := lookupPeer(p, m.peers)
	if peer == nil {
		return
	}

	// Remove the peer from the list of candidate peers.
	delete(m.peers, p)

	// Remove requested transactions from the global map so that they will
	// be fetched from elsewhere next time we get an inv.
	for txHash := range peer.requestedTxns {
		delete(m.requestedTxns, txHash)
	}

	// Remove requested blocks from the global map so that they will be
	// fetched from elsewhere next time we get an inv.
	// TODO(oga) we could possibly here check which peers have these blocks
	// and request them now to speed things up a little.
	for blockHash := range peer.requestedBlocks {
		delete(m.requestedBlocks, blockHash)
	}

	// Attempt to find a new peer to sync from if the quitting peer is the
	// sync peer.  Also, reset the headers-first state if in headers-first
	// mode so
	if m.syncPeer == peer {
		m.syncPeer = nil
		if m.headersFirstMode {
			best := m.cfg.Chain.BestSnapshot()
			m.resetHeaderState(&best.Hash, best.Height)
		}
		m.startSync()
	}
}

// errToWireRejectCode determines the wire rejection code and description for a
// given error. This function can convert some select blockchain and mempool
// error types to the historical rejection codes used on the p2p wire protocol.
func errToWireRejectCode(err error) (wire.RejectCode, string) {
	// The default reason to reject a transaction/block is due to it being
	// invalid somehow.
	code := wire.RejectInvalid
	var reason string

	// Convert recognized errors to a reject code.
	switch {
	// Rejected due to duplicate.
	case errors.Is(err, blockchain.ErrDuplicateBlock):
		code = wire.RejectDuplicate
		reason = err.Error()

	// Rejected due to obsolete version.
	case errors.Is(err, blockchain.ErrBlockVersionTooOld):
		code = wire.RejectObsolete
		reason = err.Error()

	// Rejected due to checkpoint.
	case errors.Is(err, blockchain.ErrCheckpointTimeTooOld),
		errors.Is(err, blockchain.ErrDifficultyTooLow),
		errors.Is(err, blockchain.ErrBadCheckpoint),
		errors.Is(err, blockchain.ErrForkTooOld):
		code = wire.RejectCheckpoint
		reason = err.Error()

	// Error codes which map to a duplicate transaction already
	// mined or in the mempool.
	case errors.Is(err, mempool.ErrMempoolDoubleSpend),
		errors.Is(err, mempool.ErrAlreadyVoted),
		errors.Is(err, mempool.ErrDuplicate),
		errors.Is(err, mempool.ErrTooManyVotes),
		errors.Is(err, mempool.ErrDuplicateRevocation),
		errors.Is(err, mempool.ErrAlreadyExists),
		errors.Is(err, mempool.ErrOrphan):
		code = wire.RejectDuplicate
		reason = err.Error()

	// Error codes which map to a non-standard transaction being
	// relayed.
	case errors.Is(err, mempool.ErrOrphanPolicyViolation),
		errors.Is(err, mempool.ErrOldVote),
		errors.Is(err, mempool.ErrSeqLockUnmet),
		errors.Is(err, mempool.ErrNonStandard):
		code = wire.RejectNonstandard
		reason = err.Error()

	// Error codes which map to an insufficient fee being paid.
	case errors.Is(err, mempool.ErrInsufficientFee),
		errors.Is(err, mempool.ErrInsufficientPriority):
		code = wire.RejectInsufficientFee
		reason = err.Error()

	// Error codes which map to an attempt to create dust outputs.
	case errors.Is(err, mempool.ErrDustOutput):
		code = wire.RejectDust
		reason = err.Error()

	default:
		reason = fmt.Sprintf("rejected: %v", err)
	}
	return code, reason
}

// handleTxMsg handles transaction messages from all peers.
func (m *SyncManager) handleTxMsg(tmsg *txMsg) {
	peer := lookupPeer(tmsg.peer, m.peers)
	if peer == nil {
		return
	}

	// NOTE:  BitcoinJ, and possibly other wallets, don't follow the spec of
	// sending an inventory message and allowing the remote peer to decide
	// whether or not they want to request the transaction via a getdata
	// message.  Unfortunately, the reference implementation permits
	// unrequested data, so it has allowed wallets that don't follow the
	// spec to proliferate.  While this is not ideal, there is no check here
	// to disconnect peers for sending unsolicited transactions to provide
	// interoperability.
	txHash := tmsg.tx.Hash()

	// Ignore transactions that we have already rejected.  Do not
	// send a reject message here because if the transaction was already
	// rejected, the transaction was unsolicited.
	if _, exists := m.rejectedTxns[*txHash]; exists {
		log.Debugf("Ignoring unsolicited previously rejected transaction %v "+
			"from %s", txHash, peer)
		return
	}

	// Process the transaction to include validation, insertion in the
	// memory pool, orphan handling, etc.
	allowOrphans := m.cfg.MaxOrphanTxs > 0
	acceptedTxs, err := m.cfg.TxMemPool.ProcessTransaction(tmsg.tx,
		allowOrphans, true, true, mempool.Tag(peer.ID()))

	// Remove transaction from request maps. Either the mempool/chain
	// already knows about it and as such we shouldn't have any more
	// instances of trying to fetch it, or we failed to insert and thus
	// we'll retry next time we get an inv.
	delete(peer.requestedTxns, *txHash)
	delete(m.requestedTxns, *txHash)

	if err != nil {
		// Do not request this transaction again until a new block
		// has been processed.
		limitAdd(m.rejectedTxns, *txHash, maxRejectedTxns)

		// When the error is a rule error, it means the transaction was
		// simply rejected as opposed to something actually going wrong,
		// so log it as such.  Otherwise, something really did go wrong,
		// so log it as an actual error.
		var rErr mempool.RuleError
		if errors.As(err, &rErr) {
			log.Debugf("Rejected transaction %v from %s: %v", txHash, peer, err)
		} else {
			log.Errorf("Failed to process transaction %v: %v", txHash, err)
		}

		// Convert the error into an appropriate reject message and
		// send it.
		code, reason := errToWireRejectCode(err)
		peer.PushRejectMsg(wire.CmdTx, code, reason, txHash, false)
		return
	}

	m.cfg.PeerNotifier.AnnounceNewTransactions(acceptedTxs)
}

// isKnownOrphan returns whether the passed hash is currently a known orphan.
// Keep in mind that only a limited number of orphans are held onto for a
// limited amount of time, so this function must not be used as an absolute way
// to test if a block is an orphan block.  A full block (as opposed to just its
// hash) must be passed to ProcessBlock for that purpose.  This function
// provides a mechanism for a caller to intelligently detect *recent* duplicate
// orphans and react accordingly.
//
// This function is safe for concurrent access.
func (m *SyncManager) isKnownOrphan(hash *chainhash.Hash) bool {
	// Protect concurrent access.  Using a read lock only so multiple readers
	// can query without blocking each other.
	m.orphanLock.RLock()
	_, exists := m.orphans[*hash]
	m.orphanLock.RUnlock()
	return exists
}

// orphanRoot returns the head of the chain for the provided hash from the map
// of orphan blocks.
//
// This function is safe for concurrent access.
func (m *SyncManager) orphanRoot(hash *chainhash.Hash) *chainhash.Hash {
	// Protect concurrent access.  Using a read lock only so multiple
	// readers can query without blocking each other.
	m.orphanLock.RLock()
	defer m.orphanLock.RUnlock()

	// Keep looping while the parent of each orphaned block is known and is an
	// orphan itself.
	orphanRoot := hash
	prevHash := hash
	for {
		orphan, exists := m.orphans[*prevHash]
		if !exists {
			break
		}
		orphanRoot = prevHash
		prevHash = &orphan.block.MsgBlock().Header.PrevBlock
	}

	return orphanRoot
}

// removeOrphanBlock removes the passed orphan block from the orphan pool and
// previous orphan index.
func (m *SyncManager) removeOrphanBlock(orphan *orphanBlock) {
	// Protect concurrent access.
	m.orphanLock.Lock()
	defer m.orphanLock.Unlock()

	// Remove the orphan block from the orphan pool.
	orphanHash := orphan.block.Hash()
	delete(m.orphans, *orphanHash)

	// Remove the reference from the previous orphan index too.  An indexing
	// for loop is intentionally used over a range here as range does not
	// reevaluate the slice on each iteration nor does it adjust the index
	// for the modified slice.
	prevHash := &orphan.block.MsgBlock().Header.PrevBlock
	orphans := m.prevOrphans[*prevHash]
	for i := 0; i < len(orphans); i++ {
		hash := orphans[i].block.Hash()
		if hash.IsEqual(orphanHash) {
			copy(orphans[i:], orphans[i+1:])
			orphans[len(orphans)-1] = nil
			orphans = orphans[:len(orphans)-1]
			i--
		}
	}
	m.prevOrphans[*prevHash] = orphans

	// Remove the map entry altogether if there are no longer any orphans
	// which depend on the parent hash.
	if len(m.prevOrphans[*prevHash]) == 0 {
		delete(m.prevOrphans, *prevHash)
	}
}

// addOrphanBlock adds the passed block (which is already determined to be an
// orphan prior calling this function) to the orphan pool.  It lazily cleans up
// any expired blocks so a separate cleanup poller doesn't need to be run.  It
// also imposes a maximum limit on the number of outstanding orphan blocks and
// will remove the oldest received orphan block if the limit is exceeded.
func (m *SyncManager) addOrphanBlock(block *dcrutil.Block) {
	// Remove expired orphan blocks.
	for _, oBlock := range m.orphans {
		if time.Now().After(oBlock.expiration) {
			m.removeOrphanBlock(oBlock)
			continue
		}

		// Update the oldest orphan block pointer so it can be discarded
		// in case the orphan pool fills up.
		if m.oldestOrphan == nil ||
			oBlock.expiration.Before(m.oldestOrphan.expiration) {
			m.oldestOrphan = oBlock
		}
	}

	// Limit orphan blocks to prevent memory exhaustion.
	if len(m.orphans)+1 > maxOrphanBlocks {
		// Remove the oldest orphan to make room for the new one.
		m.removeOrphanBlock(m.oldestOrphan)
		m.oldestOrphan = nil
	}

	// Protect concurrent access.  This is intentionally done here instead
	// of near the top since removeOrphanBlock does its own locking and
	// the range iterator is not invalidated by removing map entries.
	m.orphanLock.Lock()
	defer m.orphanLock.Unlock()

	// Insert the block into the orphan map with an expiration time
	// 1 hour from now.
	expiration := time.Now().Add(time.Hour)
	oBlock := &orphanBlock{
		block:      block,
		expiration: expiration,
	}
	m.orphans[*block.Hash()] = oBlock

	// Add to previous hash lookup index for faster dependency lookups.
	prevHash := &block.MsgBlock().Header.PrevBlock
	m.prevOrphans[*prevHash] = append(m.prevOrphans[*prevHash], oBlock)
}

// maybeUpdateIsCurrent potentially updates the manager to signal it believes the
// chain is considered synced.
//
// This function MUST be called with the is current mutex held (for writes).
func (m *SyncManager) maybeUpdateIsCurrent() {
	// Nothing to do when already considered synced.
	if m.isCurrent {
		return
	}

	// The chain is considered synced once both the blockchain believes it is
	// current and the sync height is reached or exceeded.
	best := m.cfg.Chain.BestSnapshot()
	syncHeight := m.SyncHeight()
	if best.Height >= syncHeight && m.cfg.Chain.IsCurrent() {
		m.isCurrent = true
	}
}

// processOrphans determines if there are any orphans which depend on the passed
// block hash (they are no longer orphans if true) and potentially accepts them.
// It repeats the process for the newly accepted blocks (to detect further
// orphans which may no longer be orphans) until there are no more.
//
// The flags do not modify the behavior of this function directly, however they
// are needed to pass along to maybeAcceptBlock.
func (m *SyncManager) processOrphans(hash *chainhash.Hash, flags blockchain.BehaviorFlags) error {
	// Start with processing at least the passed hash.  Leave a little room for
	// additional orphan blocks that need to be processed without needing to
	// grow the array in the common case.
	processHashes := make([]*chainhash.Hash, 0, 10)
	processHashes = append(processHashes, hash)
	for len(processHashes) > 0 {
		// Pop the first hash to process from the slice.
		processHash := processHashes[0]
		processHashes[0] = nil // Prevent GC leak.
		processHashes = processHashes[1:]

		// Look up all orphans that are parented by the block we just accepted.
		// This will typically only be one, but it could be multiple if multiple
		// blocks are mined and broadcast around the same time.  The one with
		// the most proof of work will eventually win out.  An indexing for loop
		// is intentionally used over a range here as range does not reevaluate
		// the slice on each iteration nor does it adjust the index for the
		// modified slice.
		for i := 0; i < len(m.prevOrphans[*processHash]); i++ {
			orphan := m.prevOrphans[*processHash][i]
			if orphan == nil {
				log.Warnf("Found a nil entry at index %d in the orphan "+
					"dependency list for block %v", i, processHash)
				continue
			}

			// Remove the orphan from the orphan pool.
			orphanHash := orphan.block.Hash()
			m.removeOrphanBlock(orphan)
			i--

			// Potentially accept the block into the block chain.
			_, err := m.cfg.Chain.ProcessBlock(orphan.block, flags)
			if err != nil {
				return err
			}
			m.isCurrentMtx.Lock()
			m.maybeUpdateIsCurrent()
			m.isCurrentMtx.Unlock()

			// Add this block to the list of blocks to process so any orphan
			// blocks that depend on this block are handled too.
			processHashes = append(processHashes, orphanHash)
		}
	}
	return nil
}

// processBlockAndOrphans processes the provided block using the internal chain
// instance while keeping track of orphan blocks and also processing any orphans
// that depend on the passed block to potentially accept as well.
//
// When no errors occurred during processing, the first return value indicates
// the length of the fork the block extended.  In the case it either extended
// the best chain or is now the tip of the best chain due to causing a
// reorganize, the fork length will be 0.  The second return value indicates
// whether or not the block is an orphan, in which case the fork length will
// also be zero as expected, because it, by definition, does not connect to the
// best chain.
func (m *SyncManager) processBlockAndOrphans(block *dcrutil.Block, flags blockchain.BehaviorFlags) (int64, bool, error) {
	// Process the block to include validation, best chain selection, etc.
	//
	// Also, keep track of orphan blocks in the sync manager when the error
	// returned indicates the block is an orphan.
	blockHash := block.Hash()
	forkLen, err := m.cfg.Chain.ProcessBlock(block, flags)
	if errors.Is(err, blockchain.ErrMissingParent) {
		log.Infof("Adding orphan block %v with parent %v", blockHash,
			block.MsgBlock().Header.PrevBlock)
		m.addOrphanBlock(block)

		// The fork length of orphans is unknown since they, by definition, do
		// not connect to the best chain.
		return 0, true, nil
	}
	if err != nil {
		return 0, false, err
	}
	m.isCurrentMtx.Lock()
	m.maybeUpdateIsCurrent()
	m.isCurrentMtx.Unlock()

	// Accept any orphan blocks that depend on this block (they are no longer
	// orphans) and repeat for those accepted blocks until there are no more.
	if err := m.processOrphans(blockHash, flags); err != nil {
		return 0, false, err
	}

	return forkLen, false, nil
}

// handleBlockMsg handles block messages from all peers.
func (m *SyncManager) handleBlockMsg(bmsg *blockMsg) {
	peer := lookupPeer(bmsg.peer, m.peers)
	if peer == nil {
		return
	}

	// If we didn't ask for this block then the peer is misbehaving.
	blockHash := bmsg.block.Hash()
	if _, exists := peer.requestedBlocks[*blockHash]; !exists {
		log.Warnf("Got unrequested block %v from %s -- disconnecting",
			blockHash, peer.Addr())
		peer.Disconnect()
		return
	}

	// When in headers-first mode, if the block matches the hash of the
	// first header in the list of headers that are being fetched, it's
	// eligible for less validation since the headers have already been
	// verified to link together and are valid up to the next checkpoint.
	// Also, remove the list entry for all blocks except the checkpoint
	// since it is needed to verify the next round of headers links
	// properly.
	isCheckpointBlock := false
	behaviorFlags := blockchain.BFNone
	if m.headersFirstMode {
		firstNodeEl := m.headerList.Front()
		if firstNodeEl != nil {
			firstNode := firstNodeEl.Value.(*headerNode)
			if blockHash.IsEqual(firstNode.hash) {
				behaviorFlags |= blockchain.BFFastAdd
				if firstNode.hash.IsEqual(m.nextCheckpoint.Hash) {
					isCheckpointBlock = true
				} else {
					m.headerList.Remove(firstNodeEl)
				}
			}
		}
	}

	// Remove block from request maps. Either chain will know about it and
	// so we shouldn't have any more instances of trying to fetch it, or we
	// will fail the insert and thus we'll retry next time we get an inv.
	delete(peer.requestedBlocks, *blockHash)
	delete(m.requestedBlocks, *blockHash)

	// Process the block to include validation, best chain selection, orphan
	// handling, etc.
	forkLen, isOrphan, err := m.processBlockAndOrphans(bmsg.block, behaviorFlags)
	if err != nil {
		// When the error is a rule error, it means the block was simply
		// rejected as opposed to something actually going wrong, so log
		// it as such.  Otherwise, something really did go wrong, so log
		// it as an actual error.
		var rErr blockchain.RuleError
		if errors.As(err, &rErr) {
			log.Infof("Rejected block %v from %s: %v", blockHash, peer, err)
		} else {
			log.Errorf("Failed to process block %v: %v", blockHash, err)
		}
		if errors.Is(err, database.ErrCorruption) {
			log.Errorf("Critical failure: %v", err)
		}

		// Convert the error into an appropriate reject message and
		// send it.
		code, reason := errToWireRejectCode(err)
		peer.PushRejectMsg(wire.CmdBlock, code, reason, blockHash, false)
		return
	}

	// Request the parents for the orphan block from the peer that sent it.
	onMainChain := !isOrphan && forkLen == 0
	if isOrphan {
		orphanRoot := m.orphanRoot(blockHash)
		blkLocator := m.cfg.Chain.LatestBlockLocator()
		locator := chainBlockLocatorToHashes(blkLocator)
		err := peer.PushGetBlocksMsg(locator, orphanRoot)
		if err != nil {
			log.Warnf("Failed to push getblocksmsg for the latest block: %v",
				err)
		}
	} else {
		// When the block is not an orphan, log information about it and
		// update the chain state.
		m.progressLogger.LogBlockHeight(bmsg.block.MsgBlock(), m.SyncHeight())

		if onMainChain {
			// Notify stake difficulty subscribers and prune invalidated
			// transactions.
			best := m.cfg.Chain.BestSnapshot()
			m.cfg.TxMemPool.PruneStakeTx(best.NextStakeDiff, best.Height)
			m.cfg.TxMemPool.PruneExpiredTx()

			// Clear the rejected transactions.
			m.rejectedTxns = make(map[chainhash.Hash]struct{})
		}
	}

	// Update the latest block height for the peer to avoid stale heights when
	// looking for future potential sync node candidacy.
	//
	// Also, when the block is an orphan or the chain is considered current and
	// the block was accepted to the main chain, update the heights of other
	// peers whose invs may have been ignored when actively syncing while the
	// chain was not yet current or lost the lock announcement race.
	blockHeight := int64(bmsg.block.MsgBlock().Header.Height)
	peer.UpdateLastBlockHeight(blockHeight)
	if isOrphan || (onMainChain && m.IsCurrent()) {
		for _, p := range m.peers {
			// The height for the sending peer is already updated.
			if p == peer {
				continue
			}

			lastAnnBlock := p.LastAnnouncedBlock()
			if lastAnnBlock != nil && *lastAnnBlock == *blockHash {
				p.UpdateLastBlockHeight(blockHeight)
				p.UpdateLastAnnouncedBlock(nil)
			}
		}
	}

	// Nothing more to do if we aren't in headers-first mode.
	if !m.headersFirstMode {
		return
	}

	// This is headers-first mode, so if the block is not a checkpoint
	// request more blocks using the header list when the request queue is
	// getting short.
	if !isCheckpointBlock {
		if m.startHeader != nil &&
			len(peer.requestedBlocks) < minInFlightBlocks {
			m.fetchHeaderBlocks()
		}
		return
	}

	// This is headers-first mode and the block is a checkpoint.  When
	// there is a next checkpoint, get the next round of headers by asking
	// for headers starting from the block after this one up to the next
	// checkpoint.
	prevHeight := m.nextCheckpoint.Height
	prevHash := m.nextCheckpoint.Hash
	m.nextCheckpoint = m.findNextHeaderCheckpoint(prevHeight)
	if m.nextCheckpoint != nil {
		locator := []chainhash.Hash{*prevHash}
		err := peer.PushGetHeadersMsg(locator, m.nextCheckpoint.Hash)
		if err != nil {
			log.Warnf("Failed to send getheaders message to peer %s: %v",
				peer.Addr(), err)
			return
		}
		log.Infof("Downloading headers for blocks %d to %d from peer %s",
			prevHeight+1, m.nextCheckpoint.Height, m.syncPeer.Addr())
		return
	}

	// This is headers-first mode, the block is a checkpoint, and there are
	// no more checkpoints, so switch to normal mode by requesting blocks
	// from the block after this one up to the end of the chain (zero hash).
	m.headersFirstMode = false
	m.headerList.Init()
	log.Infof("Reached the final checkpoint -- switching to normal mode")
	locator := []chainhash.Hash{*blockHash}
	err = peer.PushGetBlocksMsg(locator, &zeroHash)
	if err != nil {
		log.Warnf("Failed to send getblocks message to peer %s: %v",
			peer.Addr(), err)
		return
	}
}

// fetchHeaderBlocks creates and sends a request to the syncPeer for the next
// list of blocks to be downloaded based on the current list of headers.
func (m *SyncManager) fetchHeaderBlocks() {
	// Nothing to do if there is no start header.
	if m.startHeader == nil {
		log.Warnf("fetchHeaderBlocks called with no start header")
		return
	}

	// Build up a getdata request for the list of blocks the headers
	// describe.  The size hint will be limited to wire.MaxInvPerMsg by
	// the function, so no need to double check it here.
	gdmsg := wire.NewMsgGetDataSizeHint(uint(m.headerList.Len()))
	numRequested := 0
	for e := m.startHeader; e != nil; e = e.Next() {
		node, ok := e.Value.(*headerNode)
		if !ok {
			log.Warn("Header list node type is not a headerNode")
			continue
		}

		if m.needBlock(node.hash) {
			iv := wire.NewInvVect(wire.InvTypeBlock, node.hash)
			m.requestedBlocks[*node.hash] = struct{}{}
			m.syncPeer.requestedBlocks[*node.hash] = struct{}{}
			err := gdmsg.AddInvVect(iv)
			if err != nil {
				log.Warnf("Failed to add invvect while fetching block "+
					"headers: %v", err)
			}
			numRequested++
		}
		m.startHeader = e.Next()
		if numRequested >= wire.MaxInvPerMsg {
			break
		}
	}
	if len(gdmsg.InvList) > 0 {
		m.syncPeer.QueueMessage(gdmsg, nil)
	}
}

// handleHeadersMsg handles headers messages from all peers.
func (m *SyncManager) handleHeadersMsg(hmsg *headersMsg) {
	peer := lookupPeer(hmsg.peer, m.peers)
	if peer == nil {
		return
	}

	// The remote peer is misbehaving if we didn't request headers.
	msg := hmsg.headers
	numHeaders := len(msg.Headers)
	if !m.headersFirstMode {
		log.Warnf("Got %d unrequested headers from %s -- disconnecting",
			numHeaders, peer.Addr())
		peer.Disconnect()
		return
	}

	// Nothing to do for an empty headers message.
	if numHeaders == 0 {
		return
	}

	// Process all of the received headers ensuring each one connects to the
	// previous and that checkpoints match.
	receivedCheckpoint := false
	var finalHash *chainhash.Hash
	for _, blockHeader := range msg.Headers {
		blockHash := blockHeader.BlockHash()
		finalHash = &blockHash

		// Ensure there is a previous header to compare against.
		prevNodeEl := m.headerList.Back()
		if prevNodeEl == nil {
			log.Warnf("Header list does not contain a previous element as " +
				"expected -- disconnecting peer")
			peer.Disconnect()
			return
		}

		// Ensure the header properly connects to the previous one and
		// add it to the list of headers.
		node := headerNode{hash: &blockHash}
		prevNode := prevNodeEl.Value.(*headerNode)
		if prevNode.hash.IsEqual(&blockHeader.PrevBlock) {
			node.height = prevNode.height + 1
			e := m.headerList.PushBack(&node)
			if m.startHeader == nil {
				m.startHeader = e
			}
		} else {
			log.Warnf("Received block header that does not properly connect "+
				"to the chain from peer %s -- disconnecting", peer.Addr())
			peer.Disconnect()
			return
		}

		// Verify the header at the next checkpoint height matches.
		if node.height == m.nextCheckpoint.Height {
			if node.hash.IsEqual(m.nextCheckpoint.Hash) {
				receivedCheckpoint = true
				log.Infof("Verified downloaded block header against "+
					"checkpoint at height %d/hash %s", node.height, node.hash)
			} else {
				log.Warnf("Block header at height %d/hash %s from peer %s "+
					"does NOT match expected checkpoint hash of %s -- "+
					"disconnecting", node.height, node.hash, peer.Addr(),
					m.nextCheckpoint.Hash)
				peer.Disconnect()
				return
			}
			break
		}
	}

	// When this header is a checkpoint, switch to fetching the blocks for
	// all of the headers since the last checkpoint.
	if receivedCheckpoint {
		// Since the first entry of the list is always the final block
		// that is already in the database and is only used to ensure
		// the next header links properly, it must be removed before
		// fetching the blocks.
		m.headerList.Remove(m.headerList.Front())
		log.Infof("Received %v block headers: Fetching blocks",
			m.headerList.Len())
		m.progressLogger.SetLastLogTime(time.Now())
		m.fetchHeaderBlocks()
		return
	}

	// This header is not a checkpoint, so request the next batch of
	// headers starting from the latest known header and ending with the
	// next checkpoint.
	locator := []chainhash.Hash{*finalHash}
	err := peer.PushGetHeadersMsg(locator, m.nextCheckpoint.Hash)
	if err != nil {
		log.Warnf("Failed to send getheaders message to peer %s: %v",
			peer.Addr(), err)
		return
	}
}

// handleNotFoundMsg handles notfound messages from all peers.
func (m *SyncManager) handleNotFoundMsg(nfmsg *notFoundMsg) {
	peer := lookupPeer(nfmsg.peer, m.peers)
	if peer == nil {
		return
	}

	for _, inv := range nfmsg.notFound.InvList {
		// verify the hash was actually announced by the peer
		// before deleting from the global requested maps.
		switch inv.Type {
		case wire.InvTypeBlock:
			if _, exists := peer.requestedBlocks[inv.Hash]; exists {
				delete(peer.requestedBlocks, inv.Hash)
				delete(m.requestedBlocks, inv.Hash)
			}
		case wire.InvTypeTx:
			if _, exists := peer.requestedTxns[inv.Hash]; exists {
				delete(peer.requestedTxns, inv.Hash)
				delete(m.requestedTxns, inv.Hash)
			}
		}
	}
}

// needBlock returns whether or not the block needs to be downloaded.  For
// example, it does not need to be downloaded when it is already known.
func (m *SyncManager) needBlock(hash *chainhash.Hash) bool {
	return !m.isKnownOrphan(hash) && !m.cfg.Chain.HaveBlock(hash)
}

// needTx returns whether or not the transaction needs to be downloaded.  For
// example, it does not need to be downloaded when it is already known.
func (m *SyncManager) needTx(hash *chainhash.Hash) bool {
	// No need for transactions that have already been rejected.
	if _, exists := m.rejectedTxns[*hash]; exists {
		return false
	}

	// No need for transactions that are already available in the transaction
	// memory pool (main pool or orphan).
	if m.cfg.TxMemPool.HaveTransaction(hash) {
		return false
	}

	// No need for transactions that were recently confirmed.
	if m.cfg.RecentlyConfirmedTxns.Contains(*hash) {
		return false
	}

	return false
}

// handleInvMsg handles inv messages from all peers.
// We examine the inventory advertised by the remote peer and act accordingly.
func (m *SyncManager) handleInvMsg(imsg *invMsg) {
	peer := lookupPeer(imsg.peer, m.peers)
	if peer == nil {
		return
	}
	fromSyncPeer := peer == m.syncPeer
	isCurrent := m.IsCurrent()

	// Update state information regarding per-peer known inventory and determine
	// what inventory to request based on factors such as the current sync state
	// and whether or not the data is already available.
	//
	// Also, keep track of the final announced block (when there is one) so the
	// peer can be updated with that information accordingly.
	var lastBlock *wire.InvVect
	var requestQueue []*wire.InvVect
	for _, iv := range imsg.inv.InvList {
		switch iv.Type {
		case wire.InvTypeBlock:
			// Add the block to the cache of known inventory for the peer.  This
			// helps avoid sending blocks to the peer that it is already known
			// to have.
			peer.AddKnownInventory(iv)

			// Update the last block in the announced inventory.
			lastBlock = iv

			// Ignore block announcements for blocks that are from peers other
			// than the sync peer before the chain is current or are otherwise
			// not needed, such as when they're already available.
			//
			// This helps prevent fetching a mass of orphans.
			if (!fromSyncPeer && !isCurrent) || !m.needBlock(&iv.Hash) {
				continue
			}

			// Request the block if there is not one already pending.
			if _, exists := m.requestedBlocks[iv.Hash]; !exists {
				limitAdd(m.requestedBlocks, iv.Hash, maxRequestedBlocks)
				limitAdd(peer.requestedBlocks, iv.Hash, maxRequestedBlocks)
				requestQueue = append(requestQueue, iv)
			}

		case wire.InvTypeTx:
			// Add the tx to the cache of known inventory for the peer.  This
			// helps avoid sending transactions to the peer that it is already
			// known to have.
			peer.AddKnownInventory(iv)

			// Ignore transaction announcements before the chain is current or
			// are otherwise not needed, such as when they were recently
			// rejected or are already known.
			//
			// Transaction announcements are based on the state of the fully
			// synced ledger, so they are likely to be invalid before the chain
			// is current.
			if !isCurrent || !m.needTx(&iv.Hash) {
				continue
			}

			// Request the transaction if there is not one already pending.
			if _, exists := m.requestedTxns[iv.Hash]; !exists {
				limitAdd(m.requestedTxns, iv.Hash, maxRequestedTxns)
				limitAdd(peer.requestedTxns, iv.Hash, maxRequestedTxns)
				requestQueue = append(requestQueue, iv)
			}
		}
	}

	if lastBlock != nil {
		// When the block is an orphan that we already have, the missing parent
		// blocks were requested when the orphan was processed.  In that case,
		// there were more blocks missing than are allowed into a single
		// inventory message.  As a result, once this peer requested the final
		// advertised block, the remote peer noticed and is now resending the
		// orphan block as an available block to signal there are more missing
		// blocks that need to be requested.
		if m.isKnownOrphan(&lastBlock.Hash) {
			// Request blocks starting at the latest known up to the root of the
			// orphan that just came in.
			orphanRoot := m.orphanRoot(&lastBlock.Hash)
			blkLocator := m.cfg.Chain.LatestBlockLocator()
			locator := chainBlockLocatorToHashes(blkLocator)
			err := peer.PushGetBlocksMsg(locator, orphanRoot)
			if err != nil {
				log.Errorf("Failed to push getblocksmsg for orphan chain: %v",
					err)
			}
		}

		// Update the last announced block to the final one in the announced
		// inventory above (if any).  In the case the header for that block is
		// already known, use that information to update the height for the peer
		// too.
		peer.UpdateLastAnnouncedBlock(&lastBlock.Hash)
		if isCurrent {
			header, err := m.cfg.Chain.HeaderByHash(&lastBlock.Hash)
			if err == nil {
				peer.UpdateLastBlockHeight(int64(header.Height))
			}
		}

		// Request blocks after the last one advertised up to the final one the
		// remote peer knows about (zero stop hash).
		blkLocator := m.cfg.Chain.BlockLocatorFromHash(&lastBlock.Hash)
		locator := chainBlockLocatorToHashes(blkLocator)
		err := peer.PushGetBlocksMsg(locator, &zeroHash)
		if err != nil {
			log.Errorf("Failed to push getblocksmsg: %v", err)
		}
	}

	// Request as much as possible at once.
	var numRequested int32
	gdmsg := wire.NewMsgGetData()
	for _, iv := range requestQueue {
		gdmsg.AddInvVect(iv)
		numRequested++
		if numRequested == wire.MaxInvPerMsg {
			// Send full getdata message and reset.
			//
			// NOTE: There should never be more than wire.MaxInvPerMsg in the
			// inv request, so this could return after the QueueMessage, but
			// this is safer.
			peer.QueueMessage(gdmsg, nil)
			gdmsg = wire.NewMsgGetData()
			numRequested = 0
		}
	}

	if len(gdmsg.InvList) > 0 {
		peer.QueueMessage(gdmsg, nil)
	}
}

// limitAdd is a helper function for maps that require a maximum limit by
// evicting a random value if adding the new value would cause it to
// overflow the maximum allowed.
func limitAdd(m map[chainhash.Hash]struct{}, hash chainhash.Hash, limit int) {
	if len(m)+1 > limit {
		// Remove a random entry from the map.  For most compilers, Go's
		// range statement iterates starting at a random item although
		// that is not 100% guaranteed by the spec.  The iteration order
		// is not important here because an adversary would have to be
		// able to pull off preimage attacks on the hashing function in
		// order to target eviction of specific entries anyways.
		for txHash := range m {
			delete(m, txHash)
			break
		}
	}
	m[hash] = struct{}{}
}

// eventHandler is the main handler for the sync manager.  It must be run as a
// goroutine.  It processes block and inv messages in a separate goroutine from
// the peer handlers so the block (MsgBlock) messages are handled by a single
// thread without needing to lock memory data structures.  This is important
// because the sync manager controls which blocks are needed and how the
// fetching should proceed.
func (m *SyncManager) eventHandler(ctx context.Context) {
out:
	for {
		select {
		case data := <-m.msgChan:
			switch msg := data.(type) {
			case *newPeerMsg:
				m.handleNewPeerMsg(ctx, msg.peer)

			case *txMsg:
				m.handleTxMsg(msg)
				select {
				case msg.reply <- struct{}{}:
				case <-ctx.Done():
				}

			case *blockMsg:
				m.handleBlockMsg(msg)
				select {
				case msg.reply <- struct{}{}:
				case <-ctx.Done():
				}

			case *invMsg:
				m.handleInvMsg(msg)

			case *headersMsg:
				m.handleHeadersMsg(msg)

			case *notFoundMsg:
				m.handleNotFoundMsg(msg)

			case *donePeerMsg:
				m.handleDonePeerMsg(msg.peer)

			case getSyncPeerMsg:
				var peerID int32
				if m.syncPeer != nil {
					peerID = m.syncPeer.ID()
				}
				msg.reply <- peerID

			case requestFromPeerMsg:
				err := m.requestFromPeer(msg.peer, msg.blocks, msg.voteHashes,
					msg.tSpendHashes)
				msg.reply <- requestFromPeerResponse{
					err: err,
				}

			case processBlockMsg:
				forkLen, isOrphan, err := m.processBlockAndOrphans(msg.block,
					msg.flags)
				if err != nil {
					msg.reply <- processBlockResponse{
						forkLen:  forkLen,
						isOrphan: isOrphan,
						err:      err,
					}
					continue
				}

				onMainChain := !isOrphan && forkLen == 0
				if onMainChain {
					// Prune invalidated transactions.
					best := m.cfg.Chain.BestSnapshot()
					m.cfg.TxMemPool.PruneStakeTx(best.NextStakeDiff,
						best.Height)
					m.cfg.TxMemPool.PruneExpiredTx()
				}

				msg.reply <- processBlockResponse{
					isOrphan: isOrphan,
					err:      nil,
				}

			case processTransactionMsg:
				acceptedTxs, err := m.cfg.TxMemPool.ProcessTransaction(msg.tx,
					msg.allowOrphans, msg.rateLimit, msg.allowHighFees, msg.tag)
				msg.reply <- processTransactionResponse{
					acceptedTxs: acceptedTxs,
					err:         err,
				}

			default:
				log.Warnf("Invalid message type in event handler: %T", msg)
			}

		case <-ctx.Done():
			break out
		}
	}

	m.wg.Done()
	log.Trace("Sync manager event handler done")
}

// NewPeer informs the sync manager of a newly active peer.
func (m *SyncManager) NewPeer(peer *peerpkg.Peer) {
	select {
	case m.msgChan <- &newPeerMsg{peer: peer}:
	case <-m.quit:
	}
}

// QueueTx adds the passed transaction message and peer to the event handling
// queue.
func (m *SyncManager) QueueTx(tx *dcrutil.Tx, peer *peerpkg.Peer, done chan struct{}) {
	select {
	case m.msgChan <- &txMsg{tx: tx, peer: peer, reply: done}:
	case <-m.quit:
		done <- struct{}{}
	}
}

// QueueBlock adds the passed block message and peer to the event handling
// queue.
func (m *SyncManager) QueueBlock(block *dcrutil.Block, peer *peerpkg.Peer, done chan struct{}) {
	select {
	case m.msgChan <- &blockMsg{block: block, peer: peer, reply: done}:
	case <-m.quit:
		done <- struct{}{}
	}
}

// QueueInv adds the passed inv message and peer to the event handling queue.
func (m *SyncManager) QueueInv(inv *wire.MsgInv, peer *peerpkg.Peer) {
	select {
	case m.msgChan <- &invMsg{inv: inv, peer: peer}:
	case <-m.quit:
	}
}

// QueueHeaders adds the passed headers message and peer to the event handling
// queue.
func (m *SyncManager) QueueHeaders(headers *wire.MsgHeaders, peer *peerpkg.Peer) {
	select {
	case m.msgChan <- &headersMsg{headers: headers, peer: peer}:
	case <-m.quit:
	}
}

// QueueNotFound adds the passed notfound message and peer to the event handling
// queue.
func (m *SyncManager) QueueNotFound(notFound *wire.MsgNotFound, peer *peerpkg.Peer) {
	select {
	case m.msgChan <- &notFoundMsg{notFound: notFound, peer: peer}:
	case <-m.quit:
	}
}

// DonePeer informs the sync manager that a peer has disconnected.
func (m *SyncManager) DonePeer(peer *peerpkg.Peer) {
	select {
	case m.msgChan <- &donePeerMsg{peer: peer}:
	case <-m.quit:
	}
}

// SyncPeerID returns the ID of the current sync peer, or 0 if there is none.
func (m *SyncManager) SyncPeerID() int32 {
	reply := make(chan int32, 1)
	select {
	case m.msgChan <- getSyncPeerMsg{reply: reply}:
	case <-m.quit:
	}

	select {
	case peerID := <-reply:
		return peerID
	case <-m.quit:
		return 0
	}
}

// RequestFromPeer allows an outside caller to request blocks or transactions
// from a peer.  The requests are logged in the internal map of requests so the
// peer is not later banned for sending the respective data.
func (m *SyncManager) RequestFromPeer(p *peerpkg.Peer, blocks, voteHashes,
	tSpendHashes []*chainhash.Hash) error {

	reply := make(chan requestFromPeerResponse, 1)
	select {
	case m.msgChan <- requestFromPeerMsg{peer: p, blocks: blocks,
		voteHashes: voteHashes, tSpendHashes: tSpendHashes, reply: reply}:
	case <-m.quit:
	}

	select {
	case response := <-reply:
		return response.err
	case <-m.quit:
		return fmt.Errorf("sync manager stopped")
	}
}

func (m *SyncManager) requestFromPeer(p *peerpkg.Peer, blocks, voteHashes,
	tSpendHashes []*chainhash.Hash) error {

	peer := lookupPeer(p, m.peers)
	if peer == nil {
		return fmt.Errorf("unknown peer %s", p)
	}

	// Add the blocks to the request.
	msgResp := wire.NewMsgGetData()
	for _, bh := range blocks {
		// If we've already requested this block, skip it.
		_, alreadyReqP := peer.requestedBlocks[*bh]
		_, alreadyReqB := m.requestedBlocks[*bh]

		if alreadyReqP || alreadyReqB {
			continue
		}

		// Skip the block when it is already known.
		if m.isKnownOrphan(bh) || m.cfg.Chain.HaveBlock(bh) {
			continue
		}

		err := msgResp.AddInvVect(wire.NewInvVect(wire.InvTypeBlock, bh))
		if err != nil {
			return fmt.Errorf("unexpected error encountered building request "+
				"for mining state block %v: %v",
				bh, err.Error())
		}

		peer.requestedBlocks[*bh] = struct{}{}
		m.requestedBlocks[*bh] = struct{}{}
	}

	addTxsToRequest := func(txs []*chainhash.Hash, txType stake.TxType) error {
		// Return immediately if txs is nil.
		if txs == nil {
			return nil
		}

		for _, tx := range txs {
			// If we've already requested this transaction, skip it.
			_, alreadyReqP := peer.requestedTxns[*tx]
			_, alreadyReqB := m.requestedTxns[*tx]

			if alreadyReqP || alreadyReqB {
				continue
			}

			// Ask the transaction memory pool if the transaction is known
			// to it in any form (main pool or orphan).
			if m.cfg.TxMemPool.HaveTransaction(tx) {
				continue
			}

			// Check if the transaction exists from the point of view of the main
			// chain tip.  Note that this is only a best effort since it is expensive
			// to check existence of every output and the only purpose of this check
			// is to avoid requesting already known transactions.
			//
			// Check for a specific outpoint based on the tx type.
			outpoint := wire.OutPoint{Hash: *tx}
			switch txType {
			case stake.TxTypeSSGen:
				// The first two outputs of vote transactions are OP_RETURN <data>, and
				// therefore never exist as an unspent txo.  Use the third output, as
				// the third output (and subsequent outputs) are OP_SSGEN outputs.
				outpoint.Index = 2
				outpoint.Tree = wire.TxTreeStake
			case stake.TxTypeTSpend:
				// The first output of a tSpend transaction is OP_RETURN <data>, and
				// therefore never exists as an unspent txo.  Use the second output, as
				// the second output (and subsequent outputs) are OP_TGEN outputs.
				outpoint.Index = 1
				outpoint.Tree = wire.TxTreeStake
			}
			entry, err := m.cfg.Chain.FetchUtxoEntry(outpoint)
			if err != nil {
				return err
			}
			if entry != nil {
				continue
			}

			err = msgResp.AddInvVect(wire.NewInvVect(wire.InvTypeTx, tx))
			if err != nil {
				return fmt.Errorf("unexpected error encountered building request "+
					"for mining state vote %v: %v",
					tx, err.Error())
			}

			peer.requestedTxns[*tx] = struct{}{}
			m.requestedTxns[*tx] = struct{}{}
		}

		return nil
	}

	// Add the vote transactions to the request.
	err := addTxsToRequest(voteHashes, stake.TxTypeSSGen)
	if err != nil {
		return err
	}

	// Add the tspend transactions to the request.
	err = addTxsToRequest(tSpendHashes, stake.TxTypeTSpend)
	if err != nil {
		return err
	}

	if len(msgResp.InvList) > 0 {
		p.QueueMessage(msgResp, nil)
	}

	return nil
}

// ProcessBlock makes use of ProcessBlock on an internal instance of a block
// chain.  It is funneled through the sync manager since blockchain is not safe
// for concurrent access.
func (m *SyncManager) ProcessBlock(block *dcrutil.Block, flags blockchain.BehaviorFlags) (bool, error) {
	reply := make(chan processBlockResponse, 1)
	select {
	case m.msgChan <- processBlockMsg{block: block, flags: flags, reply: reply}:
	case <-m.quit:
	}

	select {
	case response := <-reply:
		return response.isOrphan, response.err
	case <-m.quit:
		return false, fmt.Errorf("sync manager stopped")
	}
}

// ProcessTransaction makes use of ProcessTransaction on an internal instance of
// a block chain.  It is funneled through the sync manager since blockchain is
// not safe for concurrent access.
func (m *SyncManager) ProcessTransaction(tx *dcrutil.Tx, allowOrphans bool,
	rateLimit bool, allowHighFees bool, tag mempool.Tag) ([]*dcrutil.Tx, error) {

	reply := make(chan processTransactionResponse, 1)
	select {
	case m.msgChan <- processTransactionMsg{tx, allowOrphans, rateLimit,
		allowHighFees, tag, reply}:
	case <-m.quit:
	}

	select {
	case response := <-reply:
		return response.acceptedTxs, response.err
	case <-m.quit:
		return nil, fmt.Errorf("sync manager stopped")
	}
}

// IsCurrent returns whether or not the sync manager believes it is synced with
// the connected peers.
//
// This function is safe for concurrent access.
func (m *SyncManager) IsCurrent() bool {
	m.isCurrentMtx.Lock()
	m.maybeUpdateIsCurrent()
	isCurrent := m.isCurrent
	m.isCurrentMtx.Unlock()
	return isCurrent
}

// Run starts the sync manager and all other goroutines necessary for it to
// function properly and blocks until the provided context is cancelled.
func (m *SyncManager) Run(ctx context.Context) {
	log.Trace("Starting sync manager")

	// Start the event handler goroutine.
	m.wg.Add(1)
	go m.eventHandler(ctx)

	// Shutdown the sync manager when the context is cancelled.
	m.wg.Add(1)
	go func(ctx context.Context) {
		<-ctx.Done()
		close(m.quit)
		m.wg.Done()
	}(ctx)

	m.wg.Wait()
	log.Trace("Sync manager stopped")
}

// Config holds the configuration options related to the network chain
// synchronization manager.
type Config struct {
	// PeerNotifier specifies an implementation to use for notifying peers of
	// status changes related to blocks and transactions.
	PeerNotifier PeerNotifier

	// ChainParams identifies which chain parameters the manager is associated
	// with.
	ChainParams *chaincfg.Params

	// Chain specifies the chain instance to use for processing blocks and
	// transactions.
	Chain *blockchain.BlockChain

	// TxMemPool specifies the mempool to use for processing transactions.
	TxMemPool *mempool.TxPool

	// RpcServer returns an instance of an RPC server to use for notifications.
	// It may return nil if there is no active RPC server.
	RpcServer func() *rpcserver.Server

	// DisableCheckpoints indicates whether or not the sync manager should make
	// use of checkpoints.
	DisableCheckpoints bool

	// NoMiningStateSync indicates whether or not the sync manager should
	// perform an initial mining state synchronization with peers once they are
	// believed to be fully synced.
	NoMiningStateSync bool

	// MaxPeers specifies the maximum number of peers the server is expected to
	// be connected with.  It is primarily used as a hint for more efficient
	// synchronization.
	MaxPeers int

	// MaxOrphanTxs specifies the maximum number of orphan transactions the
	// transaction pool associated with the server supports.
	MaxOrphanTxs int

	// RecentlyConfirmedTxns specifies a size limited set to use for tracking
	// and querying the most recently confirmed transactions.  It is useful for
	// preventing duplicate requests.
	RecentlyConfirmedTxns *lru.Cache
}

// New returns a new network chain synchronization manager.  Use Run to begin
// processing asynchronous events.
func New(config *Config) *SyncManager {
	m := SyncManager{
		cfg:             *config,
		rejectedTxns:    make(map[chainhash.Hash]struct{}),
		requestedTxns:   make(map[chainhash.Hash]struct{}),
		requestedBlocks: make(map[chainhash.Hash]struct{}),
		peers:           make(map[*peerpkg.Peer]*syncMgrPeer),
		progressLogger:  progresslog.New("Processed", log),
		msgChan:         make(chan interface{}, config.MaxPeers*3),
		headerList:      list.New(),
		quit:            make(chan struct{}),
		orphans:         make(map[chainhash.Hash]*orphanBlock),
		prevOrphans:     make(map[chainhash.Hash][]*orphanBlock),
		isCurrent:       config.Chain.IsCurrent(),
	}

	best := m.cfg.Chain.BestSnapshot()
	if !m.cfg.DisableCheckpoints {
		// Initialize the next checkpoint based on the current height.
		m.nextCheckpoint = m.findNextHeaderCheckpoint(best.Height)
		if m.nextCheckpoint != nil {
			m.resetHeaderState(&best.Hash, best.Height)
		}
	} else {
		log.Info("Checkpoints are disabled")
	}

	m.syncHeightMtx.Lock()
	m.syncHeight = best.Height
	m.syncHeightMtx.Unlock()

	return &m
}