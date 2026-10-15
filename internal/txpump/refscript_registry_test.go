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
	"errors"
	"strings"
	"testing"

	"github.com/blinklabs-io/gouroboros/ledger/conway"
	"github.com/stretchr/testify/require"
)

type publisherHarness struct {
	t         *testing.T
	wallet    *Wallet
	key       *UTxOKey
	params    ScriptParams
	validator PlutusV3Validator
	refs      *RefScripts
	submitted [][]byte
	reject    error
}

func newPublisherHarness(t *testing.T) *publisherHarness {
	t.Helper()
	key, in, v, p := publisherFixture(t)
	w := NewWallet()
	w.Add(in...)
	w.Add(UTxO{TxHash: strings.Repeat("a9", 32), Amount: 80_000_000, SigningKey: escrowTestKey(0x01)})
	w.ReserveKey(key)
	return &publisherHarness{t: t, wallet: w, key: key, params: p, validator: v, refs: NewRefScripts()}
}

func (h *publisherHarness) publish() (bool, error) {
	return h.refs.Publish(h.validator, h.wallet, h.params, 100, 0, func(_ string, tx []byte) error {
		if h.reject != nil {
			return h.reject
		}
		h.submitted = append(h.submitted, tx)
		return nil
	})
}

// onChain is what the node reports for the publisher address once txBytes is
// in a block.
func onChain(t *testing.T, txBytes []byte, address []byte) []UTxO {
	t.Helper()
	tx, err := conway.NewConwayTransactionFromCbor(txBytes)
	require.NoError(t, err)
	var out []UTxO
	for i, o := range tx.Outputs() {
		u := UTxO{TxHash: tx.Hash().String(), Index: uint32(i), Amount: o.Amount().Uint64(), Address: address}
		if s := o.ScriptRef(); s != nil {
			u.ScriptRef = s.RawScriptBytes()
		}
		out = append(out, u)
	}
	return out
}

func TestPublishSubmitsOneVerifiedTransactionPerValidator(t *testing.T) {
	h := newPublisherHarness(t)
	ok, err := h.publish()
	require.NoError(t, err)
	require.True(t, ok)
	require.Len(t, h.submitted, 1)
	_, err = NewVerifier(h.params).Verify(h.submitted[0], walletKnown([]UTxO{{
		TxHash: strings.Repeat("e1", 32), Amount: 60_000_000, SigningKey: h.key,
	}}), 100)
	require.NoError(t, err)

	ok, err = h.publish()
	require.NoError(t, err)
	require.False(t, ok, "a publish in flight is not repeated")
	require.Len(t, h.submitted, 1)
	require.Empty(t, h.refs.Missing([]PlutusV3Validator{h.validator}))
}

func TestPublishSpendsOnlyThePublisherKeysFunds(t *testing.T) {
	h := newPublisherHarness(t)
	_, err := h.publish()
	require.NoError(t, err)
	// The other user's 80M is untouched by the publish.
	inputs, _, err := h.wallet.SelectCoins(70_000_000)
	require.NoError(t, err)
	require.Equal(t, escrowTestKey(0x01).Address, inputs[0].SigningKey.Address)
}

func TestRejectedPublishReturnsFundsAndMayBeRetried(t *testing.T) {
	h := newPublisherHarness(t)
	before := h.wallet.Len()
	h.reject = errors.New("node said no")
	ok, err := h.publish()
	require.NoError(t, err)
	require.False(t, ok)
	require.Equal(t, before, h.wallet.Len())
	require.Len(t, h.refs.Missing([]PlutusV3Validator{h.validator}), 1)
	h.reject = nil
	ok, err = h.publish()
	require.NoError(t, err)
	require.True(t, ok)
}

func TestPublishFailsLoudlyWhenThePublisherCannotPay(t *testing.T) {
	h := newPublisherHarness(t)
	_, _, err := h.wallet.SelectReservedCoins(1)
	require.NoError(t, err)
	ok, err := h.publish()
	require.ErrorIs(t, err, ErrInsufficientFunds)
	require.False(t, ok)
}

func TestSyncFindsThePublishedScriptAfterARestart(t *testing.T) {
	h := newPublisherHarness(t)
	_, err := h.publish()
	require.NoError(t, err)
	chain := onChain(t, h.submitted[0], h.key.Address)

	// A new process has no memory of the publish and must learn it from the
	// chain instead of publishing again.
	restarted := NewRefScripts()
	require.Len(t, restarted.Missing([]PlutusV3Validator{h.validator}), 1)
	restarted.Sync(chain, []PlutusV3Validator{h.validator}, nil)
	require.Empty(t, restarted.Missing([]PlutusV3Validator{h.validator}))
	ref, ok := restarted.Lookup(h.validator.Hash)
	require.True(t, ok)
	require.Equal(t, chain[0].TxHash, ref.UTxO.TxHash)
	require.Equal(t, h.validator.Code, ref.Script)
	require.Equal(t, h.key.Address, ref.UTxO.Address)
}

