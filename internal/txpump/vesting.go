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

	"github.com/blinklabs-io/plutigo/data"
)

func init() { RegisterApp("vesting", mustNewVestingApp()) }

const (
	vestingAmountMin = 3_000_000
	vestingAmountMax = 6_000_000
	// vestingLockMinSlots and vestingLockMaxSlots bound how long after
	// locking a position stays closed to its beneficiary.
	vestingLockMinSlots = 30
	vestingLockMaxSlots = 120
	// vestingFeeBudget is the wallet value selected beyond what a lock pays
	// out, covering the fee and a change output.
	vestingFeeBudget uint64 = 3_000_000
	// vestingCollateral is the value pledged as collateral, far above 150%
	// of a withdraw or clawback fee.
	vestingCollateral uint64 = 5_000_000
	// vestingMaxRejections bounds how often a position may fail to be spent
	// before it is abandoned, so a position whose creating transaction never
	// reached the chain, rejected with an unrecognised message, is not
	// retried forever.
	vestingMaxRejections = 3
)

type vestingAction int

const (
	vestingLock vestingAction = iota
	vestingWithdraw
	vestingClawback
)

func (a vestingAction) String() string {
	return [...]string{"lock", "withdraw", "clawback"}[a]
}

// vestingPosition is one locked script UTxO this process created.
type vestingPosition struct {
	utxo        UTxO
	known       []KnownUTxO // outputs of the transaction that created it
	owner       *User
	beneficiary *User
	lovelace    uint64
	// lockUntil is POSIX milliseconds, as in the datum.
	lockUntil int64
	// availableAt keeps a new position unspendable until its creating
	// transaction has had the confirmation window to reach the chain.
	availableAt time.Time
	// busy marks a position claimed by a step in progress.
	busy         bool
	rejections   int
	pendingSpend string
	absentRounds int
}

// vestingApp locks ADA for a beneficiary through the vesting template: the
// beneficiary withdraws once the lock has passed, and the owner may claw the
// value back at any time.
type vestingApp struct {
	validator PlutusV3Validator
	// choose returns a value in [lo, hi]; IntRange outside tests.
	choose func(lo, hi int) int
	now    func() time.Time

	mu        sync.Mutex
	positions []*vestingPosition
}

func newVestingApp() (*vestingApp, error) {
	v, err := LoadValidator("vesting.json", "vesting.vesting.spend")
	if err != nil {
		return nil, err
	}
	return &vestingApp{validator: v, choose: IntRange, now: time.Now}, nil
}

// mustNewVestingApp panics only if the embedded blueprint is unusable, which
// is a build-time defect.
func mustNewVestingApp() *vestingApp {
	a, err := newVestingApp()
	if err != nil {
		panic(fmt.Sprintf("txpump: vesting blueprint: %v", err))
	}
	return a
}

// Enabled reports whether there is a second user to be the beneficiary.
func (a *vestingApp) Enabled(users int) bool { return users >= 2 }

// slotMillis is the POSIX time in milliseconds at which slot starts, the
// value the script sees as a transaction's inclusive lower bound.
func slotMillis(p ScriptParams, slot uint64) int64 {
	return p.SystemStart.Add(time.Duration(slot) * p.SlotLength).UnixMilli() //nolint:gosec // devnet slots are small
}

// firstSlotAfter returns the earliest nonzero slot at or below upper whose
// start time is strictly after millis. Slot zero represents an omitted lower
// bound in ScriptTxSpec. Callers ensure upper itself meets the condition.
func firstSlotAfter(p ScriptParams, upper uint64, millis int64) uint64 {
	low := uint64(1)
	for low < upper {
		mid := low + (upper-low)/2
		if slotMillis(p, mid) > millis {
			upper = mid
		} else {
			low = mid + 1
		}
	}
	return low
}

