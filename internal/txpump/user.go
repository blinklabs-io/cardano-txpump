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
// A nil selection keeps every key; a range past the end is an error, so a stale
// setting cannot silently shrink the user set.
func SelectUserKeys(keys []*UTxOKey, indexes []UserIndexRange) ([]*UTxOKey, error) {
	if indexes == nil {
		return keys, nil
	}
	if len(keys) == 0 && len(indexes) > 0 {
		return nil, fmt.Errorf(
			"TXPUMP_USERS: index %d out of range, 0 keys loaded",
			indexes[0].Last,
		)
	}
	selected := make([]bool, len(keys))
	for _, indexRange := range indexes {
		if indexRange.First < 0 || indexRange.Last < indexRange.First ||
			indexRange.Last >= len(keys) {
			badIndex := indexRange.First
			if indexRange.Last >= len(keys) {
				badIndex = indexRange.Last
			}
			return nil, fmt.Errorf(
				"TXPUMP_USERS: index %d out of range, %d keys loaded",
				badIndex, len(keys),
			)
		}
		for i := indexRange.First; i <= indexRange.Last; i++ {
			selected[i] = true
		}
	}
	out := make([]*UTxOKey, 0, len(indexes))
	for i, include := range selected {
		if include {
			out = append(out, keys[i])
		}
	}
	return out, nil
}
