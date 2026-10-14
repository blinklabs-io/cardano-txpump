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

func TestUsersFromWalletOrdersByAddress(t *testing.T) {
	ka := &UTxOKey{Address: []byte{0x60, 0x0a}}
	kb := &UTxOKey{Address: []byte{0x60, 0x0b}}
	w := NewWallet()
	w.Add(
		UTxO{TxHash: strings.Repeat("bb", 32), Amount: 1, SigningKey: kb},
		UTxO{TxHash: strings.Repeat("aa", 32), Amount: 1, SigningKey: ka},
	)
	users := usersFromWallet(w)
	require.Len(t, users, 2)
	assert.Equal(t, "user-0", users[0].ID)
	assert.Same(t, ka, users[0].Key)
	assert.Equal(t, "user-1", users[1].ID)
	assert.Same(t, kb, users[1].Key)
}

func TestUsersFromWalletWithoutKeysIsEmpty(t *testing.T) {
	assert.Empty(t, usersFromWallet(NewWallet()))
}
