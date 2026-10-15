// Copyright 2026 Blink Labs Software
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package txpump

import (
	"bytes"
	"errors"
	"fmt"
	"sync"
	"time"
)

func init() { RegisterApp("auction", mustNewAuctionApp()) }

const (
	auctionStartBidMin = 4_000_000
	auctionStartBidMax = 6_000_000
	// auctionBidStepMin and auctionBidStepMax bound how far a bid rises
	// above the highest bid.
	auctionBidStepMin = 1_000_000
	auctionBidStepMax = 3_000_000
	// auctionDurationMinSlots and auctionDurationMaxSlots bound how long an
	// auction takes bids. The minimum leaves room for auctionBidTTLSlots.
	auctionDurationMinSlots = 60
	auctionDurationMaxSlots = 180
	// auctionBidTTLSlots is how far past the current slot a transaction that
	// must end before the expiration stays valid.
	auctionBidTTLSlots = 20
	// auctionItemLovelace accompanies the item when it is sent to its
	// winner, above the minimum output for a single-asset value.
	auctionItemLovelace uint64 = 2_000_000
	// auctionFeeBudget is the wallet value selected beyond what a step pays
	// out, covering the fee and a change output.
	auctionFeeBudget uint64 = 3_000_000
	// auctionCollateral is the value pledged as collateral, far above 150%
	// of any auction step's fee.
	auctionCollateral uint64 = 5_000_000
	// auctionMaxRejections bounds how often a position may fail to be spent
	// before it is abandoned, so a position whose creating transaction never
	// reached the chain, rejected with an unrecognised message, is not
	// retried forever.
	auctionMaxRejections = 3
	// auctionFromEpoch matches the plutus workload's gate.
	auctionFromEpoch = 3
)

type auctionAction int

const (
	auctionStart auctionAction = iota
	auctionBid
	auctionEnd
)

func (a auctionAction) String() string {
	return [...]string{"start", "bid", "end"}[a]
}

// auctionPosition is one open auction: the script UTxO this process created.
type auctionPosition struct {
	utxo  UTxO
	known []KnownUTxO // outputs of the transaction that created it
	// seller opened the auction; bidder is nil until the first bid.
	seller, bidder *User
	// bid is the datum's highest bid, which is also the lovelace the UTxO
	// holds.
	bid int64
	// expiration is POSIX milliseconds, as in the datum.
	expiration int64
	lovelace   uint64
	assetName  []byte
	// availableAt keeps a new position untouched until its creating
	// transaction has had the confirmation window to reach the chain.
	availableAt time.Time
	// busy marks a position claimed by a step in progress.
	busy       bool
	rejections int
}

// auctionApp runs English auctions through the auction template: a seller
// mints an item into the auction UTxO, others outbid each other until the
// expiration, and the auction then settles to the winner.
type auctionApp struct {
	validator PlutusV3Validator
	// choose returns a value in [lo, hi]; IntRange outside tests.
	choose func(lo, hi int) int
	now    func() time.Time

	mu        sync.Mutex
	positions []*auctionPosition
	seq       int
}

func newAuctionApp() (*auctionApp, error) {
	v, err := LoadValidator("auction.json", "auction.auction.spend")
	if err != nil {
		return nil, err
	}
	return &auctionApp{validator: v, choose: IntRange, now: time.Now}, nil
}

// mustNewAuctionApp panics only if the embedded blueprint is unusable, which
// is a build-time defect.
func mustNewAuctionApp() *auctionApp {
	a, err := newAuctionApp()
	if err != nil {
		panic(fmt.Sprintf("txpump: auction blueprint: %v", err))
	}
	return a
}

// Enabled reports whether there is a second user to bid.
func (a *auctionApp) Enabled(users int) bool { return users >= 2 }

// isKeyAddress reports whether addr is an enterprise key address, the only
// form the validator's whole-address comparisons can match.
func isKeyAddress(addr []byte) bool { return len(addr) == 29 && addr[0]>>4 == 0x6 }

