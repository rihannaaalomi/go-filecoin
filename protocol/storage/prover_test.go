package storage_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/filecoin-project/go-filecoin/address"
	"github.com/filecoin-project/go-filecoin/proofs"
	"github.com/filecoin-project/go-filecoin/protocol/storage"
	"github.com/filecoin-project/go-filecoin/types"
)

func TestProver(t *testing.T) {
	ctx := context.Background()
	makeAddress := address.NewForTestGetter()
	actorAddress := makeAddress()
	ownerAddress := makeAddress()

	var fakeSeed types.PoStChallengeSeed
	var fakeInputs []storage.PoStInputs

	t.Run("produces proof", func(t *testing.T) {
		start := types.NewBlockHeight(100)
		end := types.NewBlockHeight(200)
		fake := &fakeProverDeps{
			seed:   fakeSeed,
			height: end.Sub(types.NewBlockHeight(1)),
			proofs: []types.PoStProof{{1, 2, 3, 4}},
			faults: []uint64{},
		}
		prover := storage.NewProver(actorAddress, ownerAddress, fake, fake)

		submission, e := prover.CalculatePoSt(ctx, start, end, fakeInputs)
		require.NoError(t, e)
		assert.Equal(t, fake.proofs, submission.Proofs)
	})
}

type fakeProverDeps struct {
	seed   types.PoStChallengeSeed
	height *types.BlockHeight
	proofs []types.PoStProof
	faults []uint64
}

func (f *fakeProverDeps) ChainHeight() (*types.BlockHeight, error) {
	return f.height, nil
}

func (f *fakeProverDeps) ChallengeSeed(ctx context.Context, periodStart *types.BlockHeight) (types.PoStChallengeSeed, error) {
	return f.seed, nil
}

func (f *fakeProverDeps) CalculatePost(sortedCommRs proofs.SortedCommRs, seed types.PoStChallengeSeed) ([]types.PoStProof, []uint64, error) {
	return f.proofs, f.faults, nil
}
