package rhp

import (
	"math/bits"

	"go.sia.tech/core/consensus"
	"go.sia.tech/core/types"
)

// ContractFormationCost returns the cost of forming a contract.
func ContractFormationCost(fc types.FileContract, contractFee types.Currency) types.Currency {
	return fc.ValidRenterPayout().Add(contractFee).Add(contractTax(fc))
}

// ContractFormationCollateral returns the amount of collateral we add when forming a contract.
func ContractFormationCollateral(expectedStorage, period uint64, host HostSettings) types.Currency {
	hostCollateral := host.Collateral.Mul64(expectedStorage).Mul64(period)
	if hostCollateral.Cmp(host.MaxCollateral) > 0 {
		hostCollateral = host.MaxCollateral
	}
	return hostCollateral
}

// PrepareContractFormation constructs a contract formation transaction.
func PrepareContractFormation(renterKey types.PrivateKey, hostKey types.PublicKey, renterPayout, hostCollateral types.Currency, endHeight uint64, host HostSettings, refundAddr types.Address) types.FileContract {
	renterPubkey := renterKey.PublicKey()
	uc := types.UnlockConditions{
		PublicKeys: []types.UnlockKey{
			{Algorithm: types.SpecifierEd25519, Key: renterPubkey[:]},
			{Algorithm: types.SpecifierEd25519, Key: hostKey[:]},
		},
		SignaturesRequired: 2,
	}

	hostPayout := host.ContractPrice.Add(hostCollateral)
	payout := taxAdjustedPayout(renterPayout.Add(hostPayout))

	return types.FileContract{
		Filesize:       0,
		FileMerkleRoot: types.Hash256{},
		WindowStart:    uint64(endHeight),
		WindowEnd:      uint64(endHeight + host.WindowSize),
		Payout:         payout,
		UnlockHash:     types.Hash256(uc.UnlockHash()),
		RevisionNumber: 0,
		ValidProofOutputs: []types.SiacoinOutput{
			// outputs need to account for tax
			{Value: renterPayout, Address: refundAddr},
			// collateral is returned to host
			{Value: hostPayout, Address: host.Address},
		},
		MissedProofOutputs: []types.SiacoinOutput{
			// same as above
			{Value: renterPayout, Address: refundAddr},
			// same as above
			{Value: hostPayout, Address: host.Address},
			// once we start doing revisions, we'll move some coins to the host and some to the void
			{Value: types.ZeroCurrency, Address: types.Address{}},
		},
	}
}

// ContractRenewalCost returns the cost of renewing a contract.
func ContractRenewalCost(fc types.FileContract, contractFee types.Currency) types.Currency {
	return fc.ValidRenterPayout().Add(contractFee).Add(contractTax(fc))
}

// ContractRenewalCollateral returns the amount of collateral we add when
// renewing a contract. It takes into account the host's max collateral setting
// and ensures the total collateral does not exceed it.
func ContractRenewalCollateral(fc types.FileContract, renterFunds types.Currency, host HostSettings, endHeight uint64) types.Currency {
	if endHeight < fc.EndHeight() {
		panic("endHeight should be at least the current end height of the contract")
	}
	extension := endHeight - fc.EndHeight()

	// calculate cost per byte
	costPerByte := host.UploadBandwidthPrice.Add(host.StoragePrice).Add(host.DownloadBandwidthPrice)
	if costPerByte.IsZero() {
		return types.ZeroCurrency
	}

	// calculate the base collateral - if it exceeds MaxCollateral we can't add more collateral
	baseCollateral := host.Collateral.Mul64(fc.Filesize).Mul64(extension)
	if baseCollateral.Cmp(host.MaxCollateral) >= 0 {
		return types.ZeroCurrency
	}

	// calculate the new collateral
	newCollateral := host.Collateral.Mul(renterFunds.Div(costPerByte))

	// if the total collateral is more than the MaxCollateral subtract the
	// delta.
	totalCollateral := baseCollateral.Add(newCollateral)
	if totalCollateral.Cmp(host.MaxCollateral) > 0 {
		delta := totalCollateral.Sub(host.MaxCollateral)
		if delta.Cmp(newCollateral) > 0 {
			newCollateral = types.ZeroCurrency
		} else {
			newCollateral = newCollateral.Sub(delta)
		}
	}
	return newCollateral
}

