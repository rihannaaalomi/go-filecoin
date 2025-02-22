package miner

import (
	"bytes"
	"math/big"
	"strconv"

	"github.com/ipfs/go-cid"
	cbor "github.com/ipfs/go-ipld-cbor"
	"github.com/libp2p/go-libp2p-peer"
	xerrors "github.com/pkg/errors"

	"github.com/filecoin-project/go-filecoin/abi"
	"github.com/filecoin-project/go-filecoin/actor"
	"github.com/filecoin-project/go-filecoin/address"
	"github.com/filecoin-project/go-filecoin/exec"
	"github.com/filecoin-project/go-filecoin/proofs"
	"github.com/filecoin-project/go-filecoin/proofs/sectorbuilder"
	"github.com/filecoin-project/go-filecoin/types"
	"github.com/filecoin-project/go-filecoin/vm/errors"
)

func init() {
	cbor.RegisterCborType(State{})
	cbor.RegisterCborType(Ask{})
}

// MaximumPublicKeySize is a limit on how big a public key can be.
const MaximumPublicKeySize = 100

// LargestSectorSizeProvingPeriodBlocks defines the number of blocks in a
// proving period for a miner configured to use the largest sector size
// supported by the network.
//
// TODO: If the following PR is merged - and the network doesn't define a
// largest sector size - this constant and consensus.AncestorRoundsNeeded will
// need to be reconsidered.
// https://github.com/filecoin-project/specs/pull/318
const LargestSectorSizeProvingPeriodBlocks = 20000

// LargestSectorGenerationAttackThresholdBlocks defines the number of blocks
// after a proving period ends after which a miner using the largest sector size
// supported by the network is subject to storage fault slashing.
//
// TODO: If the following PR is merged - and the network doesn't define a
// largest sector size - this constant and consensus.AncestorRoundsNeeded will
// need to be reconsidered.
// https://github.com/filecoin-project/specs/pull/318
const LargestSectorGenerationAttackThresholdBlocks = 100

// MinimumCollateralPerSector is the minimum amount of collateral required per sector
var MinimumCollateralPerSector, _ = types.NewAttoFILFromFILString("0.001")

// ClientProofOfStorageTimeoutBlocks is the number of blocks between LastPoSt and the current block height
// after which the miner is no longer considered to be storing the client's piece and they are entitled to
// a refund.
// TODO: what is a fair value for this? Value is arbitrary right now.
// See https://github.com/filecoin-project/go-filecoin/issues/1887
const PieceInclusionGracePeriodBlocks = 10000

const (
	// ErrPublicKeyTooBig indicates an invalid public key.
	ErrPublicKeyTooBig = 33
	// ErrInvalidSector indicates and invalid sector id.
	ErrInvalidSector = 34
	// ErrSectorCommitted indicates the sector has already been committed.
	ErrSectorCommitted = 35
	// ErrStoragemarketCallFailed indicates the call to commit the deal failed.
	ErrStoragemarketCallFailed = 36
	// ErrCallerUnauthorized signals an unauthorized caller.
	ErrCallerUnauthorized = 37
	// ErrInsufficientPledge signals insufficient pledge for what you are trying to do.
	ErrInsufficientPledge = 38
	// ErrInvalidPoSt signals that the passed in PoSt was invalid.
	ErrInvalidPoSt = 39
	// ErrAskNotFound indicates that no ask was found with the given ID.
	ErrAskNotFound = 40
	// ErrInvalidSealProof signals that the passed in seal proof was invalid.
	ErrInvalidSealProof = 41
	// ErrGetProofsModeFailed indicates the call to get the proofs mode failed.
	ErrGetProofsModeFailed = 42
	// ErrInsufficientCollateral indicates that the miner does not have sufficient collateral to commit additional sectors.
	ErrInsufficientCollateral = 43
)