func (a *auctionApp) Step(env *AppEnv) (bool, error) {
	if len(env.Cfg.PlutusV3CostModel) == 0 {
		return false, nil
	}
	epochLength := env.Cfg.EpochLength
	if epochLength == 0 {
		epochLength = 500
	}
	if env.Slot/epochLength < auctionFromEpoch {
		return false, nil
	}
	params := scriptParamsFor(env.Cfg)
	// Without the real system start the expiration written to a datum and
	// the time a node derives from a slot would disagree.
	if params.SystemStart.IsZero() || params.SlotLength <= 0 {
		return false, nil
	}
	if !isKeyAddress(env.User.Key.Address) {
		return false, nil
	}
	nowMs := slotMillis(params, env.Slot)
	cutoffMs := slotMillis(params, env.Slot+auctionBidTTLSlots)
	pos, action := a.claim(env.User, nowMs, cutoffMs)
	if pos == nil {
		return a.start(env, params, nowMs)
	}
	if action == auctionEnd {
		return a.end(env, params, pos)
	}
	return a.bid(env, params, pos)
}

// actionFor is what user can do with p, if anything. A bid must be able to
// end its validity window by the expiration (the upper bound is exclusive, so
// it may equal it), so bidding closes auctionBidTTLSlots early (cutoffMs is
// the end of the window a bid started now would have). Settling needs a lower bound strictly after the
// expiration; with bids anyone may settle, without them only the seller,
// whose signature the validator asks for.
func actionFor(p *auctionPosition, user *User, nowMs, cutoffMs int64) (auctionAction, bool) {
	if nowMs > p.expiration {
		if p.bidder != nil || sameUser(p.seller, user) {
			return auctionEnd, true
		}
		return 0, false
	}
	if p.expiration >= cutoffMs && !sameUser(p.seller, user) && !sameUser(p.bidder, user) {
		return auctionBid, true
	}
	return 0, false
}

// claim picks the action for user and, for any action but start, marks the
// position it acts on busy so that no concurrent step can spend it.
func (a *auctionApp) claim(user *User, nowMs, cutoffMs int64) (*auctionPosition, auctionAction) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.choose(0, 3) == 0 {
		return nil, auctionStart
	}
	now := a.now()
	var claimable []*auctionPosition
	for _, p := range a.positions {
		if p.busy || now.Before(p.availableAt) {
			continue
		}
		if _, ok := actionFor(p, user, nowMs, cutoffMs); ok {
			claimable = append(claimable, p)
		}
	}
	if len(claimable) == 0 {
		return nil, auctionStart
	}
	p := claimable[a.choose(0, len(claimable)-1)]
	action, _ := actionFor(p, user, nowMs, cutoffMs)
	p.busy = true
	return p, action
}

// settle releases a claimed position. A spent or dropped position is
// removed, next (the position a bid creates) is added, and a failed attempt
// counts toward auctionMaxRejections.
func (a *auctionApp) settle(p *auctionPosition, spent bool, next *auctionPosition, failed, drop bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	p.busy = false
	if failed {
		p.rejections++
		if p.rejections >= auctionMaxRejections {
			drop = true
		}
	}
	if spent || drop {
		for i, q := range a.positions {
			if q == p {
				a.positions = append(a.positions[:i], a.positions[i+1:]...)
				break
			}
		}
	}
	if next != nil {
		a.positions = append(a.positions, next)
	}
}

func (a *auctionApp) add(p *auctionPosition) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.positions = append(a.positions, p)
}

// nextAssetName returns a name no earlier auction of this run used.
func (a *auctionApp) nextAssetName(slot uint64) []byte {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.seq++
	return []byte(fmt.Sprintf("lot-%d-%d", slot, a.seq))
}

// itemOf is the single item an auction holds.
func (a *auctionApp) itemOf(name []byte) []NativeAsset {
	return []NativeAsset{{Policy: a.validator.Hash, Name: name, Quantity: 1}}
}

