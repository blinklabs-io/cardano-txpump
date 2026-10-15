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

import "fmt"

// User is one simulated identity: a signing key whose UTxOs live in the
// shared wallet.
type User struct {
	ID  string
	Key *UTxOKey
}

// usersFromWallet returns one user per distinct key the wallet holds, in
// address order so IDs are stable across runs.
func usersFromWallet(w *Wallet) []*User {
	keys := w.Keys()
	users := make([]*User, len(keys))
	for i, k := range keys {
		users[i] = &User{ID: fmt.Sprintf("user-%d", i), Key: k}
	}
	return users
}

// SelectUserKeys returns the keys at the given positions, in position order.
// A nil selection keeps every key; a position past the end is an error, so a
// stale setting cannot silently shrink the user set.
func SelectUserKeys(keys []*UTxOKey, indexes []int) ([]*UTxOKey, error) {
	if indexes == nil {
		return keys, nil
	}
	out := make([]*UTxOKey, 0, len(indexes))
	for _, i := range indexes {
		if i < 0 || i >= len(keys) {
			return nil, fmt.Errorf(
				"TXPUMP_USERS: index %d out of range, %d keys loaded",
				i, len(keys),
			)
		}
		out = append(out, keys[i])
	}
	return out, nil
}
