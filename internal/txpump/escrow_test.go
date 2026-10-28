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
	"crypto/ed25519"
	"encoding/hex"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blinklabs-io/gouroboros/ledger/conway"
	"github.com/stretchr/testify/require"
)

// escrowTestKey returns a deterministic key whose enterprise address is
// derived from it, as the ledger requires for a key-locked input.
func escrowTestKey(seed byte) *UTxOKey {
	s := bytes.Repeat([]byte{seed}, ed25519.SeedSize)
	k := &UTxOKey{VKey: ed25519.NewKeyFromSeed(s).Public().(ed25519.PublicKey), SKey: s}
	k.Address = enterpriseAddr(k)
	return k
}

// escrowHarness drives an escrowApp against a wallet of two users and checks
// every submitted transaction with an independent verifier against the
// outputs seen so far, as a node would.
type escrowHarness struct {
	t      *testing.T
	app    *escrowApp
	wallet *Wallet
	users  []*User
	cfg    *Config
	mu     sync.Mutex
	known  []KnownUTxO
	txs    [][]byte
	reject func(txBytes []byte) error
	picks  []int
	refs   *RefScripts
}

func newEscrowHarness(t *testing.T) *escrowHarness {
	t.Helper()
	app, err := newEscrowApp()
	require.NoError(t, err)
	h := &escrowHarness{t: t, app: app, wallet: NewWallet()}
	for i, k := range []*UTxOKey{escrowTestKey(0x01), escrowTestKey(0x02)} {
		for j := 0; j < 4; j++ {
			u := UTxO{
				TxHash:     strings.Repeat(hex.EncodeToString([]byte{byte(0x10*(i+1) + j)}), 32),
				Index:      0,
				Amount:     50_000_000,
				SigningKey: k,
			}
			h.wallet.Add(u)
			h.known = append(h.known, SyntheticWalletUTxO(u, k.Address))
		}
	}
	h.users = usersFromWallet(h.wallet)
	cm := testCostModel(t)
	h.cfg = &Config{PlutusV3CostModel: cm, ScriptParams: DefaultScriptParams(cm), EpochLength: 500, ScriptAppsFromEpoch: 3}
	app.choose = func(lo, hi int) int {
		if len(h.picks) == 0 {
			return lo
		}
		v := h.picks[0]
		h.picks = h.picks[1:]
		require.True(t, v >= lo && v <= hi, "scripted pick %d outside [%d,%d]", v, lo, hi)
		return v
	}
	return h
}

func (h *escrowHarness) env(user *User) *AppEnv {
	return &AppEnv{
		User:   user,
		Users:  h.users,
		Wallet: h.wallet,
		Cfg:    h.cfg,
		Refs:   h.refs,
		Slot:   3 * 500,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Submit: func(txType, txID string, txBytes []byte) error {
			require.Equal(h.t, "escrow", txType)
			if h.reject != nil {
				if err := h.reject(txBytes); err != nil {
					return err
				}
			}
			h.mu.Lock()
			defer h.mu.Unlock()
			_, err := NewVerifier(h.cfg.ScriptParams).Verify(txBytes, h.known, 3*500)
			require.NoError(h.t, err, "submitted transaction must verify")
			outs, err := KnownUTxOsFromTx(txBytes)
			require.NoError(h.t, err)
			require.Equal(h.t, txID, strings.Split(outs[0].Ref, "#")[0])
			h.known = append(h.known, outs...)
			h.txs = append(h.txs, txBytes)
			return nil
		},
	}
}

