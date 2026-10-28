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
	"io"
	"log/slog"
	"math/rand"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/blinklabs-io/gouroboros/ledger/conway"
	"github.com/blinklabs-io/plutigo/data"
	"github.com/stretchr/testify/require"
)

const transferTestStart = 3 * 500

// transferHarness drives a transferApp against a wallet of three users and
// checks every submitted transaction with an independent verifier against the
// outputs seen so far, as a node would.
type transferHarness struct {
	t      *testing.T
	app    *transferApp
	wallet *Wallet
	users  []*User
	cfg    *Config
	slot   uint64
	known  []KnownUTxO
	txs    [][]byte
	reject func(txBytes []byte) error
	picks  []int
}

func newTransferHarness(t *testing.T) *transferHarness {
	t.Helper()
	app, err := newTransferApp()
	require.NoError(t, err)
	h := &transferHarness{t: t, app: app, wallet: NewWallet(), slot: transferTestStart}
	for i, k := range []*UTxOKey{escrowTestKey(0x01), escrowTestKey(0x02), escrowTestKey(0x03)} {
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

func (h *transferHarness) env(user *User) *AppEnv {
	return &AppEnv{
		User:   user,
		Users:  h.users,
		Wallet: h.wallet,
		Cfg:    h.cfg,
		Slot:   h.slot,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Submit: func(txType, txID string, txBytes []byte) error {
			require.Equal(h.t, "simple-transfer", txType)
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

func (h *transferHarness) step(user *User, picks ...int) *conway.ConwayTransaction {
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

// Scripted picks: the first decides lock (0) against claim (1-3); a lock then
// draws the amount and the receiver among the other users; a claim draws the
// position.
const (
	transferLockPick  = 0
	transferClaimPick = 1
)

// lock has sender lock transferAmountMin for the receiverIdx-th of the other
// users.
func (h *transferHarness) lock(sender *User, receiverIdx int) {
	h.t.Helper()
	h.step(sender, transferLockPick, transferAmountMin, receiverIdx)
}

func TestTransferLockPaysTheScriptAppliedToTheReceiver(t *testing.T) {
	h := newTransferHarness(t)
	sender, receiver := h.users[0], h.users[1]
	h.lock(sender, 0)
	require.Len(t, h.app.positions, 1)
	pos := h.app.positions[0]
	require.True(t, sameUser(pos.receiver, receiver))

	want, err := h.app.template.Apply(data.NewByteString(keyHash(receiver.Key)))
	require.NoError(t, err)
	require.Equal(t, want.Address(), h.known[len(h.known)-2].Address)
	require.Equal(t, want.Hash, pos.validator.Hash)
	require.NotEqual(t, h.app.template.Hash, pos.validator.Hash)
	require.Equal(t, uint64(transferAmountMin), pos.lovelace)
}

func TestTransferEachReceiverHasItsOwnScript(t *testing.T) {
	h := newTransferHarness(t)
	sender := h.users[0]
	h.lock(sender, 0)
	h.lock(sender, 1)
	require.Len(t, h.app.positions, 2)
	a, b := h.app.positions[0], h.app.positions[1]
	require.False(t, sameUser(a.receiver, b.receiver))
	require.NotEqual(t, a.validator.Hash, b.validator.Hash)
	require.NotEqual(t, a.validator.Address(), b.validator.Address())
}

func TestTransferReceiverClaimsThePosition(t *testing.T) {
	h := newTransferHarness(t)
	sender, receiver := h.users[0], h.users[1]
	h.lock(sender, 0)
	tx := h.step(receiver, transferClaimPick, 0)
	require.Empty(t, h.app.positions)
	require.Len(t, tx.RequiredSigners(), 1)
	require.Equal(t, keyHash(receiver.Key), tx.RequiredSigners()[0].Bytes())
	require.GreaterOrEqual(t, paidTo(tx, receiver.Key.Address), uint64(transferAmountMin-1_000_000))
}

func TestTransferOnlyTheReceiverClaims(t *testing.T) {
	h := newTransferHarness(t)
	sender, other := h.users[0], h.users[2]
	h.lock(sender, 0)
	// Neither the sender nor a third user can claim, so both lock again.
	h.step(sender, transferClaimPick, transferAmountMin, 0)
	h.step(other, transferClaimPick, transferAmountMin, 0)
	require.Len(t, h.app.positions, 3)
}

// Each rejection below is the validator's own: the planner must refuse to
// build a transaction the script would fail, so the app never submits one.
func TestTransferPlannerRejectsWhatTheValidatorRejects(t *testing.T) {
	h := newTransferHarness(t)
	sender, receiver, other := h.users[0], h.users[1], h.users[2]
	h.lock(sender, 0)
	pos := h.app.positions[0]
	params := h.cfg.ScriptParams

	plan := func(validator PlutusV3Validator, actor *User, signers [][]byte) error {
		collateral, _, err := h.wallet.SelectCoinsForKey(actor.Key, transferCollateral)
		require.NoError(t, err)
		defer h.wallet.ReturnUTxOs(collateral)
		spec := ScriptTxSpec{
			Validator:       validator,
			CostModel:       params.CostModel,
			ScriptInputs:    []ScriptInput{{UTxO: pos.utxo, Redeemer: data.NewConstr(0)}},
			Collateral:      collateral,
			Outputs:         []ScriptOutput{{Address: actor.Key.Address, Lovelace: pos.lovelace}},
			RequiredSigners: signers,
			SigningKeys:     []*UTxOKey{actor.Key},
		}
		known := append(append([]KnownUTxO(nil), pos.known...), walletKnown(collateral)...)
		_, _, err = PlanScriptTx(spec, NewVerifier(params), params, known, h.slot)
		return err
	}

	require.NoError(t, plan(pos.validator, receiver, [][]byte{keyHash(receiver.Key)}))
	require.Error(t, plan(pos.validator, receiver, nil), "receiver has not signed")
	require.Error(t, plan(pos.validator, sender, [][]byte{keyHash(sender.Key)}), "the sender is not the receiver")
	require.Error(t, plan(pos.validator, other, [][]byte{keyHash(other.Key)}), "a third user is not the receiver")

	// A script applied to another receiver is another script: it is not the
	// one the position is locked at.
	wrong, err := h.app.validatorFor(other)
	require.NoError(t, err)
	require.Error(t, plan(wrong, other, [][]byte{keyHash(other.Key)}))
	require.Error(t, plan(h.app.template, receiver, [][]byte{keyHash(receiver.Key)}), "the unapplied template")
}

func TestTransferAbandonsPositionOnMissingInputRejection(t *testing.T) {
	h := newTransferHarness(t)
	sender, receiver := h.users[0], h.users[1]
	h.lock(sender, 0)
	pos := h.app.positions[0]
	balance := h.wallet.Balance()

	h.reject = func([]byte) error { return errors.New("mempool full") }
	h.picks = []int{transferClaimPick, 0}
	ok, err := h.app.Step(h.env(receiver))
	require.NoError(t, err)
	require.False(t, ok)
	require.Len(t, h.app.positions, 1, "an unrelated rejection keeps the position")
	require.False(t, h.app.positions[0].busy)
	require.Equal(t, balance, h.wallet.Balance(), "collateral is returned")

	h.reject = func([]byte) error {
		return errors.New("BadInputsUTxO TxId " + pos.utxo.TxHash + " TxIx 0")
	}
	h.picks = []int{transferClaimPick, 0}
	ok, err = h.app.Step(h.env(receiver))
	require.NoError(t, err)
	require.False(t, ok)
	require.Empty(t, h.app.positions)
	require.Equal(t, balance, h.wallet.Balance())
}

func TestTransferRepeatedRejectionsDropPosition(t *testing.T) {
	h := newTransferHarness(t)
	sender, receiver := h.users[0], h.users[1]
	h.lock(sender, 0)
	h.reject = func([]byte) error { return errors.New("unrecognised failure") }
	for i := 0; i < transferMaxRejections; i++ {
		require.Len(t, h.app.positions, 1, "attempt %d", i)
		h.picks = []int{transferClaimPick, 0}
		ok, err := h.app.Step(h.env(receiver))
		require.NoError(t, err)
		require.False(t, ok)
	}
	require.Empty(t, h.app.positions)
}

func TestTransferRejectedLockReturnsTheWalletInputs(t *testing.T) {
	h := newTransferHarness(t)
	balance := h.wallet.Balance()
	h.reject = func([]byte) error { return errors.New("mempool full") }
	h.picks = []int{transferLockPick, transferAmountMin, 0}
	ok, err := h.app.Step(h.env(h.users[0]))
	require.NoError(t, err)
	require.False(t, ok)
	require.Empty(t, h.app.positions)
	require.Equal(t, balance, h.wallet.Balance())
}

func TestTransferClaimTakesPositionOnce(t *testing.T) {
	h := newTransferHarness(t)
	h.lock(h.users[0], 0)
	receiver := h.users[1]

	h.picks = []int{transferClaimPick, 0}
	first := h.app.claim(receiver)
	require.NotNil(t, first)
	h.picks = []int{transferClaimPick, 0}
	require.Nil(t, h.app.claim(receiver), "a claimed position is not offered again")
}

func TestTransferKeepsNewPositionUnspendableUntilConfirmed(t *testing.T) {
	h := newTransferHarness(t)
	h.lock(h.users[0], 0)
	h.app.positions[0].availableAt = h.app.now().Add(time.Hour)
	h.picks = []int{transferClaimPick, 0}
	require.Nil(t, h.app.claim(h.users[1]))
}

func TestTransferSkipsReceiverWithoutCollateral(t *testing.T) {
	h := newTransferHarness(t)
	receiver := h.users[1]
	h.lock(h.users[0], 0)
	h.wallet = NewWallet()
	h.wallet.Add(UTxO{TxHash: strings.Repeat("dd", 32), Amount: 1_000_000, SigningKey: receiver.Key})
	balance := h.wallet.Balance()
	h.picks = []int{transferClaimPick, 0}
	ok, err := h.app.Step(h.env(receiver))
	require.NoError(t, err)
	require.False(t, ok)
	require.Equal(t, balance, h.wallet.Balance())
	require.Len(t, h.app.positions, 1)
	require.False(t, h.app.positions[0].busy)
}

func TestTransferSkipsSenderWithoutFunds(t *testing.T) {
	h := newTransferHarness(t)
	sender := h.users[0]
	h.wallet = NewWallet()
	h.wallet.Add(UTxO{TxHash: strings.Repeat("dd", 32), Amount: 1_000_000, SigningKey: sender.Key})
	h.picks = []int{transferLockPick, transferAmountMin, 0}
	ok, err := h.app.Step(h.env(sender))
	require.NoError(t, err)
	require.False(t, ok)
	require.Empty(t, h.txs)
}

func TestTransferGating(t *testing.T) {
	h := newTransferHarness(t)
	env := h.env(h.users[0])
	env.Slot = 2*500 + 499
	ok, err := h.app.Step(env)
	require.NoError(t, err)
	require.False(t, ok, "epoch 2 is too early")

	h.cfg.PlutusV3CostModel = nil
	ok, err = h.app.Step(h.env(h.users[0]))
	require.NoError(t, err)
	require.False(t, ok, "no cost model")
	require.Empty(t, h.txs)
}

func TestTransferEnabledNeedsTwoUsers(t *testing.T) {
	app, err := newTransferApp()
	require.NoError(t, err)
	require.False(t, app.Enabled(1))
	require.True(t, app.Enabled(2))
}

func TestTransferIsRegistered(t *testing.T) {
	_, ok := lookupApp("simple-transfer")
	require.True(t, ok)
}

func TestTransferKeepsCollateralReservedUntilReconciled(t *testing.T) {
	h := newTransferHarness(t)
	h.lock(h.users[0], 0)
	claim := h.step(h.users[1], transferClaimPick, 0)

	collateral := claim.Collateral()
	require.Len(t, collateral, 1)
	ref := collateral[0].Id().String() + ":" + strconv.FormatUint(uint64(collateral[0].Index()), 10)
	all, _, err := h.wallet.SelectCoins(h.wallet.Balance())
	require.NoError(t, err)
	for _, u := range all {
		require.NotEqual(t, ref, utxoKey(u))
	}
}

// A long random run exercises both transitions with real choices; the
// harness verifies each submitted transaction against the chain it has seen.
func TestTransferRandomRunVerifiesEveryStep(t *testing.T) {
	h := newTransferHarness(t)
	for i, u := range h.users {
		for j := 0; j < 80; j++ {
			c := UTxO{
				TxHash:     strings.Repeat(hex.EncodeToString([]byte{byte(0x40 + i), byte(j)}), 16),
				Amount:     50_000_000,
				SigningKey: u.Key,
			}
			h.wallet.Add(c)
			h.known = append(h.known, SyntheticWalletUTxO(c, u.Key.Address))
		}
	}
	rng := rand.New(rand.NewSource(7))
	h.app.choose = func(lo, hi int) int { return lo + rng.Intn(hi-lo+1) }

	var locks, claims int
	for range 200 {
		before := len(h.txs)
		ok, err := h.app.Step(h.env(h.users[rng.Intn(len(h.users))]))
		require.NoError(t, err)
		if !ok {
			require.Len(t, h.txs, before)
			continue
		}
		tx, err := conway.NewConwayTransactionFromCbor(h.txs[len(h.txs)-1])
		require.NoError(t, err)
		if len(tx.RequiredSigners()) > 0 {
			claims++
		} else {
			locks++
		}
	}
	t.Logf("locks %d, claims %d", locks, claims)
	require.Positive(t, locks)
	require.Positive(t, claims)
}
