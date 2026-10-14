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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSelectCoinsForKeyIsolatesKeys(t *testing.T) {
	ka := &UTxOKey{Address: []byte{0x60, 0x0a}}
	kb := &UTxOKey{Address: []byte{0x60, 0x0b}}
	w := NewWallet()
	w.Add(
		UTxO{TxHash: strings.Repeat("aa", 32), Amount: 50_000_000, SigningKey: ka},
		UTxO{TxHash: strings.Repeat("bb", 32), Amount: 90_000_000, SigningKey: kb},
	)

	inputs, change, err := w.SelectCoinsForKey(ka, 10_000_000)
	require.NoError(t, err)
	require.Len(t, inputs, 1)
	assert.Same(t, ka, inputs[0].SigningKey)
	assert.Equal(t, uint64(40_000_000), change)
	// Key B's larger UTxO must be untouched.
	assert.Equal(t, uint64(90_000_000), w.Balance())

	_, _, err = w.SelectCoinsForKey(ka, 1)
	assert.ErrorIs(t, err, ErrInsufficientFunds)
}

func TestSelectCoinsForKeyRejectsNilKey(t *testing.T) {
	_, _, err := NewWallet().SelectCoinsForKey(nil, 1)
	assert.Error(t, err)
}

func TestWalletKeysSortedAndDistinct(t *testing.T) {
	ka := &UTxOKey{Address: []byte{0x60, 0x0a}}
	kb := &UTxOKey{Address: []byte{0x60, 0x0b}}
	w := NewWallet()
	w.Add(
		UTxO{TxHash: strings.Repeat("bb", 32), Amount: 1, SigningKey: kb},
		UTxO{TxHash: strings.Repeat("aa", 32), Amount: 1, SigningKey: ka},
		UTxO{TxHash: strings.Repeat("cc", 32), Amount: 1, SigningKey: ka},
	)
	keys := w.Keys()
	require.Len(t, keys, 2)
	assert.Same(t, ka, keys[0])
	assert.Same(t, kb, keys[1])
}