// Errors map error codes to revert errors this actor may return.
var Errors = map[uint8]error{
	ErrPublicKeyTooBig:         errors.NewCodedRevertErrorf(ErrPublicKeyTooBig, "public key must be less than %d bytes", MaximumPublicKeySize),
	ErrInvalidSector:           errors.NewCodedRevertErrorf(ErrInvalidSector, "sectorID out of range"),
	ErrSectorCommitted:         errors.NewCodedRevertErrorf(ErrSectorCommitted, "sector already committed"),
	ErrStoragemarketCallFailed: errors.NewCodedRevertErrorf(ErrStoragemarketCallFailed, "call to StorageMarket failed"),
	ErrCallerUnauthorized:      errors.NewCodedRevertErrorf(ErrCallerUnauthorized, "not authorized to call the method"),
	ErrInsufficientPledge:      errors.NewCodedRevertErrorf(ErrInsufficientPledge, "not enough pledged"),
	ErrInvalidPoSt:             errors.NewCodedRevertErrorf(ErrInvalidPoSt, "PoSt proof did not validate"),
	ErrAskNotFound:             errors.NewCodedRevertErrorf(ErrAskNotFound, "no ask was found"),
	ErrInvalidSealProof:        errors.NewCodedRevertErrorf(ErrInvalidSealProof, "seal proof was invalid"),
	ErrGetProofsModeFailed:     errors.NewCodedRevertErrorf(ErrGetProofsModeFailed, "failed to get proofs mode"),
	ErrInsufficientCollateral:  errors.NewCodedRevertErrorf(ErrInsufficientCollateral, "insufficient collateral"),
}

// Actor is the miner actor.
//
// If `Bootstrap` is `true`, the miner will not verify seal proofs. This is
// useful when testing, as miners with non-zero power can be created using bogus
// commitments. This is a temporary measure; we want to ultimately be able to
// create a real genesis block whose miners are seeded with real commitments.
//
// The `Bootstrap` field must be set to `true` if the miner was created in the
// genesis block. If the miner was created in any other block, `Bootstrap` must
// be false.
type Actor struct {
	Bootstrap bool
}

// Ask is a price advertisement by the miner
type Ask struct {
	Price  types.AttoFIL
	Expiry *types.BlockHeight
	ID     *big.Int
}

// State is the miner actors storage.
type State struct {
	Owner address.Address

	// PeerID references the libp2p identity that the miner is operating.
	PeerID peer.ID

	// PublicKey is used to validate blocks generated by the miner this actor represents.
	PublicKey []byte

	// ActiveCollateral is the amount of collateral currently committed to live
	// storage.
	ActiveCollateral types.AttoFIL

	// Asks is the set of asks this miner has open
	Asks      []*Ask
	NextAskID *big.Int

	// SectorCommitments maps sector id to commitments, for all sectors this
	// miner has committed. Due to a bug in refmt, the sector id-keys need to be
	// stringified.
	//
	// See also: https://github.com/polydawn/refmt/issues/35
	SectorCommitments map[string]types.Commitments

	LastUsedSectorID uint64

	ProvingPeriodStart *types.BlockHeight
	LastPoSt           *types.BlockHeight

	// The amount of space committed to the network by this miner.
	Power *types.BytesAmount

	// SectorSize is the amount of space in each sector committed to the network
	// by this miner.
	SectorSize *types.BytesAmount
}

// NewActor returns a new miner actor with the provided balance.
func NewActor() *actor.Actor {
	return actor.NewActor(types.MinerActorCodeCid, types.ZeroAttoFIL)
}

// NewState creates a miner state struct
func NewState(owner address.Address, key []byte, pid peer.ID, sectorSize *types.BytesAmount) *State {
	return &State{
		Owner:             owner,
		PeerID:            pid,
		PublicKey:         key,
		SectorCommitments: make(map[string]types.Commitments),
		Power:             types.NewBytesAmount(0),
		NextAskID:         big.NewInt(0),
		SectorSize:        sectorSize,
		ActiveCollateral:  types.ZeroAttoFIL,
	}
}

// InitializeState stores this miner's initial data structure.
func (ma *Actor) InitializeState(storage exec.Storage, initializerData interface{}) error {
	minerState, ok := initializerData.(*State)
	if !ok {
		return errors.NewFaultError("Initial state to miner actor is not a miner.State struct")
	}

	// TODO: we should validate this is actually a public key (possibly the owner's public key) once we have a better
	// TODO: idea what crypto looks like.
	if len(minerState.PublicKey) > MaximumPublicKeySize {
		return Errors[ErrPublicKeyTooBig]
	}

	stateBytes, err := cbor.DumpObject(minerState)
	if err != nil {
		return xerrors.Wrap(err, "failed to cbor marshal object")
	}

	id, err := storage.Put(stateBytes)
	if err != nil {
		return err
	}

	return storage.Commit(id, cid.Undef)
}

var _ exec.ExecutableActor = (*Actor)(nil)

