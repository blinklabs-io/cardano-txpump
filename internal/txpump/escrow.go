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
	"strings"
	"sync"
	"time"

	"github.com/blinklabs-io/gouroboros/cbor"
	"github.com/blinklabs-io/gouroboros/ledger/common"
	"github.com/blinklabs-io/plutigo/data"
)

func init() { RegisterApp("escrow", mustNewEscrowApp()) }

const (
	escrowAmountMin = 3_000_000
	escrowAmountMax = 6_000_000
	// escrowFeeBudget is the wallet value selected beyond what a step pays
	// out, covering the fee and a change output.
	escrowFeeBudget uint64 = 3_000_000
	// escrowCollateral is the value pledged as collateral, far above 150%
	// of any escrow step's fee.
	escrowCollateral uint64 = 5_000_000
	// escrowMaxRejections bounds how often a position may fail to be spent
	// before it is abandoned, so a position whose creating transaction never
	// reached the chain, rejected with an unrecognised message, is not
	// retried forever.
	escrowMaxRejections = 3
	// escrowFromEpoch matches the plutus workload's gate.
	escrowFromEpoch = 3
)

type escrowState int

const (
	escrowInitiated escrowState = iota + 1 // Initiation datum on chain, no recipient yet
	escrowActive                           // ActiveEscrow datum on chain
)

type escrowAction int

const (
	escrowInitiate escrowAction = iota
	escrowDeposit
	escrowComplete
	escrowCancel
)

func (a escrowAction) String() string {
	return [...]string{"initiate", "deposit", "complete", "cancel"}[a]
}

// escrowPosition is one open escrow: a script UTxO this process created.
type escrowPosition struct {
	state        escrowState
	utxo         UTxO        // the script UTxO
	known        []KnownUTxO // outputs of the transaction that created it
	initiator    *User
	recipient    *User // nil until active
	initLovelace uint64
	recLovelace  uint64
	// availableAt keeps a new position unspendable until its creating
	// transaction has had the confirmation window to reach the chain.
	availableAt time.Time
	// busy marks a position claimed by a step in progress; only the
	// claiming step may spend it.
	busy       bool
	rejections int
}

func sameUser(a, b *User) bool {
	return a != nil && b != nil && bytes.Equal(a.Key.Address, b.Key.Address)
}

func (p *escrowPosition) isParty(u *User) bool {
	return sameUser(p.initiator, u) || sameUser(p.recipient, u)
}

// escrowApp trades ADA between two users through the escrow template:
// initiate, deposit, then complete or cancel.
type escrowApp struct {
	validator PlutusV3Validator
	// choose returns a value in [lo, hi]; IntRange outside tests.
	choose func(lo, hi int) int
	now    func() time.Time

	mu        sync.Mutex
	positions []*escrowPosition
}

func newEscrowApp() (*escrowApp, error) {
	v, err := LoadValidator("escrow.json", "escrow.escrow.spend")
	if err != nil {
		return nil, err
	}
	return &escrowApp{validator: v, choose: IntRange, now: time.Now}, nil
}

// mustNewEscrowApp panics only if the embedded blueprint is unusable, which
// is a build-time defect.
func mustNewEscrowApp() *escrowApp {
	a, err := newEscrowApp()
	if err != nil {
		panic(fmt.Sprintf("txpump: escrow blueprint: %v", err))
	}
	return a
}

// ReferenceValidators lists the validators the app can spend through a
// published reference script.
func (a *escrowApp) ReferenceValidators() []PlutusV3Validator {
	return []PlutusV3Validator{a.validator}
}

// Enabled reports whether there is a second user to trade with.
func (a *escrowApp) Enabled(users int) bool { return users >= 2 }

func (a *escrowApp) Step(env *AppEnv) (bool, error) {
	if len(env.Cfg.PlutusV3CostModel) == 0 {
		return false, nil
	}
	epochLength := env.Cfg.EpochLength
	if epochLength == 0 {
		epochLength = 500
	}
	if env.Slot/epochLength < escrowFromEpoch {
		return false, nil
	}
	params := scriptParamsFor(env.Cfg)
	pos, action := a.claim(env.User)
	if pos == nil {
		return a.initiate(env, params)
	}
	return a.spend(env, params, pos, action)
}