// start mints a new item into an auction opened by env.User.
func (a *auctionApp) start(env *AppEnv, params ScriptParams, nowMs int64) (bool, error) {
	seller := env.User
	//nolint:gosec // bounded by small constants
	opening := uint64(a.choose(auctionStartBidMin, auctionStartBidMax))
	duration := a.choose(auctionDurationMinSlots, auctionDurationMaxSlots)
	expiration := nowMs + int64(duration)*params.SlotLength.Milliseconds()
	name := a.nextAssetName(env.Slot)
	item := a.itemOf(name)

	inputs, change, err := env.Wallet.SelectCoinsForKey(seller.Key, opening+auctionFeeBudget)
	if errors.Is(err, ErrInsufficientFunds) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	collateral, ok, err := a.collateralFor(env, params, inputs)
	if !ok {
		return false, err
	}
	spec := ScriptTxSpec{
		Validator:    a.validator,
		CostModel:    params.CostModel,
		Mint:         item,
		MintRedeemer: auctionMintRedeemer(),
		WalletInputs: inputs,
		Collateral:   collateral,
		Outputs: []ScriptOutput{
			{
				Address:  a.validator.Address(),
				Lovelace: opening,
				Assets:   item,
				Datum:    auctionDatum(keyHash(seller.Key), nil, int64(opening), expiration, a.validator.Hash, name), //nolint:gosec // small
			},
			{Address: seller.Key.Address, Lovelace: change + auctionFeeBudget},
		},
		ChangeIndex:     1,
		RequiredSigners: [][]byte{keyHash(seller.Key)},
		SigningKeys:     []*UTxOKey{seller.Key},
		// The validator wants the range to end before the expiration.
		TTL: env.Slot + auctionBidTTLSlots,
	}
	held := append(append([]UTxO(nil), inputs...), collateral...)
	txBytes, txID, err := PlanScriptTx(spec, NewVerifier(params), params, walletKnown(inputs, collateral), env.Slot)
	if err != nil {
		env.Wallet.ReturnUTxOs(held)
		return false, fmt.Errorf("auction start: %w", err)
	}
	if err := env.Submit("auction", txID, txBytes); err != nil {
		env.Wallet.ReturnUTxOs(held)
		env.Logger.Warn("tx rejected", "action", "start", "tx_id", txID, "err", err)
		return false, nil
	}
	known, err := KnownUTxOsFromTx(txBytes)
	if err != nil {
		return false, err
	}
	delay := env.Cfg.confirmationDelay()
	a.add(&auctionPosition{
		utxo:        UTxO{TxHash: txID, Index: 0, Amount: opening},
		known:       known,
		seller:      seller,
		bid:         int64(opening), //nolint:gosec // small
		expiration:  expiration,
		lovelace:    opening,
		assetName:   name,
		availableAt: a.now().Add(delay),
	})
	env.Wallet.RecordAccepted(txID, held, a.walletOutputs(txID, known, seller), delay)
	env.Logger.Info("auction step", "action", "start", "tx_id", txID, "opening_bid", opening, "expiration", expiration)
	return true, nil
}

// collateralFor selects collateral for env.User, returning inputs to the
// wallet and ok=false when none can be had.
func (a *auctionApp) collateralFor(env *AppEnv, params ScriptParams, inputs []UTxO) ([]UTxO, bool, error) {
	collateral, _, err := env.Wallet.SelectCoinsForKey(env.User.Key, auctionCollateral)
	if err != nil || uint(len(collateral)) > params.MaxCollateralInputs { //nolint:gosec // small slice
		env.Wallet.ReturnUTxOs(append(append([]UTxO(nil), inputs...), collateral...))
		if err != nil && !errors.Is(err, ErrInsufficientFunds) {
			return nil, false, err
		}
		return nil, false, nil
	}
	return collateral, true, nil
}

