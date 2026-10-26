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
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestLoadConfigReferenceScripts(t *testing.T) {
	t.Setenv("TXPUMP_REFERENCE_SCRIPTS", "on")
	clearTxpumpEnv(t)
	cfg, err := LoadConfig()
	require.NoError(t, err)
	require.Equal(t, ReferenceScriptsOff, cfg.ReferenceScripts)
	require.False(t, cfg.referenceScriptsEnabled())

	for _, mode := range []string{"off", "on", "mixed"} {
		t.Setenv("TXPUMP_REFERENCE_SCRIPTS", mode)
		cfg, err = LoadConfig()
		require.NoError(t, err, mode)
		require.Equal(t, mode, cfg.ReferenceScripts)
		require.Equal(t, mode != "off", cfg.referenceScriptsEnabled())
	}
	t.Setenv("TXPUMP_REFERENCE_SCRIPTS", "sometimes")
	_, err = LoadConfig()
	require.Error(t, err)
}

func newRefPump(t *testing.T, mode string, keys ...*UTxOKey) *Pump {
	t.Helper()
	w := NewWallet()
	for i, k := range keys {
		w.Add(UTxO{TxHash: strings.Repeat(string(rune('a'+i)), 64), Amount: 50_000_000, SigningKey: k})
	}
	cfg := &Config{ReferenceScripts: mode, Types: []string{"escrow"}}
	return NewPump(cfg, w, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, time.Now())
}

func TestSetupReferenceScriptsDesignatesTheLastKeyAsPublisher(t *testing.T) {
	k1, k2, k3 := escrowTestKey(1), escrowTestKey(2), escrowTestKey(3)
	p := newRefPump(t, ReferenceScriptsOn, k1, k2, k3)
	require.NoError(t, p.setupReferenceScripts())
	require.NotNil(t, p.refs)
	pub := p.wallet.ReservedKey()
	require.NotNil(t, pub)
	last := p.wallet.Keys()
	require.Len(t, last, 2)
	for _, k := range last {
		require.Equal(t, 1, bytes.Compare(pub.Address, k.Address), "the publisher sorts after every user")
	}
}

func TestSetupReferenceScriptsIsANoopWhenOff(t *testing.T) {
	p := newRefPump(t, ReferenceScriptsOff, escrowTestKey(1), escrowTestKey(2))
	require.NoError(t, p.setupReferenceScripts())
	require.Nil(t, p.refs)
	require.Nil(t, p.wallet.ReservedKey())
}

func TestSetupReferenceScriptsNeedsAKeyBesideThePublisher(t *testing.T) {
	p := newRefPump(t, ReferenceScriptsOn, escrowTestKey(1))
	require.Error(t, p.setupReferenceScripts())
}

func TestSetupReferenceScriptsRequiresTwoEscrowUsers(t *testing.T) {
	p := newRefPump(t, ReferenceScriptsOn, escrowTestKey(1), escrowTestKey(2))
	err := p.setupReferenceScripts()
	require.ErrorContains(t, err, "escrow")

	p = newRefPump(t, ReferenceScriptsOn, escrowTestKey(1), escrowTestKey(2))
	p.cfg.Types = []string{"payment"}
	require.NoError(t, p.setupReferenceScripts(), "non-reference workloads retain the two-key minimum")
}

func TestSplitReferenceSnapshotKeepsReferenceOutputsOutOfTheWallet(t *testing.T) {
	pub, other := []byte{0x60, 1}, []byte{0x60, 2}
	snapshot := []UTxO{
		{TxHash: "a", Address: pub, ScriptRef: []byte{1}},
		{TxHash: "b", Address: pub},
		{TxHash: "c", Address: other, ScriptRef: []byte{1}},
	}
	wallet, refs := splitReferenceSnapshot(snapshot, pub)
	require.Len(t, refs, 1)
	require.Equal(t, "a", refs[0].TxHash)
	require.Equal(t, []UTxO{snapshot[1]}, wallet)
}

func TestApplySnapshotExcludesForeignReferenceOutputsWhenDisabled(t *testing.T) {
	key := escrowTestKey(1)
	wallet := NewWallet()
	wallet.AddKey(key)
	p := NewPump(
		&Config{ReferenceScripts: ReferenceScriptsOff},
		wallet,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		nil,
		time.Now(),
	)
	plain := UTxO{
		TxHash:  strings.Repeat("01", 32),
		Amount:  5_000_000,
		Address: key.Address,
	}
	withReference := UTxO{
		TxHash:    strings.Repeat("02", 32),
		Amount:    50_000_000,
		Address:   key.Address,
		ScriptRef: []byte{1},
	}

	p.applySnapshot([]UTxO{plain, withReference}, nil, []byte{0x70})

	require.Equal(t, uint64(5_000_000), p.wallet.Balance())
	selected, _, err := p.wallet.SelectCoins(1)
	require.NoError(t, err)
	require.Len(t, selected, 1)
	require.Equal(t, plain.TxHash, selected[0].TxHash)
}

func TestReferenceValidatorsAreThoseOfEnabledReferenceApps(t *testing.T) {
	p := newRefPump(t, ReferenceScriptsOn, escrowTestKey(1), escrowTestKey(2), escrowTestKey(3))
	require.NoError(t, p.setupReferenceScripts())
	p.users = usersFromWallet(p.wallet)
	vs := p.referenceValidators()
	require.Len(t, vs, 1)
	app := mustNewEscrowApp()
	require.Equal(t, app.validator.Hash, vs[0].Hash)

	p.users = p.users[:1]
	require.Empty(t, p.referenceValidators(), "a gated app publishes nothing")
	p.cfg.Types = []string{"payment"}
	p.users = usersFromWallet(p.wallet)
	require.Empty(t, p.referenceValidators())
}

func TestApplySnapshotRoutesReferenceOutputsToTheRegistryOnly(t *testing.T) {
	k1, k2, k3 := escrowTestKey(1), escrowTestKey(2), escrowTestKey(3)
	p := newRefPump(t, ReferenceScriptsOn, k1, k2, k3)
	require.NoError(t, p.setupReferenceScripts())
	p.users = usersFromWallet(p.wallet)
	pub := p.wallet.ReservedKey()
	require.NotNil(t, pub)
	v := mustNewEscrowApp().validator
	snapshot := []UTxO{
		{TxHash: strings.Repeat("0a", 32), Index: 0, Amount: 16_000_000, Address: pub.Address, ScriptRef: v.Code},
		{TxHash: strings.Repeat("0a", 32), Index: 1, Amount: 30_000_000, Address: pub.Address},
	}
	p.applySnapshot(snapshot, nil, []byte{0x70})

	ref, ok := p.refs.Lookup(v.Hash)
	require.True(t, ok)
	require.Equal(t, uint32(0), ref.UTxO.Index)
	// Only the plain output is spendable by the publisher.
	inputs, _, err := p.wallet.SelectReservedCoins(1)
	require.NoError(t, err)
	require.Len(t, inputs, 1)
	require.Equal(t, uint32(1), inputs[0].Index)
	require.Equal(t, uint64(30_000_000), inputs[0].Amount)
	p.wallet.ReturnUTxOs(inputs)
	_, _, err = p.wallet.SelectReservedCoins(40_000_000)
	require.ErrorIs(t, err, ErrInsufficientFunds, "the reference output's 16M is not spendable")
}