// PrepareContractRenewal constructs a contract renewal transaction.
func PrepareContractRenewal(currentRevision types.FileContractRevision, renterAddress types.Address, renterKey types.PrivateKey, renterPayout, newCollateral types.Currency, hostKey types.PublicKey, host HostSettings, endHeight uint64) types.FileContract {
	hostValidPayout, hostMissedPayout, voidMissedPayout := CalculateHostPayouts(currentRevision.FileContract, newCollateral, host, endHeight)

	return types.FileContract{
		Filesize:       currentRevision.Filesize,
		FileMerkleRoot: currentRevision.FileMerkleRoot,
		WindowStart:    uint64(endHeight),
		WindowEnd:      uint64(endHeight + host.WindowSize),
		Payout:         taxAdjustedPayout(renterPayout.Add(hostValidPayout)),
		UnlockHash:     currentRevision.UnlockHash,
		RevisionNumber: 0,
		ValidProofOutputs: []types.SiacoinOutput{
			{Value: renterPayout, Address: renterAddress},
			{Value: hostValidPayout, Address: host.Address},
		},
		MissedProofOutputs: []types.SiacoinOutput{
			{Value: renterPayout, Address: renterAddress},
			{Value: hostMissedPayout, Address: host.Address},
			{Value: voidMissedPayout, Address: types.Address{}},
		},
	}
}

// CalculateHostPayouts calculates the contract payouts for the host.
func CalculateHostPayouts(fc types.FileContract, newCollateral types.Currency, settings HostSettings, endHeight uint64) (types.Currency, types.Currency, types.Currency) {
	// The host gets their contract fee, plus the cost of the data already in the
	// contract, plus their collateral. In the event of a missed payout, the cost
	// and collateral of the data already in the contract is subtracted from the
	// host, and sent to the void instead.
	//
	// However, it is possible for this subtraction to underflow: this can happen if
	// baseCollateral is large and MaxCollateral is small. We cannot simply replace
	// the underflow with a zero, because the host performs the same subtraction and
	// returns an error on underflow. Nor can we increase the valid payout, because
	// the host calculates its collateral contribution by subtracting the contract
	// price and base price from this payout, and we're already at MaxCollateral.
	// Thus the host has conflicting requirements, and renewing the contract is
	// impossible until they change their settings.

	// calculate base price and collateral
	var basePrice, baseCollateral types.Currency

	// if the contract height did not increase both prices are zero
	if contractEnd := uint64(endHeight + settings.WindowSize); contractEnd > fc.WindowEnd {
		timeExtension := uint64(contractEnd - fc.WindowEnd)
		basePrice = settings.StoragePrice.Mul64(fc.Filesize).Mul64(timeExtension)
		baseCollateral = settings.Collateral.Mul64(fc.Filesize).Mul64(timeExtension)
	}

	// calculate payouts
	hostValidPayout := settings.ContractPrice.Add(basePrice).Add(baseCollateral).Add(newCollateral)
	voidMissedPayout := basePrice.Add(baseCollateral)
	if hostValidPayout.Cmp(voidMissedPayout) < 0 {
		// TODO: detect this elsewhere
		panic("host's settings are unsatisfiable")
	}
	hostMissedPayout := hostValidPayout.Sub(voidMissedPayout)
	return hostValidPayout, hostMissedPayout, voidMissedPayout
}

// NOTE: due to a bug in the transaction validation code, calculating payouts
// is way harder than it needs to be. Tax is calculated on the post-tax
// contract payout (instead of the sum of the renter and host payouts). So the
// equation for the payout is:
//
//	   payout = renterPayout + hostPayout + payout*tax
//	∴  payout = (renterPayout + hostPayout) / (1 - tax)
//
// This would work if 'tax' were a simple fraction, but because the tax must
// be evenly distributed among siafund holders, 'tax' is actually a function
// that multiplies by a fraction and then rounds down to the nearest multiple
// of the siafund count. Thus, when inverting the function, we have to make an
// initial guess and then fix the rounding error.
func taxAdjustedPayout(target types.Currency) types.Currency {
	// compute initial guess as target * (1 / 1-tax); since this does not take
	// the siafund rounding into account, the guess will be up to
	// types.SiafundCount greater than the actual payout value.
	guess := target.Mul64(1000).Div64(961)

	// now, adjust the guess to remove the rounding error. We know that:
	//
	//   (target % types.SiafundCount) == (payout % types.SiafundCount)
	//
	// therefore, we can simply adjust the guess to have this remainder as
	// well. The only wrinkle is that, since we know guess >= payout, if the
	// guess remainder is smaller than the target remainder, we must subtract
	// an extra types.SiafundCount.
	//
	// for example, if target = 87654321 and types.SiafundCount = 10000, then:
	//
	//   initial_guess  = 87654321 * (1 / (1 - tax))
	//                  = 91211572
	//   target % 10000 =     4321
	//   adjusted_guess = 91204321

	mod64 := func(c types.Currency, v uint64) types.Currency {
		var r uint64
		if c.Hi < v {
			_, r = bits.Div64(c.Hi, c.Lo, v)
		} else {
			_, r = bits.Div64(0, c.Hi, v)
			_, r = bits.Div64(r, c.Lo, v)
		}
		return types.NewCurrency64(r)
	}
	sfc := (consensus.State{}).SiafundCount()
	tm := mod64(target, sfc)
	gm := mod64(guess, sfc)
	if gm.Cmp(tm) < 0 {
		guess = guess.Sub(types.NewCurrency64(sfc))
	}
	return guess.Add(tm).Sub(gm)
}
