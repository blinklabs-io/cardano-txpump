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
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func reservedWallet() (*Wallet, *UTxOKey, *UTxOKey) {
	w := NewWallet()
	pub, other := escrowTestKey(0x01), escrowTestKey(0x02)
	w.Add(
		UTxO{TxHash: strings.Repeat("11", 32), Amount: 90_000_000, SigningKey: pub},
		UTxO{TxHash: strings.Repeat("22", 32), Amount: 10_000_000, SigningKey: other},
	)
	return w, pub, other
}

func TestReservedKeyIsNotAUser(t *testing.T) {
	w, pub, other := reservedWallet()
	require.Len(t, usersFromWallet(w), 2)
	w.ReserveKey(pub)
	users := usersFromWallet(w)
	require.Len(t, users, 1)
	require.Equal(t, other.Address, users[0].Key.Address)
	require.Len(t, w.SigningAddresses(), 2, "the reserved address is still reconciled")
}

func TestReservedKeyFundsAreNotSelectedByNormalCoinSelection(t *testing.T) {
	w, pub, other := reservedWallet()
	w.ReserveKey(pub)
	// The reserved UTxO is the largest, so largest-first would take it.
	inputs, _, err := w.SelectCoins(5_000_000)
	require.NoError(t, err)
	require.Len(t, inputs, 1)
	require.Equal(t, other.Address, inputs[0].SigningKey.Address)
	w.ReturnUTxOs(inputs)

	_, _, err = w.SelectCoins(50_000_000)
	require.ErrorIs(t, err, ErrInsufficientFunds)

	_, _, err = w.SelectCoinsForKey(pub, 1_000_000)
	require.ErrorIs(t, err, ErrKeyReserved)
}

func TestSpendableBalanceExcludesReservedKeyFunds(t *testing.T) {
	w, pub, _ := reservedWallet()
	w.ReserveKey(pub)
	require.EqualValues(t, 10_000_000, w.spendableBalance())
	require.EqualValues(t, 100_000_000, w.Balance(), "Balance remains the total available wallet balance")
}

func TestSelectReservedCoinsOnlyTakesTheReservedKeysFunds(t *testing.T) {
	w, pub, _ := reservedWallet()
	_, _, err := w.SelectReservedCoins(1_000_000)
	require.ErrorIs(t, err, ErrKeyReserved, "no key reserved yet")
	w.ReserveKey(pub)
	inputs, change, err := w.SelectReservedCoins(30_000_000)
	require.NoError(t, err)
	require.Len(t, inputs, 1)
	require.Equal(t, pub.Address, inputs[0].SigningKey.Address)
	require.Equal(t, uint64(60_000_000), change)
	_, _, err = w.SelectReservedCoins(100_000_000)
	require.ErrorIs(t, err, ErrInsufficientFunds)
}