// walletOutputs lists the outputs of a submitted transaction that pay a
// user's key address, so the wallet can spend them again. An output holding
// the item is left out: the wallet spends coins as plain ADA, and spending
// one without returning the item would burn it and fail the transaction.
func (a *auctionApp) walletOutputs(txID string, outs []KnownUTxO, users ...*User) []UTxO {
	var walletOuts []UTxO
	for i, o := range outs {
		if len(o.Assets) > 0 {
			continue
		}
		for _, u := range users {
			if u != nil && bytes.Equal(o.Address, u.Key.Address) {
				walletOuts = append(walletOuts, UTxO{
					TxHash: txID, Index: uint32(i), Amount: o.Lovelace, SigningKey: u.Key, //nolint:gosec // few outputs
				})
				break
			}
		}
	}
	return walletOuts
}

// bid outbids the position's leader with the whole new bid paid from env.User's
// wallet; the previous bid returns to its bidder out of the locked value.
func (a *auctionApp) bid(env *AppEnv, params ScriptParams, pos *auctionPosition) (bool, error) {
	bidder := env.User
	//nolint:gosec // bounded by small constants
	newBid := pos.bid + int64(a.choose(auctionBidStepMin, auctionBidStepMax))
	spec := ScriptTxSpec{
		Validator: a.validator,
		CostModel: params.CostModel,
	}
	// With a leader the previous bid comes back out of the locked value; on
	// a first bid the seller's opening deposit stays in the pot.
	var refund uint64
	outputs := []ScriptOutput{{
		Address:  a.validator.Address(),
		Lovelace: uint64(newBid), //nolint:gosec // positive
		Assets:   a.itemOf(pos.assetName),
		Datum:    auctionDatum(keyHash(pos.seller.Key), keyHash(bidder.Key), newBid, pos.expiration, a.validator.Hash, pos.assetName),
	}}
	if pos.bidder != nil {
		refund = pos.lovelace
		outputs = append(outputs, ScriptOutput{Address: pos.bidder.Key.Address, Lovelace: refund})
	}
	inputs, change, err := env.Wallet.SelectCoinsForKey(bidder.Key, uint64(newBid)+auctionFeeBudget) //nolint:gosec // positive
	if errors.Is(err, ErrInsufficientFunds) {
		a.settle(pos, false, nil, false, false)
		return false, nil
	}
	if err != nil {
		a.settle(pos, false, nil, false, false)
		return false, err
	}
	collateral, ok, err := a.collateralFor(env, params, inputs)
	if !ok {
		a.settle(pos, false, nil, false, false)
		return false, err
	}
	// The wallet gives newBid, so beyond the fee what it gets back is what
	// it selected less the bid, plus the locked value the new state and the
	// refund do not keep.
	changeOut := change + auctionFeeBudget + pos.lovelace - refund
	outputs = append(outputs, ScriptOutput{Address: bidder.Key.Address, Lovelace: changeOut})
	spec.Outputs = outputs
	spec.ChangeIndex = len(outputs) - 1
	spec.WalletInputs = inputs
	spec.Collateral = collateral
	spec.ScriptInputs = []ScriptInput{{UTxO: pos.utxo, Redeemer: auctionBidRedeemer()}}
	spec.RequiredSigners = [][]byte{keyHash(bidder.Key)}
	spec.SigningKeys = []*UTxOKey{bidder.Key}
	spec.TTL = env.Slot + auctionBidTTLSlots
	return a.submitSpend(env, params, pos, auctionBid, spec, inputs, collateral, bidder, newBid)
}