func (a *vestingApp) Step(env *AppEnv) (bool, error) {
	if len(env.Cfg.PlutusV3CostModel) == 0 {
		return false, nil
	}
	if !scriptAppsStarted(env.Cfg, env.Slot) {
		return false, nil
	}
	params := scriptParamsFor(env.Cfg)
	// Without the real system start the lock time written to a datum and
	// the time a node derives from a slot would disagree.
	if params.SystemStart.IsZero() || params.SlotLength <= 0 {
		return false, nil
	}
	nowMs := slotMillis(params, env.Slot)
	pos, action := a.claim(env.User, nowMs)
	if pos == nil {
		return a.lock(env, params)
	}
	return a.spend(env, params, pos, action)
}

// claim picks the action for user and, for any action but lock, marks the
// position it acts on busy so that no concurrent step can spend it. A
// beneficiary can only claim once nowMs is past the lock; the owner can
// always claw back.
func (a *vestingApp) claim(user *User, nowMs int64) (*vestingPosition, vestingAction) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.choose(0, 3) == 0 {
		return nil, vestingLock
	}
	now := a.now()
	var claimable []*vestingPosition
	for _, p := range a.positions {
		if p.busy || p.pendingSpend != "" || now.Before(p.availableAt) {
			continue
		}
		if sameUser(p.owner, user) || (sameUser(p.beneficiary, user) && nowMs > p.lockUntil) {
			claimable = append(claimable, p)
		}
	}
	if len(claimable) == 0 {
		return nil, vestingLock
	}
	p := claimable[a.choose(0, len(claimable)-1)]
	action := vestingWithdraw
	if sameUser(p.owner, user) {
		if a.choose(0, 3) != 0 {
			return nil, vestingLock
		}
		action = vestingClawback
	}
	p.busy = true
	return p, action
}

// settle releases a claimed position. A spent or dropped position is
// removed, and a failed attempt counts toward vestingMaxRejections.
func (a *vestingApp) settle(p *vestingPosition, spent, failed, drop bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	p.busy = false
	if failed {
		p.rejections++
		if p.rejections >= vestingMaxRejections {
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
}

func (a *vestingApp) add(p *vestingPosition) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.positions = append(a.positions, p)
}

// ReconcileSnapshot refreshes positions from the validator's current UTxOs.
// A submitted spend remains reserved while it is in the mempool. If it
// disappears while its input remains unspent, the position becomes claimable
// again; if the input is gone, the spend is confirmed.
func (a *vestingApp) ReconcileSnapshot(
	snapshot []UTxO,
	users []*User,
	presence map[string]bool,
) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	onChain := make(map[string]UTxO, len(snapshot))
	for _, utxo := range snapshot {
		onChain[utxoKey(utxo)] = utxo
	}

	positions := make([]*vestingPosition, 0, len(a.positions)+len(snapshot))
	known := make(map[string]struct{}, len(a.positions)+len(snapshot))
	for _, pos := range a.positions {
		key := utxoKey(pos.utxo)
		utxo, exists := onChain[key]
		if pos.pendingSpend != "" {
			if presence[pos.pendingSpend] {
				pos.absentRounds = 0
				pos.busy = false
				positions = append(positions, pos)
				known[key] = struct{}{}
				continue
			}
			if !exists {
				continue
			}
			pos.absentRounds++
			if pos.absentRounds < 2 {
				pos.busy = false
				positions = append(positions, pos)
				known[key] = struct{}{}
				continue
			}
			pos.pendingSpend = ""
			pos.absentRounds = 0
			pos.busy = false
		}
		if !exists && !presence[pos.utxo.TxHash] {
			continue
		}
		if exists {
			pos.utxo = utxo
		}
		positions = append(positions, pos)
		known[key] = struct{}{}
	}

	var decodeErr error
	for _, utxo := range snapshot {
		key := utxoKey(utxo)
		if _, exists := known[key]; exists {
			continue
		}
		pos, err := a.positionFromUTxO(utxo, users)
		if err != nil {
			if decodeErr == nil {
				decodeErr = fmt.Errorf("vesting output %s: %w", key, err)
			}
			continue
		}
		if pos != nil {
			positions = append(positions, pos)
			known[key] = struct{}{}
		}
	}
	a.positions = positions
	return decodeErr
}

