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
		want []UserIndexRange
	}{
		{"0", []UserIndexRange{{First: 0, Last: 0}}},
		{"2,0", []UserIndexRange{{First: 0, Last: 0}, {First: 2, Last: 2}}},
		{"0-2", []UserIndexRange{{First: 0, Last: 2}}},
		{" 1 , 3-4 ", []UserIndexRange{{First: 1, Last: 1}, {First: 3, Last: 4}}},
		{"1,1,0-1", []UserIndexRange{{First: 0, Last: 1}}},
		{"3-3", []UserIndexRange{{First: 3, Last: 3}}},
		{"9,8,7,6,5,4,3,2,1,0", []UserIndexRange{{First: 0, Last: 9}}},
		{"65536", []UserIndexRange{{First: 65536, Last: 65536}}},
		{"0-999999999", []UserIndexRange{{First: 0, Last: 999999999}}},
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
	} {
		_, err := parseUserIndexes(in)
		require.Error(t, err, in)
		assert.Contains(t, err.Error(), "TXPUMP_USERS", in)
	}
}

func TestLoadConfigUsers(t *testing.T) {
	t.Setenv("TXPUMP_USERS", "1")
	clearTxpumpEnv(t)
	cfg, err := LoadConfig()
	require.NoError(t, err)
	assert.Nil(t, cfg.Users)

	t.Setenv("TXPUMP_USERS", "1,3")
	cfg, err = LoadConfig()
	require.NoError(t, err)
	assert.Equal(t, []UserIndexRange{
		{First: 1, Last: 1}, {First: 3, Last: 3},
	}, cfg.Users)

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

	got, err = SelectUserKeys(keys, []UserIndexRange{
		{First: 0, Last: 0}, {First: 2, Last: 2},
	})
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Same(t, keys[0], got[0])
	assert.Same(t, keys[2], got[1])

	_, err = SelectUserKeys(keys, []UserIndexRange{{First: 1, Last: 3}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "TXPUMP_USERS")
	assert.Contains(t, err.Error(), "3")
	assert.Contains(t, err.Error(), "3 keys")

	_, err = SelectUserKeys(keys, []UserIndexRange{{First: -1, Last: -1}})
	require.Error(t, err)

	_, err = SelectUserKeys([]*UTxOKey{}, []UserIndexRange{{First: 0, Last: 0}})
	require.Error(t, err)
}

func TestSelectUserKeysSupportsLargeIndexesAndDeduplicatesRanges(t *testing.T) {
	keys := make([]*UTxOKey, 65537)
	selectedKey := &UTxOKey{Address: []byte{1}}
	keys[65536] = selectedKey

	indexes, err := parseUserIndexes("65536,65536")
	require.NoError(t, err)
	got, err := SelectUserKeys(keys, indexes)
	require.NoError(t, err)
	require.Equal(t, []*UTxOKey{selectedKey}, got)
}

func TestSelectUserKeysBoundsLargeRangesByLoadedKeys(t *testing.T) {
	_, err := SelectUserKeys(
		[]*UTxOKey{{}, {}, {}},
		[]UserIndexRange{{First: 0, Last: 999999999}},
	)
	require.ErrorContains(t, err, "999999999")
	require.ErrorContains(t, err, "3 keys loaded")
}
