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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseUserIndexes(t *testing.T) {
	tests := []struct {
		in   string
		want []int
	}{
		{"0", []int{0}},
		{"2,0", []int{0, 2}},
		{"0-2", []int{0, 1, 2}},
		{" 1 , 3-4 ", []int{1, 3, 4}},
		{"1,1,0-1", []int{0, 1}},
		{"3-3", []int{3}},
		{"9,8,7,6,5,4,3,2,1,0", []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}},
	}
	for _, tt := range tests {
		got, err := parseUserIndexes(tt.in)
		require.NoError(t, err, tt.in)
		assert.Equal(t, tt.want, got, tt.in)
	}
}

func TestParseUserIndexesRejectsBadInput(t *testing.T) {
	for _, in := range []string{
		",", " ", "a", "-1", "1-", "-", "3-1", "1,,x", "0-1-2", "99999999999999999999",
		"0-9999999",
	} {
		_, err := parseUserIndexes(in)
		require.Error(t, err, in)
		assert.Contains(t, err.Error(), "TXPUMP_USERS", in)
	}
}

func TestLoadConfigUsers(t *testing.T) {
	clearTxpumpEnv(t)
	cfg, err := LoadConfig()
	require.NoError(t, err)
	assert.Nil(t, cfg.Users)

	t.Setenv("TXPUMP_USERS", "1,3")
	cfg, err = LoadConfig()
	require.NoError(t, err)
	assert.Equal(t, []int{1, 3}, cfg.Users)

	t.Setenv("TXPUMP_USERS", "x")
	_, err = LoadConfig()
	require.Error(t, err)
}

func TestSelectUserKeys(t *testing.T) {
	keys := []*UTxOKey{
		{Address: []byte{1}}, {Address: []byte{2}}, {Address: []byte{3}},
	}

	got, err := SelectUserKeys(keys, nil)
	require.NoError(t, err)
	assert.Equal(t, keys, got, "nil selection keeps every key")

	got, err = SelectUserKeys(keys, []int{0, 2})
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Same(t, keys[0], got[0])
	assert.Same(t, keys[2], got[1])

	_, err = SelectUserKeys(keys, []int{1, 3})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "TXPUMP_USERS")
	assert.Contains(t, err.Error(), "3")
	assert.Contains(t, err.Error(), "3 keys")

	_, err = SelectUserKeys(keys, []int{-1})
	require.Error(t, err)

	_, err = SelectUserKeys(nil, []int{0})
	require.Error(t, err)
}
