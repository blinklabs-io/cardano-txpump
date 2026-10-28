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
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/blinklabs-io/plutigo/data"
)

func init() { RegisterApp("simple-transfer", mustNewTransferApp()) }

const (
	transferAmountMin = 3_000_000
	transferAmountMax = 6_000_000
	// transferFeeBudget is the wallet value selected beyond what a lock pays
	// out, covering the fee and a change output.
	transferFeeBudget uint64 = 3_000_000
	// transferCollateral is the value pledged as collateral, far above 150%
	// of a claim fee.
	transferCollateral uint64 = 5_000_000
	// transferMaxRejections bounds how often a position may fail to be
	// claimed before it is abandoned.
	transferMaxRejections = 3
	// transferFromEpoch matches the plutus workload's gate.
	transferFromEpoch = 3
	transferTitle     = "simple_transfer.simpleTransfer.spend"
)

// transferPosition is one locked script UTxO this process created.
type transferPosition struct {
	utxo     UTxO
	known    []KnownUTxO // outputs of the transaction that created it
	receiver *User
	lovelace uint64
	// validator is the template applied to receiver's key hash: the script
	// the position is locked at.
	validator PlutusV3Validator
	// availableAt keeps a new position unspendable until its creating
	// transaction has had the confirmation window to reach the chain.
	availableAt time.Time
	// busy marks a position claimed by a step in progress.
	busy       bool
	rejections int
}

// transferApp locks ADA at the simple-transfer template applied to a
// receiver's key hash, which only a transaction signed by that receiver can
// spend. Every receiver has its own script and address, so the validator is
// parameterized when the app first meets a receiver.
type transferApp struct {
	template PlutusV3Validator
	choose   func(lo, hi int) int
	now      func() time.Time

	mu        sync.Mutex
	applied   map[string]PlutusV3Validator
	positions []*transferPosition
}

func newTransferApp() (*transferApp, error) {
	v, err := LoadValidator("simple-transfer.json", transferTitle)
	if err != nil {
		return nil, err
	}
	return &transferApp{
		template: v,
		choose:   IntRange,
		now:      time.Now,
		applied:  map[string]PlutusV3Validator{},
	}, nil
}

// mustNewTransferApp panics only if the embedded blueprint is unusable, which
// is a build-time defect.
func mustNewTransferApp() *transferApp {
	a, err := newTransferApp()
	if err != nil {
		panic(fmt.Sprintf("txpump: simple-transfer blueprint: %v", err))
	}
	return a
}

// Enabled reports whether there is a second user to be the receiver.
func (a *transferApp) Enabled(users int) bool { return users >= 2 }

// validatorFor returns the template applied to receiver's key hash.
func (a *transferApp) validatorFor(receiver *User) (PlutusV3Validator, error) {
	kh := keyHash(receiver.Key)
	id := hex.EncodeToString(kh)
	a.mu.Lock()
	defer a.mu.Unlock()
	if v, ok := a.applied[id]; ok {
		return v, nil
	}
	v, err := a.template.Apply(data.NewByteString(kh))
	if err != nil {
		return PlutusV3Validator{}, err
	}
	a.applied[id] = v
	return v, nil
}

func (a *transferApp) Step(env *AppEnv) (bool, error) {
	if len(env.Cfg.PlutusV3CostModel) == 0 {
		return false, nil
	}
	epochLength := env.Cfg.EpochLength
	if epochLength == 0 {
		epochLength = 500
	}
	if env.Slot/epochLength < transferFromEpoch {
		return false, nil
	}
	params := scriptParamsFor(env.Cfg)
	if pos := a.claim(env.User); pos != nil {
		return a.spend(env, params, pos)
	}
	return a.lock(env, params)
}

// claim returns a position the user is the receiver of and marks it busy so
// that no concurrent step can spend it, or nil to lock instead.
func (a *transferApp) claim(user *User) *transferPosition {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.choose(0, 3) == 0 {
		return nil
	}
	now := a.now()
	var claimable []*transferPosition
	for _, p := range a.positions {
		if !p.busy && !now.Before(p.availableAt) && sameUser(p.receiver, user) {
			claimable = append(claimable, p)
		}
	}
	if len(claimable) == 0 {
		return nil
	}
	p := claimable[a.choose(0, len(claimable)-1)]
	p.busy = true
	return p
}

