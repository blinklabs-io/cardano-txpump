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
	"encoding/hex"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/blinklabs-io/gouroboros/ledger/conway"
	"github.com/stretchr/testify/require"
)

const vestingTestStart = 3 * 500

// vestingHarness drives a vestingApp against a wallet of two users and checks
// every submitted transaction with an independent verifier against the
// outputs seen so far, as a node would.
type vestingHarness struct {
	t      *testing.T
	app    *vestingApp
	wallet *Wallet
	users  []*User
	cfg    *Config
	slot   uint64
	known  []KnownUTxO
	txs    [][]byte
	reject func(txBytes []byte) error
	picks  []int
	logs   bytes.Buffer
}

func newVestingHarness(t *testing.T) *vestingHarness {
	t.Helper()
	app, err := newVestingApp()
	require.NoError(t, err)
	h := &vestingHarness{t: t, app: app, wallet: NewWallet(), slot: vestingTestStart}
	for i, k := range []*UTxOKey{escrowTestKey(0x01), escrowTestKey(0x02)} {
		for j := 0; j < 4; j++ {
			u := UTxO{
				TxHash:     strings.Repeat(hex.EncodeToString([]byte{byte(0x10*(i+1) + j)}), 32),
				Amount:     50_000_000,
				SigningKey: k,
			}
			h.wallet.Add(u)
			h.known = append(h.known, SyntheticWalletUTxO(u, k.Address))
		}
	}
	h.users = usersFromWallet(h.wallet)
	cm := testCostModel(t)
	params := DefaultScriptParams(cm)
	params.SystemStart = time.Unix(1_700_000_000, 0)
	h.cfg = &Config{
		PlutusV3CostModel: cm, ScriptParams: params, EpochLength: 500,
		SlotLength: time.Second, SystemStartUnix: 1_700_000_000,
	}
	app.choose = func(lo, hi int) int {
		if len(h.picks) == 0 {
			return lo
		}
		v := h.picks[0]
		h.picks = h.picks[1:]
		require.True(t, v >= lo && v <= hi, "scripted pick %d outside [%d,%d]", v, lo, hi)
		return v
	}
	app.now = func() time.Time { return time.Unix(0, 0) }
	return h
}

func (h *vestingHarness) env(user *User) *AppEnv {
	return &AppEnv{
		User:   user,
		Users:  h.users,
		Wallet: h.wallet,
		Cfg:    h.cfg,
		Slot:   h.slot,
		Logger: slog.New(slog.NewTextHandler(&h.logs, &slog.HandlerOptions{Level: slog.LevelInfo})),
		Submit: func(txType, txID string, txBytes []byte) error {
			require.Equal(h.t, "vesting", txType)
			if h.reject != nil {
				if err := h.reject(txBytes); err != nil {
					return err
				}
			}
			_, err := NewVerifier(h.cfg.ScriptParams).Verify(txBytes, h.known, h.slot)
			require.NoError(h.t, err, "submitted transaction must verify")
			outs, err := KnownUTxOsFromTx(txBytes)
			require.NoError(h.t, err)
			h.known = append(h.known, outs...)
			h.txs = append(h.txs, txBytes)
			return nil
		},
	}
}

func (h *vestingHarness) step(user *User, picks ...int) *conway.ConwayTransaction {
	h.t.Helper()
	h.picks = picks
	before := len(h.txs)
	ok, err := h.app.Step(h.env(user))
	require.NoError(h.t, err)
	require.True(h.t, ok)
	require.Len(h.t, h.txs, before+1)
	tx, err := conway.NewConwayTransactionFromCbor(h.txs[len(h.txs)-1])
	require.NoError(h.t, err)
	return tx
}

// Scripted picks: the first decides lock (0) against advance (1-3); a lock
// then draws the amount, the lock length in slots and the beneficiary.
const (
	vestLock      = 0
	vestAdvance   = 1
	vestClawback  = 0
	vestProceed   = 1
	vestLockSlots = vestingLockMinSlots
)