var minerExports = exec.Exports{
	"addAsk": &exec.FunctionSignature{
		Params: []abi.Type{abi.AttoFIL, abi.Integer},
		Return: []abi.Type{abi.Integer},
	},
	"getAsks": &exec.FunctionSignature{
		Params: nil,
		Return: []abi.Type{abi.UintArray},
	},
	"getAsk": &exec.FunctionSignature{
		Params: []abi.Type{abi.Integer},
		Return: []abi.Type{abi.Bytes},
	},
	"getOwner": &exec.FunctionSignature{
		Params: nil,
		Return: []abi.Type{abi.Address},
	},
	"getLastUsedSectorID": &exec.FunctionSignature{
		Params: nil,
		Return: []abi.Type{abi.SectorID},
	},
	"commitSector": &exec.FunctionSignature{
		Params: []abi.Type{abi.SectorID, abi.Bytes, abi.Bytes, abi.Bytes, abi.PoRepProof},
		Return: []abi.Type{},
	},
	"getKey": &exec.FunctionSignature{
		Params: []abi.Type{},
		Return: []abi.Type{abi.Bytes},
	},
	"getPeerID": &exec.FunctionSignature{
		Params: []abi.Type{},
		Return: []abi.Type{abi.PeerID},
	},
	"updatePeerID": &exec.FunctionSignature{
		Params: []abi.Type{abi.PeerID},
		Return: []abi.Type{},
	},
	"getPower": &exec.FunctionSignature{
		Params: []abi.Type{},
		Return: []abi.Type{abi.BytesAmount},
	},
	"submitPoSt": &exec.FunctionSignature{
		Params: []abi.Type{abi.PoStProofs},
		Return: []abi.Type{},
	},
	"verifyPieceInclusion": &exec.FunctionSignature{
		Params: []abi.Type{abi.Bytes, abi.SectorID, abi.Bytes},
		Return: []abi.Type{},
	},
	"getProvingPeriodStart": &exec.FunctionSignature{
		Params: []abi.Type{},
		Return: []abi.Type{abi.BlockHeight},
	},
	"getSectorCommitments": &exec.FunctionSignature{
		Params: nil,
		Return: []abi.Type{abi.CommitmentsMap},
	},
	"isBootstrapMiner": &exec.FunctionSignature{
		Params: nil,
		Return: []abi.Type{abi.Boolean},
	},
	"getSectorSize": &exec.FunctionSignature{
		Params: nil,
		Return: []abi.Type{abi.BytesAmount},
	},
}

// Exports returns the miner actors exported functions.
func (ma *Actor) Exports() exec.Exports {
	return minerExports
}

//
// Exported actor methods
//

// AddAsk adds an ask to this miners ask list
func (ma *Actor) AddAsk(ctx exec.VMContext, price types.AttoFIL, expiry *big.Int) (*big.Int, uint8,
	error) {
	if err := ctx.Charge(actor.DefaultGasCost); err != nil {
		return nil, exec.ErrInsufficientGas, errors.RevertErrorWrap(err, "Insufficient gas")
	}

	var state State
	out, err := actor.WithState(ctx, &state, func() (interface{}, error) {
		if ctx.Message().From != state.Owner {
			return nil, Errors[ErrCallerUnauthorized]
		}

		id := big.NewInt(0).Set(state.NextAskID)
		state.NextAskID = state.NextAskID.Add(state.NextAskID, big.NewInt(1))

		// filter out expired asks
		asks := state.Asks
		state.Asks = state.Asks[:0]
		for _, a := range asks {
			if ctx.BlockHeight().LessThan(a.Expiry) {
				state.Asks = append(state.Asks, a)
			}
		}

		if !expiry.IsUint64() {
			return nil, errors.NewRevertError("expiry was invalid")
		}
		expiryBH := types.NewBlockHeight(expiry.Uint64())

		state.Asks = append(state.Asks, &Ask{
			Price:  price,
			Expiry: ctx.BlockHeight().Add(expiryBH),
			ID:     id,
		})

		return id, nil
	})
	if err != nil {
		return nil, errors.CodeError(err), err
	}

	askID, ok := out.(*big.Int)
	if !ok {
		return nil, 1, errors.NewRevertErrorf("expected an Integer return value from call, but got %T instead", out)
	}

	return askID, 0, nil
}