func (a *vestingApp) positionFromUTxO(
	utxo UTxO,
	users []*User,
) (*vestingPosition, error) {
	if len(utxo.InlineDatum) == 0 {
		return nil, fmt.Errorf("missing inline datum")
	}
	decoded, err := data.Decode(utxo.InlineDatum)
	if err != nil {
		return nil, fmt.Errorf("decode datum: %w", err)
	}
	datum, ok := decoded.(*data.Constr)
	if !ok || datum.Tag == nil || !datum.Tag.IsUint64() || datum.Tag.Uint64() != 0 || len(datum.Fields) != 3 {
		return nil, fmt.Errorf("datum is not a vesting record")
	}
	lock, ok := datum.Fields[0].(*data.Integer)
	if !ok || lock.Inner == nil || !lock.Inner.IsInt64() {
		return nil, fmt.Errorf("invalid lock deadline")
	}
	ownerHash, ok := datum.Fields[1].(*data.ByteString)
	if !ok || len(ownerHash.Inner) != 28 {
		return nil, fmt.Errorf("invalid owner credential")
	}
	beneficiaryHash, ok := datum.Fields[2].(*data.ByteString)
	if !ok || len(beneficiaryHash.Inner) != 28 {
		return nil, fmt.Errorf("invalid beneficiary credential")
	}
	var owner, beneficiary *User
	for _, user := range users {
		if user == nil || user.Key == nil {
			continue
		}
		credential := keyHash(user.Key)
		if bytes.Equal(credential, ownerHash.Inner) {
			owner = user
		}
		if bytes.Equal(credential, beneficiaryHash.Inner) {
			beneficiary = user
		}
	}
	if owner == nil || beneficiary == nil {
		return nil, nil
	}
	known := KnownUTxO{
		Ref:      utxoKey(utxo),
		Address:  a.validator.Address(),
		Lovelace: utxo.Amount,
		Datum:    append([]byte(nil), utxo.InlineDatum...),
	}
	return &vestingPosition{
		utxo:        utxo,
		known:       []KnownUTxO{known},
		owner:       owner,
		beneficiary: beneficiary,
		lovelace:    utxo.Amount,
		lockUntil:   lock.Inner.Int64(),
	}, nil
}

func (a *vestingApp) markPendingSpend(pos *vestingPosition, txID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	pos.pendingSpend = txID
	pos.absentRounds = 0
	pos.busy = false
}

func (a *vestingApp) lock(env *AppEnv, params ScriptParams) (bool, error) {
	owner := env.User
	var others []*User
	for _, u := range env.Users {
		if !sameUser(u, owner) {
			others = append(others, u)
		}
	}
	if len(others) == 0 {
		return false, nil
	}
	//nolint:gosec // bounded by small constants
	amount := uint64(a.choose(vestingAmountMin, vestingAmountMax))
	lockSlots := a.choose(vestingLockMinSlots, vestingLockMaxSlots)
	beneficiary := others[a.choose(0, len(others)-1)]
	lockUntil := slotMillis(params, env.Slot+uint64(lockSlots))

	inputs, change, err := env.Wallet.SelectCoinsForKey(owner.Key, amount+vestingFeeBudget)
	if errors.Is(err, ErrInsufficientFunds) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	txBytes, txID, changeOut, err := buildEscrowLock(
		inputs, a.validator.Address(), amount,
		vestingDatum(lockUntil, keyHash(owner.Key), keyHash(beneficiary.Key)),
		owner.Key.Address, change+vestingFeeBudget, params,
	)
	if err == nil {
		_, err = NewVerifier(params).Verify(txBytes, walletKnown(inputs), env.Slot)
	}
	if err != nil {
		env.Wallet.ReturnUTxOs(inputs)
		return false, fmt.Errorf("vesting lock: %w", err)
	}
	if err := env.Submit("vesting", txID, txBytes); err != nil {
		env.Wallet.ReturnUTxOs(inputs)
		env.Logger.Debug("tx rejected", "action", "lock", "tx_id", txID, "err", err)
		return false, nil
	}
	known, err := KnownUTxOsFromTx(txBytes)
	if err != nil {
		return false, err
	}
	delay := env.Cfg.confirmationDelay()
	a.add(&vestingPosition{
		utxo:        UTxO{TxHash: txID, Index: 0, Amount: amount},
		known:       known,
		owner:       owner,
		beneficiary: beneficiary,
		lovelace:    amount,
		lockUntil:   lockUntil,
		availableAt: a.now().Add(delay),
	})
	env.Wallet.RecordAccepted(txID, inputs, []UTxO{{
		TxHash: txID, Index: 1, Amount: changeOut, SigningKey: owner.Key,
	}}, delay)
	env.Logger.Debug("vesting step", "action", "lock", "tx_id", txID, "lovelace", amount, "lock_until", lockUntil)
	return true, nil
}