func scriptParamsFor(cfg *Config) ScriptParams {
	p := cfg.ScriptParams
	if p.MinFeeA == 0 && p.MinFeeB == 0 {
		p = DefaultScriptParams(nil)
		p.SlotLength = cfg.SlotLength
	}
	if len(p.CostModel) == 0 {
		p.CostModel = cfg.PlutusV3CostModel
	}
	return p
}

// claim picks the action for user and, for any action but initiate, marks
// the position it acts on busy so that no concurrent step can spend the same
// script UTxO.
func (a *escrowApp) claim(user *User) (*escrowPosition, escrowAction) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.choose(0, 3) == 0 {
		return nil, escrowInitiate
	}
	now := a.now()
	var claimable []*escrowPosition
	for _, p := range a.positions {
		if p.busy || now.Before(p.availableAt) {
			continue
		}
		if p.state == escrowInitiated || p.isParty(user) {
			claimable = append(claimable, p)
		}
	}
	if len(claimable) == 0 {
		return nil, escrowInitiate
	}
	p := claimable[a.choose(0, len(claimable)-1)]
	proceed := a.choose(0, 3) != 0
	var action escrowAction
	switch {
	case p.state == escrowInitiated && sameUser(p.initiator, user):
		if proceed {
			return nil, escrowInitiate
		}
		action = escrowCancel
	case p.state == escrowInitiated:
		action = escrowDeposit
	case proceed:
		action = escrowComplete
	default:
		action = escrowCancel
	}
	p.busy = true
	return p, action
}