func (h *vestingHarness) lock(owner *User) {
	h.t.Helper()
	h.step(owner, vestLock, vestingAmountMin, vestLockSlots, 0)
}

// Per-transaction detail is debug-level, so a run submitting thousands of
// transactions logs one line per batch at the default level.
func TestVestingPerTransactionLogsStayBelowInfo(t *testing.T) {
	h := newVestingHarness(t)
	owner := h.users[0]
	h.lock(owner)
	h.reject = func([]byte) error { return errors.New("mempool full") }
	h.picks = []int{vestAdvance, 0, vestClawback}
	ok, err := h.app.Step(h.env(owner))
	require.NoError(t, err)
	require.False(t, ok)
	require.Empty(t, h.logs.String())
}

func TestVestingBeneficiaryWithdrawsAfterLock(t *testing.T) {
	h := newVestingHarness(t)
	owner, beneficiary := h.users[0], h.users[1]
	h.lock(owner)
	require.Len(t, h.app.positions, 1)
	pos := h.app.positions[0]
	require.True(t, sameUser(pos.beneficiary, beneficiary))
	require.Equal(t, int64(1_700_000_000+vestingTestStart+vestingLockMinSlots)*1000, pos.lockUntil)

	h.slot = vestingTestStart + vestingLockMinSlots + 1
	tx := h.step(beneficiary, vestAdvance, 0)
	require.Len(t, h.app.positions, 1, "accepted spend waits for reconciliation")
	require.NoError(t, h.app.ReconcileSnapshot(nil, h.users, map[string]bool{tx.Hash().String(): false}))
	require.Empty(t, h.app.positions)
	require.Equal(t, h.slot, tx.ValidityIntervalStart())
	require.Len(t, tx.RequiredSigners(), 1)
	require.Equal(t, keyHash(beneficiary.Key), tx.RequiredSigners()[0].Bytes())
	require.GreaterOrEqual(t, paidTo(tx, beneficiary.Key.Address), uint64(vestingAmountMin-1_000_000))
}

func TestVestingLockDeadlineMatchesTargetSlotWithSubMillisecondSlots(t *testing.T) {
	h := newVestingHarness(t)
	h.cfg.ScriptParams.SlotLength = 500 * time.Microsecond
	h.cfg.ScriptParams.SystemStart = time.Unix(1_700_000_000, 250_000)
	h.lock(h.users[0])

	pos := h.app.positions[0]
	want := slotMillis(h.cfg.ScriptParams, h.slot+vestingLockMinSlots)
	require.Equal(t, want, pos.lockUntil)
}

func TestVestingBeneficiaryWaitsForLock(t *testing.T) {
	h := newVestingHarness(t)
	owner, beneficiary := h.users[0], h.users[1]
	h.lock(owner)

	// Before the lock has passed the beneficiary has nothing to claim, so
	// the step locks a new position instead.
	h.slot = vestingTestStart + vestingLockMinSlots - 1
	h.step(beneficiary, vestAdvance, vestingAmountMin, vestLockSlots, 0)
	require.Len(t, h.app.positions, 2)
	require.True(t, h.app.positions[0].known != nil && !h.app.positions[0].busy)
}