// step runs one Step as user with a scripted sequence of choices and
// requires it to submit.
func (h *escrowHarness) step(user *User, picks ...int) *conway.ConwayTransaction {
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

func paidTo(tx *conway.ConwayTransaction, addr []byte) uint64 {
	var total uint64
	for _, o := range tx.Outputs() {
		if b, _ := o.Address().Bytes(); bytes.Equal(b, addr) {
			total += o.Amount().Uint64()
		}
	}
	return total
}

// Choice values for the scripted sequence: the first pick decides initiate
// (0) against advance (1-3); amounts are drawn with the pick's lower bound.
const (
	pickInitiate = 0
	pickAdvance  = 1
	pickCancel   = 0
	pickProceed  = 1
)

func TestEscrowLifecycleVerifiesAtEveryStep(t *testing.T) {
	h := newEscrowHarness(t)
	initiator, recipient := h.users[0], h.users[1]

	h.step(initiator, pickInitiate, escrowAmountMin)
	require.Len(t, h.app.positions, 1)
	require.Equal(t, escrowInitiated, h.app.positions[0].state)

	h.step(recipient, pickAdvance, 0, pickProceed, escrowAmountMin+1_000_000)
	require.Len(t, h.app.positions, 1)
	require.Equal(t, escrowActive, h.app.positions[0].state)

	complete := h.step(initiator, pickAdvance, 0, pickProceed)
	require.Empty(t, h.app.positions)
	require.GreaterOrEqual(t, paidTo(complete, initiator.Key.Address), uint64(escrowAmountMin+1_000_000),
		"initiator receives the recipient's deposit")
	require.GreaterOrEqual(t, paidTo(complete, recipient.Key.Address), uint64(escrowAmountMin),
		"recipient receives the initiator's deposit")
	signers := complete.RequiredSigners()
	require.Len(t, signers, 2)
}

func TestEscrowInitiatorCancelsBeforeDeposit(t *testing.T) {
	h := newEscrowHarness(t)
	initiator := h.users[0]
	h.step(initiator, pickInitiate, escrowAmountMin)
	cancel := h.step(initiator, pickAdvance, 0, pickCancel)
	require.Empty(t, h.app.positions)
	require.Len(t, cancel.RequiredSigners(), 1)
	require.Equal(t, keyHash(initiator.Key), cancel.RequiredSigners()[0].Bytes())
}

func TestEscrowCancelActiveRepaysBoth(t *testing.T) {
	h := newEscrowHarness(t)
	initiator, recipient := h.users[0], h.users[1]
	h.step(initiator, pickInitiate, escrowAmountMin)
	h.step(recipient, pickAdvance, 0, pickProceed, escrowAmountMin+2_000_000)
	cancel := h.step(recipient, pickAdvance, 0, pickCancel)
	require.Empty(t, h.app.positions)
	require.GreaterOrEqual(t, paidTo(cancel, initiator.Key.Address), uint64(escrowAmountMin))
	require.GreaterOrEqual(t, paidTo(cancel, recipient.Key.Address), uint64(escrowAmountMin+2_000_000))
}

func TestEscrowRejectionNamingMissingInputDropsPosition(t *testing.T) {
	h := newEscrowHarness(t)
	initiator, recipient := h.users[0], h.users[1]
	h.step(initiator, pickInitiate, escrowAmountMin)
	pos := h.app.positions[0]
	balance := h.wallet.Balance()

	h.reject = func([]byte) error { return ledgerRejection("mempool full") }
	h.picks = []int{pickAdvance, 0, pickProceed, escrowAmountMin}
	ok, err := h.app.Step(h.env(recipient))
	require.NoError(t, err)
	require.False(t, ok)
	require.Len(t, h.app.positions, 1, "an unrelated rejection keeps the position")
	require.False(t, h.app.positions[0].busy, "and releases it")
	require.Equal(t, balance, h.wallet.Balance(), "wallet inputs and collateral are returned")

	h.reject = func([]byte) error {
		return ledgerRejection("ConwayUtxowFailure (UtxoFailure (BadInputsUTxO (fromList [TxIn (TxId {unTxId = \"" + pos.utxo.TxHash + "\"}) (TxIx 0)])))")
	}
	h.picks = []int{pickAdvance, 0, pickProceed, escrowAmountMin}
	ok, err = h.app.Step(h.env(recipient))
	require.NoError(t, err)
	require.False(t, ok)
	require.Empty(t, h.app.positions, "a rejection naming the script input drops the position")
	require.Equal(t, balance, h.wallet.Balance())
}

func TestEscrowRepeatedRejectionsDropPosition(t *testing.T) {
	h := newEscrowHarness(t)
	initiator, recipient := h.users[0], h.users[1]
	h.step(initiator, pickInitiate, escrowAmountMin)
	h.reject = func([]byte) error { return ledgerRejection("unrecognised failure") }
	for i := 0; i < escrowMaxRejections; i++ {
		require.Len(t, h.app.positions, 1, "attempt %d", i)
		h.picks = []int{pickAdvance, 0, pickProceed, escrowAmountMin}
		ok, err := h.app.Step(h.env(recipient))
		require.NoError(t, err)
		require.False(t, ok)
	}
	require.Empty(t, h.app.positions)
}

func TestEscrowConcurrentStepsClaimPositionOnce(t *testing.T) {
	h := newEscrowHarness(t)
	initiator, recipient := h.users[0], h.users[1]
	h.step(initiator, pickInitiate, escrowAmountMin)
	h.step(recipient, pickAdvance, 0, pickProceed, escrowAmountMin)
	scriptRef := h.app.positions[0].utxo.TxHash

	// Every pick is "advance, proceed"; a step that finds nothing to claim
	// falls back to initiating, which needs no position.
	h.app.choose = func(lo, hi int) int {
		if lo == escrowAmountMin {
			return lo
		}
		return min(hi, 1)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	h.reject = func(txBytes []byte) error {
		tx, err := conway.NewConwayTransactionFromCbor(txBytes)
		require.NoError(t, err)
		for _, in := range tx.Inputs() {
			if in.Id().String() == scriptRef {
				once.Do(func() { close(entered) })
				select {
				case <-release:
				case <-time.After(10 * time.Second):
				}
			}
		}
		return nil
	}
	var wg sync.WaitGroup
	results := make([]bool, 2)
	wg.Add(1)
	go func() {
		defer wg.Done()
		results[0], _ = h.app.Step(h.env(initiator))
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		close(release)
		t.Fatal("the first step never submitted its spend")
	}
	ok, err := h.app.Step(h.env(recipient))
	require.NoError(t, err)
	results[1] = ok
	close(release)
	wg.Wait()

	spends := 0
	for _, b := range h.txs {
		tx, err := conway.NewConwayTransactionFromCbor(b)
		require.NoError(t, err)
		for _, in := range tx.Inputs() {
			if in.Id().String() == scriptRef {
				spends++
			}
		}
	}
	require.Equal(t, 1, spends, "the position is spent by exactly one step")
}

func TestEscrowSkipsUserWithoutCollateral(t *testing.T) {
	h := newEscrowHarness(t)
	initiator := h.users[0]
	h.step(initiator, pickInitiate, escrowAmountMin)

	// A recipient whose only UTxO must fund the deposit has nothing left to
	// pledge as collateral.
	poor := &User{ID: "poor", Key: escrowTestKey(0x03)}
	only := UTxO{TxHash: strings.Repeat("cc", 32), Amount: escrowAmountMin + 4_000_000, SigningKey: poor.Key}
	h.wallet.Add(only)
	h.users = append(h.users, poor)
	balance := h.wallet.Balance()
	h.picks = []int{pickAdvance, 0, pickProceed, escrowAmountMin}
	ok, err := h.app.Step(h.env(poor))
	require.NoError(t, err)
	require.False(t, ok)
	require.Equal(t, balance, h.wallet.Balance(), "selected coins are returned")
	require.Len(t, h.app.positions, 1)
	require.False(t, h.app.positions[0].busy)
}

func TestEscrowGatedByCostModelAndEpoch(t *testing.T) {
	h := newEscrowHarness(t)
	env := h.env(h.users[0])
	env.Slot = 2*500 + 499
	ok, err := h.app.Step(env)
	require.NoError(t, err)
	require.False(t, ok, "epoch 2 is too early")

	h.cfg.ScriptAppsFromEpoch = 2
	require.True(t, scriptAppsStarted(h.cfg, env.Slot), "a lower start epoch opens the gate")

	h.cfg.ScriptAppsFromEpoch = 3
	h.cfg.PlutusV3CostModel = nil
	ok, err = h.app.Step(h.env(h.users[0]))
	require.NoError(t, err)
	require.False(t, ok, "no cost model")
	require.Empty(t, h.txs)
}

func TestEscrowEnabledNeedsTwoUsers(t *testing.T) {
	app, err := newEscrowApp()
	require.NoError(t, err)
	require.False(t, app.Enabled(1))
	require.True(t, app.Enabled(2))
}

func TestEscrowIsRegistered(t *testing.T) {
	_, ok := lookupApp("escrow")
	require.True(t, ok)
}

// Collateral is not consumed by an accepted script transaction, but it must
// stay out of coin selection until the transaction settles: a payment that
// spent it first would make the node reject the script transaction after the
// position was already settled as spent.
func TestEscrowKeepsCollateralReservedUntilReconciled(t *testing.T) {
	h := newEscrowHarness(t)
	initiator, recipient := h.users[0], h.users[1]
	h.step(initiator, pickInitiate, escrowAmountMin)
	deposit := h.step(recipient, pickAdvance, 0, pickProceed, escrowAmountMin)

	collateral := deposit.Collateral()
	require.Len(t, collateral, 1)
	ref := collateral[0].Id().String() + ":" + strconv.FormatUint(uint64(collateral[0].Index()), 10)

	selectable := func() bool {
		h.wallet.mu.Lock()
		defer h.wallet.mu.Unlock()
		for _, u := range h.wallet.utxos {
			if utxoKey(u) == ref {
				return true
			}
		}
		return false
	}
	require.False(t, selectable(), "collateral must stay reserved with the pending transaction")
	all, _, err := h.wallet.SelectCoins(h.wallet.Balance())
	require.NoError(t, err)
	for _, u := range all {
		require.NotEqual(t, ref, utxoKey(u))
	}
	h.wallet.ReturnUTxOs(all)

	// Once the transaction has left the mempool and the snapshot still holds
	// the unconsumed collateral, reconciliation releases it.
	var snapshot []UTxO
	h.wallet.mu.Lock()
	snapshot = append(snapshot, h.wallet.utxos...)
	h.wallet.mu.Unlock()
	snapshot = append(snapshot, UTxO{
		TxHash: collateral[0].Id().String(), Index: collateral[0].Index(),
		Amount: 50_000_000, Address: recipient.Key.Address,
	})
	h.wallet.ReconcileSnapshot(snapshot, map[string]bool{})
	h.wallet.ReconcileSnapshot(snapshot, map[string]bool{})
	require.True(t, selectable(), "reconciliation releases unconsumed collateral")
}