// settle releases a claimed position. A spent or dropped position is
// removed, next (the position a deposit creates) is added, and a failed
// attempt counts toward escrowMaxRejections.
func (a *escrowApp) settle(p *escrowPosition, spent bool, next *escrowPosition, failed, drop bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	p.busy = false
	if failed {
		p.rejections++
		if p.rejections >= escrowMaxRejections {
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

func (a *escrowApp) add(p *escrowPosition) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.positions = append(a.positions, p)
}

// namesMissingInput reports whether a node rejection says the script input
// is unknown, meaning the transaction that created the position never
// reached the ledger (or the output was already spent).
func namesMissingInput(err error, txHash string) bool {
	msg := strings.ToLower(err.Error())
	if !strings.Contains(msg, strings.ToLower(txHash)) {
		return false
	}
	for _, m := range []string{"badinputs", "bad input", "missing input", "unknown input", "not found", "does not exist"} {
		if strings.Contains(msg, m) {
			return true
		}
	}
	return false
}

func walletKnown(utxos ...[]UTxO) []KnownUTxO {
	var known []KnownUTxO
	for _, set := range utxos {
		for _, u := range set {
			addr := u.Address
			if len(addr) == 0 && u.SigningKey != nil {
				addr = u.SigningKey.Address
			}
			known = append(known, SyntheticWalletUTxO(u, addr))
		}
	}
	return known
}

func (a *escrowApp) initiate(env *AppEnv, params ScriptParams) (bool, error) {
	user := env.User
	//nolint:gosec // bounded by small constants
	amount := uint64(a.choose(escrowAmountMin, escrowAmountMax))
	userAddr, err := plutusAddressData(user.Key.Address)
	if err != nil {
		return false, fmt.Errorf("escrow initiate: %w", err)
	}
	inputs, change, err := env.Wallet.SelectCoinsForKey(user.Key, amount+escrowFeeBudget)
	if errors.Is(err, ErrInsufficientFunds) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	txBytes, txID, changeOut, err := buildEscrowLock(
		inputs, a.validator.Address(), amount,
		escrowInitiationDatum(userAddr, amount),
		user.Key.Address, change+escrowFeeBudget, params,
	)
	if err == nil {
		_, err = NewVerifier(params).Verify(txBytes, walletKnown(inputs), env.Slot)
	}
	if err != nil {
		env.Wallet.ReturnUTxOs(inputs)
		return false, fmt.Errorf("escrow initiate: %w", err)
	}
	if err := env.Submit("escrow", txID, txBytes); err != nil {
		env.Wallet.ReturnUTxOs(inputs)
		env.Logger.Warn("tx rejected", "action", "initiate", "tx_id", txID, "err", err)
		return false, nil
	}
	known, err := KnownUTxOsFromTx(txBytes)
	if err != nil {
		return false, err
	}
	delay := env.Cfg.confirmationDelay()
	a.add(&escrowPosition{
		state:        escrowInitiated,
		utxo:         UTxO{TxHash: txID, Index: 0, Amount: amount},
		known:        known,
		initiator:    user,
		initLovelace: amount,
		availableAt:  a.now().Add(delay),
	})
	env.Wallet.RecordAccepted(txID, inputs, []UTxO{{
		TxHash: txID, Index: 1, Amount: changeOut, SigningKey: user.Key,
	}}, delay)
	env.Logger.Info("escrow step", "action", "initiate", "tx_id", txID, "lovelace", amount)
	return true, nil
}

// buildEscrowLock builds the key-signed transaction that locks amount at
// scriptAddr with datum, returning change (before the fee) to changeAddr. It
// returns the change actually paid after the fee.
func buildEscrowLock(
	inputs []UTxO,
	scriptAddr []byte,
	amount uint64,
	datum data.PlutusData,
	changeAddr []byte,
	change uint64,
	params ScriptParams,
) ([]byte, string, uint64, error) {
	inputSet, _, err := plutusInputs("escrow_lock", inputs)
	if err != nil {
		return nil, "", 0, err
	}
	datumBytes, err := encodePlutusData(datum)
	if err != nil {
		return nil, "", 0, err
	}
	keys := make([]*UTxOKey, 0, len(inputs))
	for _, u := range inputs {
		keys = append(keys, u.SigningKey)
	}
	fee := MinFee
	for range 4 {
		if change < fee || change-fee < minSendAmount {
			return nil, "", 0, fmt.Errorf("escrow_lock: change %d cannot cover fee %d", change, fee)
		}
		body := txBodyPlutus{
			Inputs: inputSet,
			Outputs: []plutusOutput{
				{
					Address:     scriptAddr,
					Amount:      amount,
					DatumOption: []any{uint64(1), cbor.Tag{Number: 24, Content: datumBytes}},
				},
				{Address: changeAddr, Amount: change - fee},
			},
			Fee: fee,
		}
		txBytes, err := encodePlutusTx("escrow_lock", body, map[any]any{}, keys)
		if err != nil {
			return nil, "", 0, err
		}
		need := minFeeFor(len(txBytes), common.ExUnits{}, params, 0)
		if need <= fee {
			bodyBytes, err := cbor.Encode(body)
			if err != nil {
				return nil, "", 0, err
			}
			return txBytes, common.Blake2b256Hash(bodyBytes).String(), change - fee, nil
		}
		fee = need * 11 / 10
	}
	return nil, "", 0, errors.New("escrow_lock: fee did not converge")
}

func (a *escrowApp) spend(env *AppEnv, params ScriptParams, pos *escrowPosition, action escrowAction) (bool, error) {
	actor := env.User
	initAddr, err := plutusAddressData(pos.initiator.Key.Address)
	if err != nil {
		a.settle(pos, false, nil, true, false)
		return false, fmt.Errorf("escrow %s: %w", action, err)
	}
	spec := ScriptTxSpec{
		Validator: a.validator,
		CostModel: params.CostModel,
	}
	var pay uint64 // what the actor's wallet pays out beyond the fee
	var recLovelace uint64
	var recipient *User
	switch action {
	case escrowDeposit:
		recipient = actor
		//nolint:gosec // bounded by small constants
		recLovelace = uint64(a.choose(escrowAmountMin, escrowAmountMax))
		recAddr, err := plutusAddressData(actor.Key.Address)
		if err != nil {
			a.settle(pos, false, nil, true, false)
			return false, fmt.Errorf("escrow deposit: %w", err)
		}
		pay = recLovelace
		spec.ScriptInputs = []ScriptInput{{UTxO: pos.utxo, Redeemer: escrowDepositRedeemer(recAddr, recLovelace)}}
		spec.Outputs = []ScriptOutput{{
			Address:  a.validator.Address(),
			Lovelace: pos.initLovelace + recLovelace,
			Datum:    escrowActiveDatum(initAddr, pos.initLovelace, recAddr, recLovelace),
		}}
	case escrowComplete:
		spec.ScriptInputs = []ScriptInput{{UTxO: pos.utxo, Redeemer: escrowCompleteRedeemer()}}
		// The trade crosses: each party receives the other's deposit, and
		// both must sign.
		spec.Outputs = []ScriptOutput{
			{Address: pos.initiator.Key.Address, Lovelace: pos.recLovelace},
			{Address: pos.recipient.Key.Address, Lovelace: pos.initLovelace},
		}
		spec.RequiredSigners = [][]byte{keyHash(pos.initiator.Key), keyHash(pos.recipient.Key)}
		spec.SigningKeys = []*UTxOKey{pos.initiator.Key, pos.recipient.Key}
	case escrowCancel:
		spec.ScriptInputs = []ScriptInput{{UTxO: pos.utxo, Redeemer: escrowCancelRedeemer()}}
		spec.RequiredSigners = [][]byte{keyHash(actor.Key)}
		spec.SigningKeys = []*UTxOKey{actor.Key}
		spec.Outputs = []ScriptOutput{{Address: pos.initiator.Key.Address, Lovelace: pos.initLovelace}}
		if pos.state == escrowActive {
			spec.Outputs = append(spec.Outputs, ScriptOutput{
				Address: pos.recipient.Key.Address, Lovelace: pos.recLovelace,
			})
		}
	default:
		a.settle(pos, false, nil, false, false)
		return false, fmt.Errorf("escrow: unexpected action %s", action)
	}

	// Reclaiming an Initiation pays the fee out of the locked value; every
	// other step funds it, and any deposit, from the actor's wallet.
	var inputs []UTxO
	if action == escrowCancel && pos.state == escrowInitiated {
		spec.ChangeIndex = 0
	} else {
		var change uint64
		inputs, change, err = env.Wallet.SelectCoinsForKey(actor.Key, pay+escrowFeeBudget)
		if errors.Is(err, ErrInsufficientFunds) {
			a.settle(pos, false, nil, false, false)
			return false, nil
		}
		if err != nil {
			a.settle(pos, false, nil, false, false)
			return false, err
		}
		spec.WalletInputs = inputs
		spec.Outputs = append(spec.Outputs, ScriptOutput{
			Address: actor.Key.Address, Lovelace: change + escrowFeeBudget,
		})
		spec.ChangeIndex = len(spec.Outputs) - 1
	}
	collateral, _, err := env.Wallet.SelectCoinsForKey(actor.Key, escrowCollateral)
	if err != nil || uint(len(collateral)) > params.MaxCollateralInputs { //nolint:gosec // small slice
		env.Wallet.ReturnUTxOs(append(inputs, collateral...))
		a.settle(pos, false, nil, false, false)
		if err != nil && !errors.Is(err, ErrInsufficientFunds) {
			return false, err
		}
		return false, nil
	}
	spec.Collateral = collateral

	known := append(append([]KnownUTxO(nil), pos.known...), walletKnown(inputs, collateral)...)
	txBytes, txID, err := PlanScriptTx(spec, NewVerifier(params), params, known, env.Slot)
	if err != nil {
		env.Wallet.ReturnUTxOs(append(inputs, collateral...))
		a.settle(pos, false, nil, true, false)
		return false, fmt.Errorf("escrow %s: %w", action, err)
	}
	if err := env.Submit("escrow", txID, txBytes); err != nil {
		env.Wallet.ReturnUTxOs(append(inputs, collateral...))
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
	var walletOuts []UTxO
	for i, o := range outs {
		for _, u := range []*User{pos.initiator, pos.recipient, actor} {
			if u != nil && bytes.Equal(o.Address, u.Key.Address) {
				walletOuts = append(walletOuts, UTxO{
					TxHash: txID, Index: uint32(i), Amount: o.Lovelace, SigningKey: u.Key, //nolint:gosec // few outputs
				})
				break
			}
		}
	}
	delay := env.Cfg.confirmationDelay()
	// Collateral is not consumed, but it stays reserved with the pending
	// transaction: spent elsewhere first, it would make a node reject this
	// transaction after the position was settled as spent. Reconciliation
	// releases it once the transaction has settled.
	env.Wallet.RecordAccepted(txID, append(inputs, collateral...), walletOuts, delay)

	var next *escrowPosition
	if action == escrowDeposit {
		next = &escrowPosition{
			state:        escrowActive,
			utxo:         UTxO{TxHash: txID, Index: 0, Amount: pos.initLovelace + recLovelace},
			known:        outs,
			initiator:    pos.initiator,
			recipient:    recipient,
			initLovelace: pos.initLovelace,
			recLovelace:  recLovelace,
			availableAt:  a.now().Add(delay),
		}
	}
	a.settle(pos, true, next, false, false)
	env.Logger.Info("escrow step", "action", action.String(), "tx_id", txID)
	return true, nil
}
