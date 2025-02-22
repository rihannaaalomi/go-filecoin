package mining

// The Worker Mines on Input received from a Scheduler.  The Worker is
// responsible for generating the necessary proofs, checking for success,
// generating new blocks, and forwarding them out to the wider node.

import (
	"context"
	"time"

	"github.com/ipfs/go-cid"
	"github.com/ipfs/go-hamt-ipld"
	"github.com/ipfs/go-ipfs-blockstore"
	logging "github.com/ipfs/go-log"
	"github.com/pkg/errors"

	"github.com/filecoin-project/go-filecoin/address"
	"github.com/filecoin-project/go-filecoin/consensus"
	"github.com/filecoin-project/go-filecoin/state"
	"github.com/filecoin-project/go-filecoin/types"
	"github.com/filecoin-project/go-filecoin/vm"
)

var log = logging.Logger("mining")

// Output is the result of a single mining run. It has either a new
// block or an error, mimicing the golang (retVal, error) pattern.
// If a mining run's context is canceled there is no output.
type Output struct {
	NewBlock *types.Block
	Err      error
}

// NewOutput instantiates a new Output.
func NewOutput(b *types.Block, e error) Output {
	return Output{NewBlock: b, Err: e}
}

// Worker is the interface called by the Scheduler to run the mining work being
// scheduled.
type Worker interface {
	Mine(runCtx context.Context, base types.TipSet, nullBlkCount int, outCh chan<- Output) bool
}

// GetStateTree is a function that gets the aggregate state tree of a TipSet. It's
// its own function to facilitate testing.
type GetStateTree func(context.Context, types.TipSet) (state.Tree, error)

// GetWeight is a function that calculates the weight of a TipSet.  Weight is
// expressed as two uint64s comprising a rational number.
type GetWeight func(context.Context, types.TipSet) (uint64, error)

// GetAncestors is a function that returns the necessary ancestor chain to
// process the input tipset.
type GetAncestors func(context.Context, types.TipSet, *types.BlockHeight) ([]types.TipSet, error)

// MessageSource provides message candidates for mining into blocks
type MessageSource interface {
	// Pending returns a slice of un-mined messages.
	Pending() []*types.SignedMessage
	// Remove removes a message from the source permanently
	Remove(message cid.Cid)
}

// A MessageApplier processes all the messages in a message pool.
type MessageApplier interface {
	// ApplyMessagesAndPayRewards applies all state transitions related to a set of messages.
	ApplyMessagesAndPayRewards(ctx context.Context, st state.Tree, vms vm.StorageMap, messages []*types.SignedMessage, minerOwnerAddr address.Address, bh *types.BlockHeight, ancestors []types.TipSet) (consensus.ApplyMessagesResponse, error)
}

type workerPorcelainAPI interface {
	BlockTime() time.Duration
}

// DefaultWorker runs a mining job.
type DefaultWorker struct {
	api workerPorcelainAPI

	createPoSTFunc DoSomeWorkFunc
	minerAddr      address.Address
	minerOwnerAddr address.Address
	minerPubKey    []byte
	workerSigner   consensus.TicketSigner

	// consensus things
	getStateTree GetStateTree
	getWeight    GetWeight
	getAncestors GetAncestors

	// core filecoin things
	messageSource MessageSource
	processor     MessageApplier
	powerTable    consensus.PowerTableView
	blockstore    blockstore.Blockstore
	cstore        *hamt.CborIpldStore
}

// NewDefaultWorker instantiates a new Worker.
func NewDefaultWorker(messageSource MessageSource,
	getStateTree GetStateTree,
	getWeight GetWeight,
	getAncestors GetAncestors,
	processor MessageApplier,
	powerTable consensus.PowerTableView,
	bs blockstore.Blockstore,
	cst *hamt.CborIpldStore,
	miner address.Address,
	minerOwner address.Address,
	minerPubKey []byte,
	workerSigner consensus.TicketSigner,
	api workerPorcelainAPI) *DefaultWorker {

	w := NewDefaultWorkerWithDeps(messageSource,
		getStateTree,
		getWeight,
		getAncestors,
		processor,
		powerTable,
		bs,
		cst,
		miner,
		minerOwner,
		minerPubKey,
		workerSigner,
		api,
		func() {})

	// TODO: create real PoST.
	// https://github.com/filecoin-project/go-filecoin/issues/1791
	w.createPoSTFunc = w.fakeCreatePoST

	return w
}