// GetAsks returns all the asks for this miner. (TODO: this isnt a great function signature, it returns the asks in a
// serialized array. Consider doing this some other way)
func (ma *Actor) GetAsks(ctx exec.VMContext) ([]uint64, uint8, error) {
	if err := ctx.Charge(actor.DefaultGasCost); err != nil {
		return nil, exec.ErrInsufficientGas, errors.RevertErrorWrap(err, "Insufficient gas")
	}
	var state State
	out, err := actor.WithState(ctx, &state, func() (interface{}, error) {
		var askids []uint64
		for _, ask := range state.Asks {
			if !ask.ID.IsUint64() {
				return nil, errors.NewFaultErrorf("miner ask has invalid ID (bad invariant)")
			}
			askids = append(askids, ask.ID.Uint64())
		}

		return askids, nil
	})
	if err != nil {
		return nil, errors.CodeError(err), err
	}

	askids, ok := out.([]uint64)
	if !ok {
		return nil, 1, errors.NewRevertErrorf("expected a []uint64 return value from call, but got %T instead", out)
	}

	return askids, 0, nil
}

// GetAsk returns an ask by ID
func (ma *Actor) GetAsk(ctx exec.VMContext, askid *big.Int) ([]byte, uint8, error) {
	if err := ctx.Charge(actor.DefaultGasCost); err != nil {
		return nil, exec.ErrInsufficientGas, errors.RevertErrorWrap(err, "Insufficient gas")
	}

	var state State
	out, err := actor.WithState(ctx, &state, func() (interface{}, error) {
		var ask *Ask
		for _, a := range state.Asks {
			if a.ID.Cmp(askid) == 0 {
				ask = a
				break
			}
		}

		if ask == nil {
			return nil, Errors[ErrAskNotFound]
		}

		out, err := cbor.DumpObject(ask)
		if err != nil {
			return nil, err
		}

		return out, nil
	})
	if err != nil {
		return nil, errors.CodeError(err), err
	}

	ask, ok := out.([]byte)
	if !ok {
		return nil, 1, errors.NewRevertErrorf("expected a Bytes return value from call, but got %T instead", out)
	}

	return ask, 0, nil
}

// GetOwner returns the miners owner.
func (ma *Actor) GetOwner(ctx exec.VMContext) (address.Address, uint8, error) {
	if err := ctx.Charge(actor.DefaultGasCost); err != nil {
		return address.Undef, exec.ErrInsufficientGas, errors.RevertErrorWrap(err, "Insufficient gas")
	}

	var state State
	out, err := actor.WithState(ctx, &state, func() (interface{}, error) {
		return state.Owner, nil
	})
	if err != nil {
		return address.Undef, errors.CodeError(err), err
	}

	a, ok := out.(address.Address)
	if !ok {
		return address.Undef, 1, errors.NewFaultErrorf("expected an Address return value from call, but got %T instead", out)
	}

	return a, 0, nil
}

// GetLastUsedSectorID returns the last used sector id.
func (ma *Actor) GetLastUsedSectorID(ctx exec.VMContext) (uint64, uint8, error) {
	if err := ctx.Charge(actor.DefaultGasCost); err != nil {
		return 0, exec.ErrInsufficientGas, errors.RevertErrorWrap(err, "Insufficient gas")
	}
	var state State
	out, err := actor.WithState(ctx, &state, func() (interface{}, error) {
		return state.LastUsedSectorID, nil
	})
	if err != nil {
		return 0, errors.CodeError(err), err
	}

	a, ok := out.(uint64)
	if !ok {
		return 0, 1, errors.NewFaultErrorf("expected a uint64 sector id, but got %T instead", out)
	}

	return a, 0, nil
}

// IsBootstrapMiner indicates whether the receiving miner was created in the
// genesis block, i.e. used to bootstrap the network
func (ma *Actor) IsBootstrapMiner(ctx exec.VMContext) (bool, uint8, error) {
	return ma.Bootstrap, 0, nil
}

// GetSectorCommitments returns all sector commitments posted by this miner.
func (ma *Actor) GetSectorCommitments(ctx exec.VMContext) (map[string]types.Commitments, uint8, error) {
	if err := ctx.Charge(actor.DefaultGasCost); err != nil {
		return nil, exec.ErrInsufficientGas, errors.RevertErrorWrap(err, "Insufficient gas")
	}

	var state State
	out, err := actor.WithState(ctx, &state, func() (interface{}, error) {
		return state.SectorCommitments, nil
	})
	if err != nil {
		return map[string]types.Commitments{}, errors.CodeError(err), err
	}

	a, ok := out.(map[string]types.Commitments)
	if !ok {
		return map[string]types.Commitments{}, 1, errors.NewFaultErrorf("expected a map[string]types.Commitments, but got %T instead", out)
	}

	return a, 0, nil
}