func TestVestingReconstructsAndRestoresPendingPosition(t *testing.T) {
	h := newVestingHarness(t)
	h.lock(h.users[0])
	locked := h.app.positions[0]
	snapshot := UTxO{
		TxHash: locked.utxo.TxHash, Index: locked.utxo.Index,
		Amount: locked.utxo.Amount, Address: h.app.validator.Address(),
		InlineDatum: append([]byte(nil), locked.known[0].Datum...),
	}
	restarted, err := newVestingApp()
	require.NoError(t, err)
	require.NoError(t, restarted.ReconcileSnapshot([]UTxO{snapshot}, h.users, nil))
	require.Len(t, restarted.positions, 1, "startup recovers script positions")
	require.Equal(t, locked.lockUntil, restarted.positions[0].lockUntil)
	require.True(t, sameUser(locked.owner, restarted.positions[0].owner))
	require.True(t, sameUser(locked.beneficiary, restarted.positions[0].beneficiary))

	tx := h.step(h.users[0], vestAdvance, 0, vestClawback)
	require.Len(t, h.app.positions, 1, "accepted spend is retained until confirmed")
	require.Equal(t, tx.Hash().String(), h.app.positions[0].pendingSpend)
	require.NoError(t, h.app.ReconcileSnapshot(
		[]UTxO{snapshot}, h.users, map[string]bool{tx.Hash().String(): false},
	))
	require.Len(t, h.app.positions, 1, "a dropped spend restores its still-unspent input")
	require.Empty(t, h.app.positions[0].pendingSpend)
	require.False(t, h.app.positions[0].busy)

	// The same absent transaction with no script input in the snapshot means
	// the spend has reached the chain.
	require.NoError(t, h.app.ReconcileSnapshot(nil, h.users, map[string]bool{tx.Hash().String(): false}))
	require.Empty(t, h.app.positions)
}

// A lower bound equal to lock_until fails the validator, so the beneficiary
// must not try before the slot after it.
func TestVestingBeneficiaryWaitsPastTheLockInstant(t *testing.T) {
	h := newVestingHarness(t)
	owner, beneficiary := h.users[0], h.users[1]
	h.lock(owner)

	h.slot = vestingTestStart + vestingLockMinSlots
	h.step(beneficiary, vestAdvance, vestingAmountMin, vestLockSlots, 0)
	require.Len(t, h.app.positions, 2, "no withdraw at the lock instant")
}

func TestVestingOwnerClawsBackBeforeLock(t *testing.T) {
	h := newVestingHarness(t)
	owner := h.users[0]
	h.lock(owner)
	tx := h.step(owner, vestAdvance, 0, vestClawback)
	require.NoError(t, h.app.ReconcileSnapshot(nil, h.users, map[string]bool{tx.Hash().String(): false}))
	require.Empty(t, h.app.positions)
	require.Equal(t, uint64(0), tx.ValidityIntervalStart(), "the owner path needs no validity bound")
	require.Equal(t, keyHash(owner.Key), tx.RequiredSigners()[0].Bytes())
	require.GreaterOrEqual(t, paidTo(tx, owner.Key.Address), uint64(vestingAmountMin-1_000_000))
}

func TestVestingOwnerMayLockAgainInsteadOfClawingBack(t *testing.T) {
	h := newVestingHarness(t)
	owner := h.users[0]
	h.lock(owner)
	h.step(owner, vestAdvance, 0, vestProceed, vestingAmountMin, vestLockSlots, 0)
	require.Len(t, h.app.positions, 2)
}

