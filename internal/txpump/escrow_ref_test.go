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

	"github.com/blinklabs-io/gouroboros/ledger/conway"
	"github.com/stretchr/testify/require"
)

// publishForHarness publishes the escrow validator from a key outside the
// harness's users and makes the result visible to both the app and the
// verifier, as discovery would on a node.
func (h *escrowHarness) publishForHarness() RefScript {
	h.t.Helper()
	key, in, _, _ := publisherFixture(h.t)
	txBytes, _, _, _, err := BuildRefPublishTx(in, h.app.validator, key.Address, h.cfg.ScriptParams)
	require.NoError(h.t, err)
	h.refs = NewRefScripts()
	h.refs.Sync(onChain(h.t, txBytes, key.Address), h.app.ReferenceValidators(), nil)
	ref, ok := h.refs.Lookup(h.app.validator.Hash)
	require.True(h.t, ok)
	h.known = append(h.known, ref.Known())
	return ref
}

func requireReferenceSpend(t *testing.T, tx *conway.ConwayTransaction, ref RefScript) {
	t.Helper()
	require.Empty(t, tx.WitnessSet.PlutusV3Scripts())
	require.Len(t, tx.ReferenceInputs(), 1)
	require.Equal(t, ref.UTxO.TxHash, tx.ReferenceInputs()[0].Id().String())
	require.Equal(t, ref.UTxO.Index, tx.ReferenceInputs()[0].Index())
}

func requireWitnessSpend(t *testing.T, tx *conway.ConwayTransaction) {
	t.Helper()
	require.Len(t, tx.WitnessSet.PlutusV3Scripts(), 1)
	require.Empty(t, tx.ReferenceInputs())
}

func TestEscrowSpendsThroughTheReferenceScript(t *testing.T) {
	h := newEscrowHarness(t)
	h.cfg.ReferenceScripts = ReferenceScriptsOn
	ref := h.publishForHarness()
	initiator, recipient := h.users[0], h.users[1]

	lock := h.step(initiator, pickInitiate, escrowAmountMin)
	require.Empty(t, lock.ReferenceInputs(), "the lock is a key spend and needs no script")
	requireReferenceSpend(t, h.step(recipient, pickAdvance, 0, pickProceed, escrowAmountMin+1_000_000), ref)
	requireReferenceSpend(t, h.step(initiator, pickAdvance, 0, pickProceed), ref)
	require.Empty(t, h.app.positions)
}

func TestEscrowReferenceCancelOfAnInitiationPaysFeeFromTheLock(t *testing.T) {
	h := newEscrowHarness(t)
	h.cfg.ReferenceScripts = ReferenceScriptsOn
	ref := h.publishForHarness()
	initiator := h.users[0]
	h.step(initiator, pickInitiate, escrowAmountMin)
	requireReferenceSpend(t, h.step(initiator, pickAdvance, 0, pickCancel), ref)
}

func TestEscrowWaitsForThePublishWhenReferencesAreRequired(t *testing.T) {
	h := newEscrowHarness(t)
	h.cfg.ReferenceScripts = ReferenceScriptsOn
	initiator, recipient := h.users[0], h.users[1]
	h.step(initiator, pickInitiate, escrowAmountMin)

	h.picks = []int{pickAdvance, 0, pickProceed, escrowAmountMin}
	ok, err := h.app.Step(h.env(recipient))
	require.NoError(t, err)
	require.False(t, ok, "no reference is published yet")
	require.Len(t, h.app.positions, 1)
	require.False(t, h.app.positions[0].busy, "the position is released")
	require.Zero(t, h.app.positions[0].rejections, "waiting is not a rejection")
	require.Len(t, h.txs, 1)

	h.publishForHarness()
	h.step(recipient, pickAdvance, 0, pickProceed, escrowAmountMin)
}

func TestEscrowCarriesTheValidatorWhenReferencesAreOff(t *testing.T) {
	h := newEscrowHarness(t)
	h.publishForHarness() // published, but the setting says to carry it
	initiator, recipient := h.users[0], h.users[1]
	h.step(initiator, pickInitiate, escrowAmountMin)
	requireWitnessSpend(t, h.step(recipient, pickAdvance, 0, pickProceed, escrowAmountMin))
}

func TestEscrowMixedModePicksPerSpend(t *testing.T) {
	h := newEscrowHarness(t)
	h.cfg.ReferenceScripts = ReferenceScriptsMixed
	ref := h.publishForHarness()
	initiator, recipient := h.users[0], h.users[1]
	h.step(initiator, pickInitiate, escrowAmountMin)
	// The source pick follows the claim picks: 0 selects the reference.
	requireReferenceSpend(t, h.step(recipient, pickAdvance, 0, pickProceed, 0, escrowAmountMin), ref)
	requireWitnessSpend(t, h.step(initiator, pickAdvance, 0, pickProceed, 1))
}

func TestEscrowMixedModeFallsBackToTheWitnessWithoutAReference(t *testing.T) {
	h := newEscrowHarness(t)
	h.cfg.ReferenceScripts = ReferenceScriptsMixed
	initiator, recipient := h.users[0], h.users[1]
	h.step(initiator, pickInitiate, escrowAmountMin)
	requireWitnessSpend(t, h.step(recipient, pickAdvance, 0, pickProceed, 0, escrowAmountMin))
}