// GetSectorSize returns the size of the sectors committed to the network by
// this miner.
func (ma *Actor) GetSectorSize(ctx exec.VMContext) (*types.BytesAmount, uint8, error) {
	if err := ctx.Charge(actor.DefaultGasCost); err != nil {
		return nil, exec.ErrInsufficientGas, errors.RevertErrorWrap(err, "Insufficient gas")
	}

	var state State
	out, err := actor.WithState(ctx, &state, func() (interface{}, error) {
		return state.SectorSize, nil
	})
	if err != nil {
		return nil, errors.CodeError(err), err
	}

	amt, ok := out.(*types.BytesAmount)
	if !ok {
		return nil, 1, errors.NewFaultErrorf("expected a *types.BytesAmount, but got %T instead", out)
	}

	return amt, 0, nil
}

// CommitSector adds a commitment to the specified sector. The sector must not
// already be committed.
func (ma *Actor) CommitSector(ctx exec.VMContext, sectorID uint64, commD, commR, commRStar []byte, proof types.PoRepProof) (uint8, error) {
	if err := ctx.Charge(actor.DefaultGasCost); err != nil {
		return exec.ErrInsufficientGas, errors.RevertErrorWrap(err, "Insufficient gas")
	}
	if len(commD) != int(types.CommitmentBytesLen) {
		return 1, errors.NewRevertError("invalid sized commD")
	}
	if len(commR) != int(types.CommitmentBytesLen) {
		return 1, errors.NewRevertError("invalid sized commR")
	}
	if len(commRStar) != int(types.CommitmentBytesLen) {
		return 1, errors.NewRevertError("invalid sized commRStar")
	}

	var state State
	_, err := actor.WithState(ctx, &state, func() (interface{}, error) {
		// As with submitPoSt messages, bootstrap miner actors don't verify
		// the commitSector messages that they are sent.
		//
		// This switching will be removed when issue #2270 is completed.
		if !ma.Bootstrap {
			req := proofs.VerifySealRequest{}
			copy(req.CommD[:], commD)
			copy(req.CommR[:], commR)
			copy(req.CommRStar[:], commRStar)
			req.Proof = proof
			req.ProverID = sectorbuilder.AddressToProverID(ctx.Message().To)
			req.SectorID = sectorbuilder.SectorIDToBytes(sectorID)
			req.SectorSize = state.SectorSize

			res, err := (&proofs.RustVerifier{}).VerifySeal(req)
			if err != nil {
				return nil, errors.RevertErrorWrap(err, "failed to verify seal proof")
			}
			if !res.IsValid {
				return nil, Errors[ErrInvalidSealProof]
			}
		}

		// verify that the caller is authorized to perform update
		if ctx.Message().From != state.Owner {
			return nil, Errors[ErrCallerUnauthorized]
		}

		// TODO: use uint64 instead of this abomination, once refmt is fixed
		// https://github.com/polydawn/refmt/issues/35
		sectorIDstr := strconv.FormatUint(sectorID, 10)

		_, ok := state.SectorCommitments[sectorIDstr]
		if ok {
			return nil, Errors[ErrSectorCommitted]
		}

		// make sure the miner has enough collateral to add more storage
		collateral := CollateralForSector(state.SectorSize)
		if collateral.GreaterThan(ctx.MyBalance().Sub(state.ActiveCollateral)) {
			return nil, Errors[ErrInsufficientCollateral]
		}

		state.ActiveCollateral = state.ActiveCollateral.Add(collateral)

		if state.Power.Equal(types.NewBytesAmount(0)) {
			state.ProvingPeriodStart = ctx.BlockHeight()
		}
		inc := state.SectorSize
		state.Power = state.Power.Add(inc)
		comms := types.Commitments{
			CommD:     types.CommD{},
			CommR:     types.CommR{},
			CommRStar: types.CommRStar{},
		}
		copy(comms.CommD[:], commD)
		copy(comms.CommR[:], commR)
		copy(comms.CommRStar[:], commRStar)
		state.LastUsedSectorID = sectorID
		state.SectorCommitments[sectorIDstr] = comms
		_, ret, err := ctx.Send(address.StorageMarketAddress, "updatePower", types.ZeroAttoFIL, []interface{}{inc})
		if err != nil {
			return nil, err
		}
		if ret != 0 {
			return nil, Errors[ErrStoragemarketCallFailed]
		}
		return nil, nil
	})
	if err != nil {
		return errors.CodeError(err), err
	}

	return 0, nil
}