// Each rejection below is the validator's own: the planner must refuse to
// build a transaction the script would fail, so the app never submits one.
func TestVestingPlannerRejectsWhatTheValidatorRejects(t *testing.T) {
	h := newVestingHarness(t)
	owner, beneficiary := h.users[0], h.users[1]
	h.lock(owner)
	pos := h.app.positions[0]
	params := h.cfg.ScriptParams
	slotMs := func(slot uint64) int64 {
		return params.SystemStart.Add(time.Duration(slot) * params.SlotLength).UnixMilli()
	}

	plan := func(actor *User, slot, start uint64) error {
		collateral, _, err := h.wallet.SelectCoinsForKey(actor.Key, vestingCollateral)
		require.NoError(t, err)
		defer h.wallet.ReturnUTxOs(collateral)
		spec := ScriptTxSpec{
			Validator:       h.app.validator,
			CostModel:       params.CostModel,
			ScriptInputs:    []ScriptInput{{UTxO: pos.utxo, Redeemer: vestingRedeemer()}},
			Collateral:      collateral,
			Outputs:         []ScriptOutput{{Address: actor.Key.Address, Lovelace: pos.lovelace}},
			RequiredSigners: [][]byte{keyHash(actor.Key)},
			SigningKeys:     []*UTxOKey{actor.Key},
			ValidityStart:   start,
		}
		known := append(append([]KnownUTxO(nil), pos.known...), walletKnown(collateral)...)
		_, _, err = PlanScriptTx(spec, NewVerifier(params), params, known, slot)
		return err
	}

	// Slot s is at lock_until exactly: the validator wants a lower bound
	// strictly after it.
	atLock := uint64(vestingTestStart + vestingLockMinSlots)
	require.Equal(t, pos.lockUntil, slotMs(atLock))
	require.Error(t, plan(beneficiary, atLock, atLock), "lower bound equal to lock_until")
	require.Error(t, plan(beneficiary, atLock-1, atLock-1), "lower bound before lock_until")
	require.Error(t, plan(beneficiary, atLock+1, 0), "no lower bound at all")
	require.NoError(t, plan(beneficiary, atLock+1, atLock+1))
	require.NoError(t, plan(owner, vestingTestStart, 0), "owner needs no bound")

	stranger := &User{ID: "stranger", Key: escrowTestKey(0x03)}
	h.wallet.Add(UTxO{TxHash: strings.Repeat("cc", 32), Amount: 50_000_000, SigningKey: stranger.Key})
	require.Error(t, plan(stranger, atLock+1, atLock+1), "neither owner nor beneficiary")
}

func TestVestingAbandonsPositionOnMissingInputRejection(t *testing.T) {
	h := newVestingHarness(t)
	owner := h.users[0]
	h.lock(owner)
	pos := h.app.positions[0]
	balance := h.wallet.Balance()

	h.reject = func([]byte) error { return errors.New("mempool full") }
	h.picks = []int{vestAdvance, 0, vestClawback}
	ok, err := h.app.Step(h.env(owner))
	require.NoError(t, err)
	require.False(t, ok)
	require.Len(t, h.app.positions, 1, "an unrelated rejection keeps the position")
	require.False(t, h.app.positions[0].busy)
	require.Equal(t, balance, h.wallet.Balance(), "collateral is returned")

	h.reject = func([]byte) error {
		return errors.New("BadInputsUTxO TxId " + pos.utxo.TxHash + " TxIx 0")
	}
	h.picks = []int{vestAdvance, 0, vestClawback}
	ok, err = h.app.Step(h.env(owner))
	require.NoError(t, err)
	require.False(t, ok)
	require.Empty(t, h.app.positions)
	require.Equal(t, balance, h.wallet.Balance())
}

func TestVestingRepeatedRejectionsDropPosition(t *testing.T) {
	h := newVestingHarness(t)
	owner := h.users[0]
	h.lock(owner)
	h.reject = func([]byte) error { return errors.New("unrecognised failure") }
	for i := 0; i < vestingMaxRejections; i++ {
		require.Len(t, h.app.positions, 1, "attempt %d", i)
		h.picks = []int{vestAdvance, 0, vestClawback}
		ok, err := h.app.Step(h.env(owner))
		require.NoError(t, err)
		require.False(t, ok)
	}
	require.Empty(t, h.app.positions)
}

func TestVestingClaimTakesPositionOnce(t *testing.T) {
	h := newVestingHarness(t)
	owner := h.users[0]
	h.lock(owner)

	h.picks = []int{vestAdvance, 0, vestClawback}
	first, _ := h.app.claim(owner, 0)
	require.NotNil(t, first)
	h.picks = []int{vestAdvance, 0, vestClawback}
	second, action := h.app.claim(owner, 0)
	require.Nil(t, second, "a claimed position is not offered again")
	require.Equal(t, vestingLock, action)
}