// NewDefaultWorkerWithDeps instantiates a new Worker with custom functions.
func NewDefaultWorkerWithDeps(messageSource MessageSource,
	getStateTree GetStateTree,
	getWeight GetWeight,
	getAncestors GetAncestors,
	processor MessageApplier,
	powerTable consensus.PowerTableView,
	bs blockstore.Blockstore,
	cst *hamt.CborIpldStore,
	miner address.Address,
	minerOwner address.Address,
	minerPubKey []byte,
	workerSigner consensus.TicketSigner,
	api workerPorcelainAPI,
	createPoST DoSomeWorkFunc) *DefaultWorker {
	return &DefaultWorker{
		api:            api,
		getStateTree:   getStateTree,
		getWeight:      getWeight,
		getAncestors:   getAncestors,
		messageSource:  messageSource,
		processor:      processor,
		powerTable:     powerTable,
		blockstore:     bs,
		cstore:         cst,
		createPoSTFunc: createPoST,
		minerAddr:      miner,
		minerOwnerAddr: minerOwner,
		minerPubKey:    minerPubKey,
		workerSigner:   workerSigner,
	}
}

// DoSomeWorkFunc is a dummy function that mimics doing something time-consuming
// in the mining loop such as computing proofs. Pass a function that calls Sleep()
// is a good idea for now.
type DoSomeWorkFunc func()

// Mine implements the DefaultWorkers main mining function..
// The returned bool indicates if this miner created a new block or not.
func (w *DefaultWorker) Mine(ctx context.Context, base types.TipSet, nullBlkCount int, outCh chan<- Output) bool {
	log.Info("Worker.Mine")
	ctx = log.Start(ctx, "Worker.Mine")
	defer log.Finish(ctx)
	if !base.Defined() {
		log.Warning("Worker.Mine returning because it can't mine on an empty tipset")
		outCh <- Output{Err: errors.New("bad input tipset with no blocks sent to Mine()")}
		return false
	}

	st, err := w.getStateTree(ctx, base)
	if err != nil {
		log.Errorf("Worker.Mine couldn't get state tree for tipset: %s", err.Error())
		outCh <- Output{Err: err}
		return false
	}

	log.Debugf("Mining on tipset: %s, with %d null blocks.", base.String(), nullBlkCount)
	if ctx.Err() != nil {
		log.Warningf("Worker.Mine returning with ctx error %s", ctx.Err().Error())
		return false
	}

	challenge, err := consensus.CreateChallengeSeed(base, uint64(nullBlkCount))
	if err != nil {
		outCh <- Output{Err: err}
		return false
	}
	prCh := createProof(challenge, w.createPoSTFunc)

	var proof types.PoStProof
	var ticket []byte
	select {
	case <-ctx.Done():
		log.Infof("Mining run on base %s with %d null blocks canceled.", base.String(), nullBlkCount)
		return false
	case prChRead, more := <-prCh:
		if !more {
			log.Errorf("Worker.Mine got zero value from channel prChRead")
			return false
		}
		proof := append(types.PoStProof{}, prChRead[:]...)
		ticket, err = consensus.CreateTicket(proof, w.minerPubKey, w.workerSigner)
		if err != nil {
			log.Errorf("failed to create ticket: %s", err)
			return false
		}
	}

	// TODO: Test the interplay of isWinningTicket() and createPoSTFunc()
	// https://github.com/filecoin-project/go-filecoin/issues/1791
	weHaveAWinner, err := consensus.IsWinningTicket(ctx, w.blockstore, w.powerTable, st, ticket, w.minerAddr)

	if err != nil {
		log.Errorf("Worker.Mine couldn't compute ticket: %s", err.Error())
		outCh <- Output{Err: err}
		return false
	}

	if weHaveAWinner {
		next, err := w.Generate(ctx, base, ticket, proof, uint64(nullBlkCount))
		if err == nil {
			log.SetTag(ctx, "block", next)
			log.Debugf("Worker.Mine generates new winning block! %s", next.Cid().String())
		}
		outCh <- NewOutput(next, err)
		return true
	}

	return false
}

// TODO: Actually use the results of the PoST once it is implemented.
// Currently createProof just passes the challenge seed through.
func createProof(challengeSeed types.PoStChallengeSeed, createPoST DoSomeWorkFunc) <-chan types.PoStChallengeSeed {
	c := make(chan types.PoStChallengeSeed)
	go func() {
		// TODO send new PoST on channel once we can create it
		//  https://github.com/filecoin-project/go-filecoin/issues/1791
		createPoST()
		c <- challengeSeed
	}()
	return c
}

// fakeCreatePoST is the default implementation of DoSomeWorkFunc.
// It simply sleeps for the blockTime.
func (w *DefaultWorker) fakeCreatePoST() {
	time.Sleep(w.api.BlockTime())
}