// end settles the auction. With bids the item goes to the winner and the
// seller is paid the winning bid, the actor funding the fee and the item's
// output; without them the item returns to the seller, who pays the fee out
// of the locked value.
func (a *auctionApp) end(env *AppEnv, params ScriptParams, pos *auctionPosition) (bool, error) {
	actor := env.User
	spec := ScriptTxSpec{
		Validator:    a.validator,
		CostModel:    params.CostModel,
		ScriptInputs: []ScriptInput{{UTxO: pos.utxo, Redeemer: auctionEndRedeemer()}},
		// The validator wants a lower bound strictly after the expiration;
		// the bound is the current slot, which claim already checked.
		ValidityStart: env.Slot,
	}
	item := a.itemOf(pos.assetName)
	var inputs []UTxO
	if pos.bidder == nil {
		spec.Outputs = []ScriptOutput{
			{Address: pos.seller.Key.Address, Lovelace: auctionItemLovelace, Assets: item},
			{Address: pos.seller.Key.Address, Lovelace: pos.lovelace - auctionItemLovelace},
		}
		spec.ChangeIndex = 1
		spec.RequiredSigners = [][]byte{keyHash(pos.seller.Key)}
		spec.SigningKeys = []*UTxOKey{pos.seller.Key}
	} else {
		var change uint64
		var err error
		inputs, change, err = env.Wallet.SelectCoinsForKey(actor.Key, auctionItemLovelace+auctionFeeBudget)
		if errors.Is(err, ErrInsufficientFunds) {
			a.settle(pos, false, nil, false, false)
			return false, nil
		}
		if err != nil {
			a.settle(pos, false, nil, false, false)
			return false, err
		}
		// The seller's payment comes first: the validator reads the first
		// output at the seller's address, and the seller may also be the
		// actor whose change follows.
		spec.Outputs = []ScriptOutput{
			{Address: pos.seller.Key.Address, Lovelace: pos.lovelace},
			{Address: pos.bidder.Key.Address, Lovelace: auctionItemLovelace, Assets: item},
			{Address: actor.Key.Address, Lovelace: change + auctionFeeBudget},
		}
		spec.ChangeIndex = 2
		spec.WalletInputs = inputs
	}
	collateral, ok, err := a.collateralFor(env, params, inputs)
	if !ok {
		a.settle(pos, false, nil, false, false)
		return false, err
	}
	spec.Collateral = collateral
	return a.submitSpend(env, params, pos, auctionEnd, spec, inputs, collateral, actor, 0)
}

// submitSpend plans, submits and books a spend of pos. For a bid, newBid is
// the amount that makes the next position.
func (a *auctionApp) submitSpend(
	env *AppEnv, params ScriptParams, pos *auctionPosition, action auctionAction,
	spec ScriptTxSpec, inputs, collateral []UTxO, actor *User, newBid int64,
) (bool, error) {
	held := append(append([]UTxO(nil), inputs...), collateral...)
	known := append(append([]KnownUTxO(nil), pos.known...), walletKnown(inputs, collateral)...)
	txBytes, txID, err := PlanScriptTx(spec, NewVerifier(params), params, known, env.Slot)
	if err != nil {
		env.Wallet.ReturnUTxOs(held)
		a.settle(pos, false, nil, true, false)
		return false, fmt.Errorf("auction %s: %w", action, err)
	}
	if err := env.Submit("auction", txID, txBytes); err != nil {
		env.Wallet.ReturnUTxOs(held)
		a.settle(pos, false, nil, true, namesMissingInput(err, pos.utxo.TxHash))
		env.Logger.Warn("tx rejected", "action", action.String(), "tx_id", txID, "err", err)
		return false, nil
	}
	outs, err := KnownUTxOsFromTx(txBytes)
	if err != nil {
		env.Wallet.ReturnUTxOs(collateral)
		a.settle(pos, true, nil, false, false)
		return false, err
	}
	delay := env.Cfg.confirmationDelay()
	// Collateral is not consumed, but it stays reserved with the pending
	// transaction: spent elsewhere first, it would make a node reject this
	// transaction after the position was settled as spent. Reconciliation
	// releases it once the transaction has settled.
	env.Wallet.RecordAccepted(txID, held, a.walletOutputs(txID, outs, pos.seller, pos.bidder, actor), delay)

	var next *auctionPosition
	if action == auctionBid {
		next = &auctionPosition{
			utxo:        UTxO{TxHash: txID, Index: 0, Amount: uint64(newBid)}, //nolint:gosec // positive
			known:       outs,
			seller:      pos.seller,
			bidder:      actor,
			bid:         newBid,
			expiration:  pos.expiration,
			lovelace:    uint64(newBid), //nolint:gosec // positive
			assetName:   pos.assetName,
			availableAt: a.now().Add(delay),
		}
	}
	a.settle(pos, true, next, false, false)
	env.Logger.Info("auction step", "action", action.String(), "tx_id", txID)
	return true, nil
}