// VerifyPieceInclusion verifies that proof proves that the data represented by commP is included in the sector.
// This method returns nothing if the verification succeeds and returns a revert error if verification fails.
func (ma *Actor) VerifyPieceInclusion(ctx exec.VMContext, commP []byte, sectorID uint64, proof []byte) (uint8, error) {
	if err := ctx.Charge(actor.DefaultGasCost); err != nil {
		return exec.ErrInsufficientGas, errors.RevertErrorWrap(err, "Insufficient gas")
	}

	var state State
	_, err := actor.WithState(ctx, &state, func() (interface{}, error) {

		// If miner has not committed sector id, proof is invalid
		sectorIDstr := strconv.FormatUint(sectorID, 10)
		commitment, ok := state.SectorCommitments[sectorIDstr]
		if !ok {
			return nil, errors.NewRevertError("sector not committed")
		}

		// If miner is not up-to-date on their PoSts, proof is invalid
		if state.LastPoSt == nil {
			return nil, errors.NewRevertError("proofs out of date")
		}

		clientProofsTimeout := state.LastPoSt.Add(types.NewBlockHeight(PieceInclusionGracePeriodBlocks))
		if ctx.BlockHeight().GreaterThan(clientProofsTimeout) {
			return nil, errors.NewRevertError("proofs out of date")
		}

		// Verify proof proves CommP is in sector's CommD
		var typedCommP types.CommP
		copy(typedCommP[:], commP)
		valid, err := verifyInclusionProof(typedCommP, commitment.CommD, proof)
		if err != nil {
			return nil, err
		}

		if !valid {
			return nil, errors.NewRevertError("invalid inclusion proof")
		}

		return nil, nil
	})

	return errors.CodeError(err), err
}

// GetKey returns the public key for this miner.
func (ma *Actor) GetKey(ctx exec.VMContext) ([]byte, uint8, error) {
	if err := ctx.Charge(actor.DefaultGasCost); err != nil {
		return nil, exec.ErrInsufficientGas, errors.RevertErrorWrap(err, "Insufficient gas")
	}

	var state State
	out, err := actor.WithState(ctx, &state, func() (interface{}, error) {
		return state.PublicKey, nil
	})
	if err != nil {
		return nil, errors.CodeError(err), err
	}

	validOut, ok := out.([]byte)
	if !ok {
		return nil, 1, errors.NewRevertError("expected a byte slice")
	}

	return validOut, 0, nil
}

// GetPeerID returns the libp2p peer ID that this miner can be reached at.
func (ma *Actor) GetPeerID(ctx exec.VMContext) (peer.ID, uint8, error) {
	if err := ctx.Charge(actor.DefaultGasCost); err != nil {
		return peer.ID(""), exec.ErrInsufficientGas, errors.RevertErrorWrap(err, "Insufficient gas")
	}

	var state State

	chunk, err := ctx.ReadStorage()
	if err != nil {
		return peer.ID(""), errors.CodeError(err), err
	}

	if err := actor.UnmarshalStorage(chunk, &state); err != nil {
		return peer.ID(""), errors.CodeError(err), err
	}

	return state.PeerID, 0, nil
}

// UpdatePeerID is used to update the peerID this miner is operating under.
func (ma *Actor) UpdatePeerID(ctx exec.VMContext, pid peer.ID) (uint8, error) {
	if err := ctx.Charge(actor.DefaultGasCost); err != nil {
		return exec.ErrInsufficientGas, errors.RevertErrorWrap(err, "Insufficient gas")
	}

	var storage State
	_, err := actor.WithState(ctx, &storage, func() (interface{}, error) {
		// verify that the caller is authorized to perform update
		if ctx.Message().From != storage.Owner {
			return nil, Errors[ErrCallerUnauthorized]
		}

		storage.PeerID = pid

		return nil, nil
	})
	if err != nil {
		return errors.CodeError(err), err
	}

	return 0, nil
}

// GetPower returns the amount of proven sectors for this miner.
func (ma *Actor) GetPower(ctx exec.VMContext) (*types.BytesAmount, uint8, error) {
	if err := ctx.Charge(actor.DefaultGasCost); err != nil {
		return nil, exec.ErrInsufficientGas, errors.RevertErrorWrap(err, "Insufficient gas")
	}

	var state State
	ret, err := actor.WithState(ctx, &state, func() (interface{}, error) {
		return state.Power, nil
	})
	if err != nil {
		return nil, errors.CodeError(err), err
	}

	power, ok := ret.(*types.BytesAmount)
	if !ok {
		return nil, 1, errors.NewFaultErrorf("expected *types.BytesAmount to be returned, but got %T instead", ret)
	}

	return power, 0, nil
}