func TestSyncMatchesOnTheScriptHash(t *testing.T) {
	h := newPublisherHarness(t)
	other := PlutusV3Validator{Title: "other", Code: alwaysSucceedsScript(), Hash: alwaysSucceedsScriptHash()}
	snapshot := []UTxO{
		{TxHash: strings.Repeat("01", 32), Amount: 20_000_000, Address: h.key.Address, ScriptRef: other.Code},
		{TxHash: strings.Repeat("02", 32), Amount: 20_000_000, Address: h.key.Address},
	}
	h.refs.Sync(snapshot, []PlutusV3Validator{h.validator, other}, nil)
	_, ok := h.refs.Lookup(h.validator.Hash)
	require.False(t, ok, "an output without the expected script is not a reference")
	ref, ok := h.refs.Lookup(other.Hash)
	require.True(t, ok)
	require.Equal(t, strings.Repeat("01", 32), ref.UTxO.TxHash)
	require.Len(t, h.refs.Missing([]PlutusV3Validator{h.validator, other}), 1)
}

func TestSyncIgnoresAnOutputWhoseScriptHashDiffers(t *testing.T) {
	h := newPublisherHarness(t)
	h.refs.Sync([]UTxO{{
		TxHash: strings.Repeat("03", 32), Amount: 20_000_000, Address: h.key.Address,
		ScriptRef: bytes.Repeat([]byte{7}, 100),
	}}, []PlutusV3Validator{h.validator}, nil)
	_, ok := h.refs.Lookup(h.validator.Hash)
	require.False(t, ok)
}

func TestSyncDropsAReferenceThatLeftTheChain(t *testing.T) {
	h := newPublisherHarness(t)
	_, err := h.publish()
	require.NoError(t, err)
	chain := onChain(t, h.submitted[0], h.key.Address)
	h.refs.Sync(chain, []PlutusV3Validator{h.validator}, nil)
	h.refs.Sync(nil, []PlutusV3Validator{h.validator}, nil)
	require.Len(t, h.refs.Missing([]PlutusV3Validator{h.validator}), 1)
}

func TestSyncKeepsAPendingPublishWhileTheNodeStillHoldsIt(t *testing.T) {
	h := newPublisherHarness(t)
	_, err := h.publish()
	require.NoError(t, err)
	txID := h.refs.pendingTxID(h.validator.Hash)
	require.NotEmpty(t, txID)
	for range 3 {
		h.refs.Sync(nil, []PlutusV3Validator{h.validator}, map[string]bool{txID: true})
	}
	require.Empty(t, h.refs.Missing([]PlutusV3Validator{h.validator}))
	// Two consecutive absences with no output on chain retire the attempt.
	h.refs.Sync(nil, []PlutusV3Validator{h.validator}, map[string]bool{txID: false})
	require.Empty(t, h.refs.Missing([]PlutusV3Validator{h.validator}))
	h.refs.Sync(nil, []PlutusV3Validator{h.validator}, map[string]bool{txID: false})
	require.Len(t, h.refs.Missing([]PlutusV3Validator{h.validator}), 1)
}

func TestDiscoveredReferenceFeedsAVerifiedSpend(t *testing.T) {
	h := newPublisherHarness(t)
	_, err := h.publish()
	require.NoError(t, err)
	chain := onChain(t, h.submitted[0], h.key.Address)
	h.refs.Sync(chain, []PlutusV3Validator{h.validator}, nil)
	ref, ok := h.refs.Lookup(h.validator.Hash)
	require.True(t, ok)

	spec, known, _ := escrowInitiationFixture(t)
	spec.Validator = h.validator
	spec.ReferenceScript = &ref
	spec.Fee, spec.ChangeIndex, spec.WalletInputs = 0, 0, nil
	spec.Outputs = []ScriptOutput{{Address: spec.Outputs[0].Address, Lovelace: 5_000_000}}
	known = append(known, ref.Known())
	_, _, err = PlanScriptTx(spec, NewVerifier(h.params), h.params, known, 100)
	require.NoError(t, err)
}

func (r *RefScripts) pendingTxID(hash []byte) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p := r.pending[string(hash)]; p != nil {
		return p.txID
	}
	return ""
}

func TestSyncIgnoresScriptsNobodyWants(t *testing.T) {
	h := newPublisherHarness(t)
	other := PlutusV3Validator{Title: "other", Code: alwaysSucceedsScript(), Hash: alwaysSucceedsScriptHash()}
	h.refs.Sync([]UTxO{{
		TxHash: strings.Repeat("04", 32), Amount: 20_000_000, Address: h.key.Address, ScriptRef: other.Code,
	}}, []PlutusV3Validator{h.validator}, nil)
	_, ok := h.refs.Lookup(other.Hash)
	require.False(t, ok)
}
