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
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/blinklabs-io/gouroboros/ledger/common"
	"github.com/blinklabs-io/gouroboros/ledger/conway"
	"github.com/stretchr/testify/require"
)

func testCostModel(t *testing.T) []int64 {
	t.Helper()
	b, err := os.ReadFile("../../testdata/plutusv3-costmodel.json")
	require.NoError(t, err)
	var g struct {
		PlutusV3CostModel []int64 `json:"plutusV3CostModel"`
	}
	require.NoError(t, json.Unmarshal(b, &g))
	require.Len(t, g.PlutusV3CostModel, 251)
	return g.PlutusV3CostModel
}

func enterpriseAddr(k *UTxOKey) []byte { return append([]byte{0x60}, keyHash(k)...) }

// escrowInitiationFixture locks 5 ADA at the escrow address with an
// Initiation datum and builds the initiator's cancel of it.
func escrowInitiationFixture(t *testing.T) (ScriptTxSpec, []KnownUTxO, *UTxOKey) {
	t.Helper()
	v, err := LoadValidator("escrow.json", "escrow.escrow.spend")
	require.NoError(t, err)
	key := testSigningKey(0x42)
	datum, err := encodePlutusData(escrowInitiationDatum(keyAddressData(keyHash(key)), 5_000_000))
	require.NoError(t, err)
	lockHash := strings.Repeat("ab", 32)
	known := []KnownUTxO{
		{Ref: lockHash + "#0", Address: v.Address(), Lovelace: 5_000_000, Datum: datum},
		{Ref: lockHash + "#1", Address: enterpriseAddr(key), Lovelace: 20_000_000},
		{Ref: lockHash + "#2", Address: enterpriseAddr(key), Lovelace: 10_000_000},
	}
	spec := ScriptTxSpec{
		Validator:       v,
		ScriptInputs:    []ScriptInput{{UTxO: UTxO{TxHash: lockHash, Index: 0, Amount: 5_000_000}, Redeemer: escrowCancelRedeemer()}},
		WalletInputs:    []UTxO{{TxHash: lockHash, Index: 1, Amount: 20_000_000, SigningKey: key}},
		Collateral:      []UTxO{{TxHash: lockHash, Index: 2, Amount: 10_000_000, SigningKey: key}},
		Outputs:         []ScriptOutput{{Address: enterpriseAddr(key), Lovelace: 23_000_000}},
		RequiredSigners: [][]byte{keyHash(key)},
		ExUnits:         common.ExUnits{Memory: 500_000, Steps: 200_000_000},
		Fee:             2_000_000,
		CostModel:       testCostModel(t),
		SigningKeys:     []*UTxOKey{key},
	}
	return spec, known, key
}

func verifyFixture(t *testing.T, spec ScriptTxSpec, known []KnownUTxO) (map[common.RedeemerKey]common.ExUnits, error) {
	t.Helper()
	txBytes, _, err := BuildScriptTx(spec)
	require.NoError(t, err)
	return NewVerifier(DefaultScriptParams(spec.CostModel)).Verify(txBytes, known, 100)
}

func TestVerifierAcceptsValidEscrowCancel(t *testing.T) {
	spec, known, _ := escrowInitiationFixture(t)
	used, err := verifyFixture(t, spec, known)
	require.NoError(t, err)
	require.Len(t, used, 1)
	for _, u := range used {
		require.Greater(t, u.Memory, int64(0))
		require.Greater(t, u.Steps, int64(0))
	}
}

func TestVerifierRejectsUnsignedCancel(t *testing.T) {
	spec, known, _ := escrowInitiationFixture(t)
	spec.RequiredSigners = nil
	_, err := verifyFixture(t, spec, known)
	require.Error(t, err)
}

func TestVerifierRejectsWrongScriptHash(t *testing.T) {
	spec, known, _ := escrowInitiationFixture(t)
	known[0].Address = append([]byte{0x70}, bytes.Repeat([]byte{0x01}, 28)...) // locked elsewhere
	_, err := verifyFixture(t, spec, known)
	require.Error(t, err, "the evaluator skips unmatched scripts; the rule set must catch it")
}

func TestVerifierRejectsMissingInput(t *testing.T) {
	spec, known, _ := escrowInitiationFixture(t)
	_, err := verifyFixture(t, spec, known[:1])
	require.Error(t, err)
}

// A redeemer with the wrong constructor index must fail locally.
func TestVerifierRejectsWrongRedeemerConstructor(t *testing.T) {
	spec, known, _ := escrowInitiationFixture(t)
	spec.ScriptInputs[0].Redeemer = escrowCompleteRedeemer() // Initiation cannot complete
	_, err := verifyFixture(t, spec, known)
	require.Error(t, err)
}

func TestVerifierRejectsInsufficientCollateral(t *testing.T) {
	spec, known, _ := escrowInitiationFixture(t)
	known[2].Lovelace = 2_000_000 // below 150% of the 2 ADA fee
	spec.Collateral[0].Amount = 2_000_000
	_, err := verifyFixture(t, spec, known)
	require.Error(t, err)
}

func TestKnownUTxOsFromTxCarriesInlineDatum(t *testing.T) {
	spec, _, key := escrowInitiationFixture(t)
	spec.Outputs = []ScriptOutput{
		{Address: spec.Validator.Address(), Lovelace: 5_000_000, Datum: escrowInitiationDatum(keyAddressData(keyHash(key)), 5_000_000)},
		{Address: enterpriseAddr(key), Lovelace: 18_000_000},
	}
	txBytes, txID, err := BuildScriptTx(spec)
	require.NoError(t, err)
	got, err := KnownUTxOsFromTx(txBytes)
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, txID+"#0", got[0].Ref)
	require.Equal(t, spec.Validator.Address(), got[0].Address)
	require.Equal(t, uint64(5_000_000), got[0].Lovelace)
	want, err := encodePlutusData(spec.Outputs[0].Datum)
	require.NoError(t, err)
	require.Equal(t, want, got[0].Datum)
	require.Nil(t, got[1].Datum)
}

// The stub ledger panics on any method it does not implement; evaluation
// must fail closed rather than crash the pump.
func TestVerifierEvaluationPanicFailsClosed(t *testing.T) {
	spec, known, _ := escrowInitiationFixture(t)
	txBytes, _, err := BuildScriptTx(spec)
	require.NoError(t, err)
	orig := evaluatePlutus
	t.Cleanup(func() { evaluatePlutus = orig })
	evaluatePlutus = func(tx common.Transaction, ls common.LedgerState, pp *conway.ConwayProtocolParameters, budget *common.ExUnits) (map[common.RedeemerKey]common.ExUnits, error) {
		if _, err := ls.StakeRegistration(nil); err != nil { // not implemented by the stub: panics
			return nil, err
		}
		return orig(tx, ls, pp, budget)
	}
	v := NewVerifier(DefaultScriptParams(spec.CostModel))
	require.NotPanics(t, func() {
		_, err = v.Measure(txBytes, known, 100)
	})
	require.ErrorContains(t, err, "plutus")
	require.NotPanics(t, func() {
		_, err = v.Verify(txBytes, known, 100)
	})
	require.Error(t, err)
}