// SubmitPoSt is used to submit a coalesced PoST to the chain to convince the chain
// that you have been actually storing the files you claim to be.
func (ma *Actor) SubmitPoSt(ctx exec.VMContext, poStProofs []types.PoStProof) (uint8, error) {
	if err := ctx.Charge(actor.DefaultGasCost); err != nil {
		return exec.ErrInsufficientGas, errors.RevertErrorWrap(err, "Insufficient gas")
	}

	chainHeight := ctx.BlockHeight()
	sender := ctx.Message().From
	var state State
	_, err := actor.WithState(ctx, &state, func() (interface{}, error) {
		// verify that the caller is authorized to perform update
		if sender != state.Owner {
			return nil, Errors[ErrCallerUnauthorized]
		}

		// Calcuate any penalties for late submission
		provingPeriodEnd := state.ProvingPeriodStart.Add(types.NewBlockHeight(ProvingPeriodDuration(state.SectorSize)))
		generationAttackGracePeriod := GenerationAttackTime(state.SectorSize)
		if chainHeight.GreaterThan(provingPeriodEnd.Add(generationAttackGracePeriod)) {
			// The PoSt has been submitted after the generation attack time.
			// The miner can expect to be slashed, and so for now the PoSt is rejected.
			// An alternative would be to apply the penalties here, duplicating the behaviour
			// of SlashStorageFault.
			return nil, errors.NewRevertErrorf("PoSt submitted later than grace period of %s rounds after proving period end",
				generationAttackGracePeriod)
		}

		feeRequired := LatePoStFee(state.ActiveCollateral, provingPeriodEnd, chainHeight, generationAttackGracePeriod)

		// The message value has been added to the actor's balance.
		// Ensure this value fully covers the fee which will be charged to this balance so that the resulting
		// balance (whichs forms pledge & storage collateral) is not less than it was before.
		messageValue := ctx.Message().Value
		if messageValue.LessThan(feeRequired) {
			return nil, errors.NewRevertErrorf("PoSt message requires value of at least %s attofil to cover fees, got %s", feeRequired, messageValue)
		}

		// Since the message value was at least equal to this fee, this burn should not fail due to
		// insufficient balance.
		err := ma.burnFunds(ctx, feeRequired)
		if err != nil {
			return nil, errors.RevertErrorWrapf(err, "Failed to burn fee %s", feeRequired)
		}

		// Refund any overpayment of fees to the owner.
		if messageValue.GreaterThan(feeRequired) {
			overpayment := messageValue.Sub(feeRequired)
			_, _, err := ctx.Send(sender, "", overpayment, []interface{}{})
			if err != nil {
				return nil, errors.NewRevertErrorf("Failed to refund overpayment of %s to %s", overpayment, sender)
			}
		}

		// As with commitSector messages, bootstrap miner actors don't verify
		// the submitPoSt messages that they are sent.
		//
		// This switching will be removed when issue #2270 is completed.
		if !ma.Bootstrap {
			seed, err := currentProvingPeriodPoStChallengeSeed(ctx, state)
			if err != nil {
				return nil, errors.RevertErrorWrap(err, "failed to sample chain for challenge seed")
			}

			var commRs []types.CommR
			for _, v := range state.SectorCommitments {
				commRs = append(commRs, v.CommR)
			}

			sortedCommRs := proofs.NewSortedCommRs(commRs...)

			req := proofs.VerifyPoStRequest{
				ChallengeSeed: seed,
				SortedCommRs:  sortedCommRs,
				Faults:        []uint64{},
				Proofs:        poStProofs,
				SectorSize:    state.SectorSize,
			}

			res, err := (&proofs.RustVerifier{}).VerifyPoST(req)
			if err != nil {
				return nil, errors.RevertErrorWrap(err, "failed to verify PoSt")
			}
			if !res.IsValid {
				return nil, Errors[ErrInvalidPoSt]
			}
		}

		// transition to the next proving period
		state.ProvingPeriodStart = provingPeriodEnd
		state.LastPoSt = chainHeight

		return nil, nil
	})
	if err != nil {
		return errors.CodeError(err), err
	}

	return 0, nil
}

// GetProvingPeriodStart returns the current ProvingPeriodStart value.
func (ma *Actor) GetProvingPeriodStart(ctx exec.VMContext) (*types.BlockHeight, uint8, error) {
	if err := ctx.Charge(actor.DefaultGasCost); err != nil {
		return nil, exec.ErrInsufficientGas, errors.RevertErrorWrap(err, "Insufficient gas")
	}

	chunk, err := ctx.ReadStorage()
	if err != nil {
		return nil, errors.CodeError(err), err
	}

	var state State
	if err := actor.UnmarshalStorage(chunk, &state); err != nil {
		return nil, errors.CodeError(err), err
	}

	return state.ProvingPeriodStart, 0, nil
}