// settle releases a claimed position. A spent or dropped position is
// removed, and a failed attempt counts toward transferMaxRejections.
func (a *transferApp) settle(p *transferPosition, spent, failed, drop bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	p.busy = false
	if failed {
		p.rejections++
		if p.rejections >= transferMaxRejections {
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

func (a *transferApp) add(p *transferPosition) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.positions = append(a.positions, p)
}

func (a *transferApp) lock(env *AppEnv, params ScriptParams) (bool, error) {
	sender := env.User
	var others []*User
	for _, u := range env.Users {
		if !sameUser(u, sender) {
			others = append(others, u)
		}
	}
	if len(others) == 0 {
		return false, nil
	}
	//nolint:gosec // bounded by small constants
	amount := uint64(a.choose(transferAmountMin, transferAmountMax))
	receiver := others[a.choose(0, len(others)-1)]
	validator, err := a.validatorFor(receiver)
	if err != nil {
		return false, fmt.Errorf("simple-transfer lock: %w", err)
	}

	inputs, change, err := env.Wallet.SelectCoinsForKey(sender.Key, amount+transferFeeBudget)
	if errors.Is(err, ErrInsufficientFunds) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	txBytes, txID, changeOut, err := buildEscrowLock(
		inputs, validator.Address(), amount, data.NewConstr(0),
		sender.Key.Address, change+transferFeeBudget, params,
	)
	if err == nil {
		_, err = NewVerifier(params).Verify(txBytes, walletKnown(inputs), env.Slot)
	}
	if err != nil {
		env.Wallet.ReturnUTxOs(inputs)
		return false, fmt.Errorf("simple-transfer lock: %w", err)
	}
	if err := env.Submit("simple-transfer", txID, txBytes); err != nil {
		env.Wallet.ReturnUTxOs(inputs)
		env.Logger.Warn("tx rejected", "action", "lock", "tx_id", txID, "err", err)
		return false, nil
	}
	known, err := KnownUTxOsFromTx(txBytes)
	if err != nil {
		return false, err
	}
	delay := env.Cfg.confirmationDelay()
	a.add(&transferPosition{
		utxo:        UTxO{TxHash: txID, Index: 0, Amount: amount},
		known:       known,
		receiver:    receiver,
		lovelace:    amount,
		validator:   validator,
		availableAt: a.now().Add(delay),
	})
	env.Wallet.RecordAccepted(txID, inputs, []UTxO{{
		TxHash: txID, Index: 1, Amount: changeOut, SigningKey: sender.Key,
	}}, delay)
	env.Logger.Info("simple-transfer step", "action", "lock", "tx_id", txID, "lovelace", amount)
	return true, nil
}

// spend pays the whole position to its receiver, taking the fee out of the
// locked value so the receiver needs only collateral.
func (a *transferApp) spend(env *AppEnv, params ScriptParams, pos *transferPosition) (bool, error) {
	actor := env.User
	spec := ScriptTxSpec{
		Validator:       pos.validator,
		CostModel:       params.CostModel,
		ScriptInputs:    []ScriptInput{{UTxO: pos.utxo, Redeemer: data.NewConstr(0)}},
		Outputs:         []ScriptOutput{{Address: actor.Key.Address, Lovelace: pos.lovelace}},
		RequiredSigners: [][]byte{keyHash(actor.Key)},
		SigningKeys:     []*UTxOKey{actor.Key},
		ChangeIndex:     0,
	}
	collateral, _, err := env.Wallet.SelectCoinsForKey(actor.Key, transferCollateral)
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
		return false, fmt.Errorf("simple-transfer claim: %w", err)
	}
	if err := env.Submit("simple-transfer", txID, txBytes); err != nil {
		env.Wallet.ReturnUTxOs(collateral)
		a.settle(pos, false, true, namesMissingInput(err, pos.utxo.TxHash))
		env.Logger.Warn("tx rejected", "action", "claim", "tx_id", txID, "err", err)
		return false, nil
	}
	outs, err := KnownUTxOsFromTx(txBytes)
	if err != nil {
		env.Wallet.ReturnUTxOs(collateral)
		a.settle(pos, true, false, false)
		return false, err
	}
	// Collateral is not consumed, but it stays reserved with the pending
	// transaction: spent elsewhere first, it would make a node reject this
	// transaction after the position was settled as spent.
	env.Wallet.RecordAccepted(txID, collateral, []UTxO{{
		TxHash: txID, Index: 0, Amount: outs[0].Lovelace, SigningKey: actor.Key,
	}}, env.Cfg.confirmationDelay())
	a.settle(pos, true, false, false)
	env.Logger.Info("simple-transfer step", "action", "claim", "tx_id", txID)
	return true, nil
}