// spend withdraws (as the beneficiary) or claws back (as the owner) the whole
// position to the actor, paying the fee out of the locked value so the actor
// needs only collateral.
func (a *vestingApp) spend(env *AppEnv, params ScriptParams, pos *vestingPosition, action vestingAction) (bool, error) {
	actor := env.User
	spec := ScriptTxSpec{
		Validator:       a.validator,
		CostModel:       params.CostModel,
		ScriptInputs:    []ScriptInput{{UTxO: pos.utxo, Redeemer: vestingRedeemer()}},
		Outputs:         []ScriptOutput{{Address: actor.Key.Address, Lovelace: pos.lovelace}},
		RequiredSigners: [][]byte{keyHash(actor.Key)},
		SigningKeys:     []*UTxOKey{actor.Key},
		ChangeIndex:     0,
	}
	if action == vestingWithdraw {
		// The validator wants a lower bound strictly after lock_until; the
		// earliest valid slot avoids asking the mempool to accept a wall-clock
		// slot that is still ahead of the node tip.
		spec.ValidityStart = firstSlotAfter(params, env.Slot, pos.lockUntil)
	}
	collateral, _, err := env.Wallet.SelectCoinsForKey(actor.Key, vestingCollateral)
	if err != nil || uint(len(collateral)) > params.MaxCollateralInputs { //nolint:gosec // small slice
		env.Wallet.ReturnUTxOs(collateral)
		a.settle(pos, false, false, false)
		if err != nil && !errors.Is(err, ErrInsufficientFunds) {
			return false, err
		}
		return false, nil
	}
	spec.Collateral = collateral

	known := append(append([]KnownUTxO(nil), pos.known...), walletKnown(collateral)...)
	txBytes, txID, err := PlanScriptTx(spec, NewVerifier(params), params, known, env.Slot)
	if err != nil {
		env.Wallet.ReturnUTxOs(collateral)
		a.settle(pos, false, true, false)
		return false, fmt.Errorf("vesting %s: %w", action, err)
	}
	outs, err := KnownUTxOsFromTx(txBytes)
	if err != nil {
		env.Wallet.ReturnUTxOs(collateral)
		a.settle(pos, false, true, false)
		return false, err
	}
	if err := env.Submit("vesting", txID, txBytes); err != nil {
		env.Wallet.ReturnUTxOs(collateral)
		a.settle(pos, false, true, namesMissingInput(err, pos.utxo.TxHash))
		env.Logger.Debug("tx rejected", "action", action.String(), "tx_id", txID, "err", err)
		return false, nil
	}
	// Collateral is not consumed, but it stays reserved with the pending
	// transaction: spent elsewhere first, it would make a node reject this
	// transaction after the position was settled as spent. Reconciliation
	// releases it once the transaction has settled.
	env.Wallet.RecordAccepted(txID, collateral, []UTxO{{
		TxHash: txID, Index: 0, Amount: outs[0].Lovelace, SigningKey: actor.Key,
	}}, env.Cfg.confirmationDelay())
	a.markPendingSpend(pos, txID)
	env.Logger.Debug("vesting step", "action", action.String(), "tx_id", txID)
	return true, nil
}