//
// Un-exported methods
//

func (ma *Actor) burnFunds(ctx exec.VMContext, amount types.AttoFIL) error {
	_, _, err := ctx.Send(address.BurntFundsAddress, "", amount, []interface{}{})
	return err
}

//
// Exported free functions.
//

// GetProofsMode returns the genesis block-configured proofs mode.
func GetProofsMode(ctx exec.VMContext) (types.ProofsMode, error) {
	var proofsMode types.ProofsMode
	msgResult, _, err := ctx.Send(address.StorageMarketAddress, "getProofsMode", types.ZeroAttoFIL, nil)
	if err != nil {
		return types.TestProofsMode, xerrors.Wrap(err, "'getProofsMode' message failed")
	}
	if err := cbor.DecodeInto(msgResult[0], &proofsMode); err != nil {
		return types.TestProofsMode, xerrors.Wrap(err, "could not unmarshall sector store type")
	}
	return proofsMode, nil
}

// CollateralForSector returns the collateral required to commit a sector of the
// given size.
func CollateralForSector(sectorSize *types.BytesAmount) types.AttoFIL {
	// TODO: Replace this function with the baseline pro-rata construction.
	// https://github.com/filecoin-project/go-filecoin/issues/2866
	return MinimumCollateralPerSector
}

// GenerationAttackTime is the number of blocks after a proving period ends
// after which a storage miner will be subject to storage fault slashing.
//
// TODO: How do we compute a non-bogus return value here?
// https://github.com/filecoin-project/specs/issues/322
func GenerationAttackTime(sectorSize *types.BytesAmount) *types.BlockHeight {
	return types.NewBlockHeight(LargestSectorGenerationAttackThresholdBlocks)
}

// ProvingPeriodDuration returns the number of blocks in a proving period for a
// given sector size.
//
// TODO: Make this function return a non-bogus value.
// https://github.com/filecoin-project/specs/issues/321
func ProvingPeriodDuration(sectorSize *types.BytesAmount) uint64 {
	return LargestSectorSizeProvingPeriodBlocks
}

// LatePostFee calculates the fee from pledge collateral that a miner must pay for submitting a PoSt
// after the proving period has ended.
// The fee is calculated as a linear proportion of pledge collateral given by the lateness as a
// fraction of the maximum possible lateness (i.e. the generation attack grace period).
// If the submission is on-time, the fee is zero. If the submission is after the maximum allowed lateness
// the fee amounts to the entire pledge collateral.
func LatePoStFee(pledgeCollateral types.AttoFIL, provingPeriodEnd *types.BlockHeight, chainHeight *types.BlockHeight, maxRoundsLate *types.BlockHeight) types.AttoFIL {
	roundsLate := chainHeight.Sub(provingPeriodEnd)
	if roundsLate.GreaterEqual(maxRoundsLate) {
		return pledgeCollateral
	} else if roundsLate.GreaterThan(types.NewBlockHeight(0)) {
		// fee = collateral * (roundsLate / maxRoundsLate)
		var fee big.Int
		fee.Mul(pledgeCollateral.AsBigInt(), roundsLate.AsBigInt())
		fee.Div(&fee, maxRoundsLate.AsBigInt()) // Integer division in AttoFIL, rounds towards zero.
		return types.NewAttoFIL(&fee)
	}
	return types.ZeroAttoFIL
}

//
// Internal functions
//

func currentProvingPeriodPoStChallengeSeed(ctx exec.VMContext, state State) (types.PoStChallengeSeed, error) {
	bytes, err := ctx.SampleChainRandomness(state.ProvingPeriodStart)
	if err != nil {
		return types.PoStChallengeSeed{}, err
	}

	seed := types.PoStChallengeSeed{}
	copy(seed[:], bytes)

	return seed, nil
}

// TODO: This is a fake implementation pending availability of the verification algorithm in rust proofs
// see https://github.com/filecoin-project/go-filecoin/issues/2629
func verifyInclusionProof(commP types.CommP, commD types.CommD, proof []byte) (bool, error) {
	if len(proof) != 2*int(types.CommitmentBytesLen) {
		return false, errors.NewRevertError("malformed inclusion proof")
	}
	combined := []byte{}
	combined = append(combined, commP[:]...)
	combined = append(combined, commD[:]...)

	return bytes.Equal(combined, proof), nil
}