func TestVestingKeepsNewPositionUnspendableUntilConfirmed(t *testing.T) {
	h := newVestingHarness(t)
	owner := h.users[0]
	h.lock(owner)
	h.app.positions[0].availableAt = h.app.now().Add(time.Hour)
	h.picks = []int{vestAdvance, 0, vestClawback}
	pos, _ := h.app.claim(owner, 0)
	require.Nil(t, pos)
}

func TestVestingSkipsUserWithoutCollateral(t *testing.T) {
	h := newVestingHarness(t)
	owner := h.users[0]
	h.lock(owner)
	// The owner's only coin is the locked change; with one small coin there
	// is nothing to pledge.
	h.wallet = NewWallet()
	h.wallet.Add(UTxO{TxHash: strings.Repeat("dd", 32), Amount: 1_000_000, SigningKey: owner.Key})
	balance := h.wallet.Balance()
	h.picks = []int{vestAdvance, 0, vestClawback}
	ok, err := h.app.Step(h.env(owner))
	require.NoError(t, err)
	require.False(t, ok)
	require.Equal(t, balance, h.wallet.Balance())
	require.Len(t, h.app.positions, 1)
	require.False(t, h.app.positions[0].busy)
}

func TestVestingGating(t *testing.T) {
	h := newVestingHarness(t)
	env := h.env(h.users[0])
	env.Slot = 2*500 + 499
	ok, err := h.app.Step(env)
	require.NoError(t, err)
	require.False(t, ok, "epoch 2 is too early")

	h.cfg.PlutusV3CostModel = nil
	ok, err = h.app.Step(h.env(h.users[0]))
	require.NoError(t, err)
	require.False(t, ok, "no cost model")

	h = newVestingHarness(t)
	h.cfg.ScriptParams.SystemStart = time.Time{}
	ok, err = h.app.Step(h.env(h.users[0]))
	require.NoError(t, err)
	require.False(t, ok, "without a system start the lock time cannot be set")
	require.Empty(t, h.txs)
}

func TestVestingEnabledNeedsTwoUsers(t *testing.T) {
	app, err := newVestingApp()
	require.NoError(t, err)
	require.False(t, app.Enabled(1))
	require.True(t, app.Enabled(2))
}

func TestVestingIsRegistered(t *testing.T) {
	_, ok := lookupApp("vesting")
	require.True(t, ok)
}

func TestVestingKeepsCollateralReservedUntilReconciled(t *testing.T) {
	h := newVestingHarness(t)
	owner := h.users[0]
	h.lock(owner)
	claw := h.step(owner, vestAdvance, 0, vestClawback)

	collateral := claw.Collateral()
	require.Len(t, collateral, 1)
	ref := collateral[0].Id().String() + ":" + strconv.FormatUint(uint64(collateral[0].Index()), 10)
	all, _, err := h.wallet.SelectCoins(h.wallet.Balance())
	require.NoError(t, err)
	for _, u := range all {
		require.NotEqual(t, ref, utxoKey(u))
	}

	pendingID := claw.Hash().String()
	h.wallet.mu.Lock()
	pending, ok := h.wallet.pending[pendingID]
	h.wallet.mu.Unlock()
	require.True(t, ok)
	// The source collateral is still on chain after a successful script spend.
	// Reconciliation retires the pending reservation and makes it selectable.
	snapshot := append([]UTxO(nil), h.wallet.utxos...)
	snapshot = append(snapshot, pending.inputs...)
	h.wallet.ReconcileSnapshot(snapshot, map[string]bool{pendingID: false})
	h.wallet.ReconcileSnapshot(snapshot, map[string]bool{pendingID: false})
	all, _, err = h.wallet.SelectCoins(h.wallet.Balance())
	require.NoError(t, err)
	found := false
	for _, u := range all {
		found = found || utxoKey(u) == ref
	}
	require.True(t, found, "reconciliation releases unspent collateral")
}
