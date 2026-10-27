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
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/blinklabs-io/gouroboros/protocol/localtxsubmission"
	"github.com/stretchr/testify/require"
)

// ledgerRejection is a node verdict against the transaction, as opposed to a
// transport failure.
func ledgerRejection(msg string) error {
	return fmt.Errorf("%s: %w", msg, localtxsubmission.TransactionRejectedError{})
}

// errTransport stands in for a connection that failed before the node gave a
// verdict: it is not a TransactionRejectedError.
var errTransport = errors.New("connection reset by peer")

func keyedPump(t *testing.T, cfg *Config, utxos ...UTxO) *Pump {
	t.Helper()
	pump := reconnectPump(cfg)
	pump.wallet = NewWallet()
	pump.wallet.Add(utxos...)
	require.NotEmpty(t, pump.wallet.SigningAddresses())
	return pump
}

func TestPeerPaymentKeepsInputsReservedAfterATransportFailure(t *testing.T) {
	ka, kb := peerTestKey(t, 0x0a), peerTestKey(t, 0x0b)
	w := fundedWallet(100_000_000, 0, ka, kb)
	w.AddKey(kb)
	users := usersFromWallet(w)
	env := peerEnv(w, users, func(string, string, []byte) error { return errTransport })
	env.User = users[0]
	if env.User.Key != ka {
		env.User = users[1]
	}

	ok, err := peerPayment{}.Step(env)

	require.NoError(t, err)
	require.False(t, ok)
	require.Len(t, w.PendingIDs(), 1, "the payment stays reserved for reconciliation")
	require.Zero(t, w.Len(), "its input is not offered to the next batch")
}

func TestEscrowInitiateKeepsInputsAndPositionAfterATransportFailure(t *testing.T) {
	h := newEscrowHarness(t)
	h.reject = func([]byte) error { return errTransport }
	h.picks = []int{pickInitiate, escrowAmountMin}
	balance := h.wallet.Balance()

	ok, err := h.app.Step(h.env(h.users[0]))

	require.NoError(t, err)
	require.False(t, ok)
	require.Len(t, h.wallet.PendingIDs(), 1, "the lock stays reserved")
	require.Less(t, h.wallet.Balance(), balance, "its inputs are not returned")
	require.Len(t, h.app.positions, 1, "the lock may be on chain, so the position is kept")
	require.Equal(t, escrowInitiated, h.app.positions[0].state)
}

func TestEscrowSpendKeepsInputsAndCollateralAfterATransportFailure(t *testing.T) {
	h := newEscrowHarness(t)
	initiator, recipient := h.users[0], h.users[1]
	h.step(initiator, pickInitiate, escrowAmountMin)
	pending := len(h.wallet.PendingIDs())
	balance := h.wallet.Balance()

	h.reject = func([]byte) error { return errTransport }
	h.picks = []int{pickAdvance, 0, pickProceed, escrowAmountMin}
	ok, err := h.app.Step(h.env(recipient))

	require.NoError(t, err)
	require.False(t, ok)
	require.Len(t, h.wallet.PendingIDs(), pending+1, "the spend stays reserved")
	require.Less(t, h.wallet.Balance(), balance, "inputs and collateral are not returned")
	require.Len(t, h.app.positions, 1)
	require.Equal(t, escrowActive, h.app.positions[0].state,
		"the deposit may be on chain, so the position is not retried")
	require.False(t, h.app.positions[0].busy)
}

func TestSubmitPlutusLockKeepsInputsReservedAfterATransportFailure(t *testing.T) {
	key := testSigningKey(0x78)
	pump := keyedPump(t, &Config{}, UTxO{
		TxHash: fmt.Sprintf("%064x", 2), Amount: 20_000_000, SigningKey: key,
	})

	require.False(t, pump.submitPlutusLock(&NodeClient{addr: "dead"}, 1))
	require.Len(t, pump.wallet.PendingIDs(), 1)
	require.Zero(t, pump.wallet.Len())
	require.Len(t, pump.plutusLocked, 1, "the lock may be on chain")
}

func TestSubmitPlutusUnlockKeepsCollateralReservedAfterATransportFailure(t *testing.T) {
	locked, collateral := sampleUnlockInputs()
	pump := keyedPump(t, &Config{PlutusV3CostModel: sampleCostModel}, collateral)

	require.False(t, pump.submitPlutusUnlock(&NodeClient{addr: "dead"}, 1, locked))
	require.Len(t, pump.wallet.PendingIDs(), 1)
	require.Zero(t, pump.wallet.Len(), "collateral stays with the unlock")
	require.Len(t, pump.pendingPlutusUnlocks, 1,
		"reconciliation restores the locked output if the unlock is absent")
}

// A transport failure says nothing about the certificate, so the registration
// record is left alone and the inputs stay reserved.
func TestSubmitCertTxKeepsInputsAndRegistrationAfterATransportFailure(t *testing.T) {
	key := testSigningKey(0x79)
	pump := keyedPump(t, &Config{}, UTxO{
		TxHash: fmt.Sprintf("%064x", 3), Amount: 20_000_000, SigningKey: key,
	})
	registered := map[string]time.Time{}

	ok := pump.submitCertTx(
		&NodeClient{addr: "dead"}, 1, "delegation", stakeKeyDeposit, registered,
		func([]UTxO, uint64, uint64, []byte) ([]byte, error) {
			return []byte{0x84}, nil
		},
	)

	require.False(t, ok)
	require.Empty(t, registered)
	require.Len(t, pump.wallet.PendingIDs(), 1)
	require.Zero(t, pump.wallet.Len())
}

// A fallback that fails, recovers and fails again is two outages, and each is
// reported at error level.
func TestRunReportsARecurringFallbackOutageAgain(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var buf bytes.Buffer
	pump := reconnectPump(&Config{StartupTimeout: 0, FallbackAddr: "fallback"})
	pump.logger = slog.New(slog.NewTextHandler(&buf, nil))
	pump.dialPrimaryFn = func() (*NodeClient, error) {
		return nil, errors.New("connection refused")
	}
	fallbackDials := 0
	pump.dialFallbackFn = func() (*NodeClient, error) {
		fallbackDials++
		if fallbackDials == 2 {
			return &NodeClient{addr: "fallback"}, nil
		}
		return nil, errors.New("fallback refused")
	}
	pump.cooldownFn = func(context.Context) bool {
		if fallbackDials >= 3 {
			cancel()
		}
		return true
	}
	pump.runBatchFn = func(context.Context, *NodeClient, int) int { return 1 }

	require.ErrorIs(t, pump.Run(ctx), context.Canceled)
	require.Equal(
		t, 2,
		strings.Count(buf.String(), "level=ERROR msg=\"fallback connection also failed"),
	)
}
