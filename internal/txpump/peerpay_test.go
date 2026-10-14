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
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func peerTestKey(t *testing.T, tag byte) *UTxOKey {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	addr := append([]byte{0x60}, bytes.Repeat([]byte{tag}, 28)...)
	return &UTxOKey{VKey: pub, SKey: priv.Seed(), Address: addr}
}

func peerEnv(
	w *Wallet,
	users []*User,
	submit func(string, string, []byte) error,
) *AppEnv {
	return &AppEnv{
		User:   users[0],
		Users:  users,
		Wallet: w,
		Cfg:    &Config{},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Submit: submit,
	}
}

func fundedWallet(amountA, amountB uint64, ka, kb *UTxOKey) *Wallet {
	w := NewWallet()
	if amountA > 0 {
		w.Add(UTxO{TxHash: strings.Repeat("aa", 32), Amount: amountA, SigningKey: ka})
	}
	if amountB > 0 {
		w.Add(UTxO{TxHash: strings.Repeat("bb", 32), Amount: amountB, SigningKey: kb})
	}
	return w
}

func TestPeerPaymentPaysAnotherUser(t *testing.T) {
	ka, kb := peerTestKey(t, 0x0a), peerTestKey(t, 0x0b)
	w := fundedWallet(100_000_000, 100_000_000, ka, kb)
	users := usersFromWallet(w)
	var sentTxType string
	var sentBytes []byte
	env := peerEnv(w, users, func(txType, _ string, b []byte) error {
		sentTxType, sentBytes = txType, b
		return nil
	})

	ok, err := peerPayment{}.Step(env)

	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "peer_payment", sentTxType)
	assert.True(t, bytes.Contains(sentBytes, kb.Address), "tx must pay the peer")
}

func TestPeerPaymentSkipsWithoutPeer(t *testing.T) {
	ka := peerTestKey(t, 0x0a)
	w := fundedWallet(100_000_000, 0, ka, nil)
	users := usersFromWallet(w)
	env := peerEnv(w, users, func(string, string, []byte) error {
		t.Fatal("must not submit")
		return nil
	})

	ok, err := peerPayment{}.Step(env)

	assert.NoError(t, err)
	assert.False(t, ok)
}

func TestPeerPaymentUserWithoutFundsIsSkipped(t *testing.T) {
	ka, kb := peerTestKey(t, 0x0a), peerTestKey(t, 0x0b)
	w := fundedWallet(0, 100_000_000, ka, kb)
	// ka holds nothing, so Keys() omits it; build both users by hand.
	users := []*User{{ID: "user-0", Key: ka}, {ID: "user-1", Key: kb}}
	env := peerEnv(w, users, func(string, string, []byte) error {
		t.Fatal("must not submit")
		return nil
	})

	ok, err := peerPayment{}.Step(env)

	assert.NoError(t, err)
	assert.False(t, ok)
	assert.Equal(t, uint64(100_000_000), w.Balance(), "peer funds untouched")
}

func TestPeerPaymentReturnsInputsOnRejection(t *testing.T) {
	ka, kb := peerTestKey(t, 0x0a), peerTestKey(t, 0x0b)
	w := fundedWallet(100_000_000, 100_000_000, ka, kb)
	users := usersFromWallet(w)
	env := peerEnv(w, users, func(string, string, []byte) error {
		return errors.New("rejected")
	})

	ok, err := peerPayment{}.Step(env)

	assert.NoError(t, err)
	assert.False(t, ok)
	assert.Equal(t, uint64(200_000_000), w.Balance(), "inputs returned")
}

// Random send amount and fee make the change land in the sub-minimum dust
// range on some iterations; BuildPayment rejects such change, so every run
// must succeed only because the app folds it into the payment.
func TestPeerPaymentFoldsDustChange(t *testing.T) {
	ka, kb := peerTestKey(t, 0x0a), peerTestKey(t, 0x0b)
	for i := 0; i < 300; i++ {
		w := fundedWallet(3_000_000, 100_000_000, ka, kb)
		users := usersFromWallet(w)
		env := peerEnv(w, users, func(string, string, []byte) error { return nil })
		_, err := peerPayment{}.Step(env)
		require.NoError(t, err, "iteration %d", i)
	}
}

func TestPeerPaymentIsRegistered(t *testing.T) {
	_, ok := lookupApp("peer_payment")
	assert.True(t, ok)
}
