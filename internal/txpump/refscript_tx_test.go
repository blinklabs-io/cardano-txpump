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
	"strings"
	"testing"

	"github.com/blinklabs-io/gouroboros/ledger/common"
	"github.com/blinklabs-io/gouroboros/ledger/conway"
	"github.com/stretchr/testify/require"
)

func publisherFixture(t *testing.T) (*UTxOKey, []UTxO, PlutusV3Validator, ScriptParams) {
	t.Helper()
	v, err := LoadValidator("escrow.json", "escrow.escrow.spend")
	require.NoError(t, err)
	key := testSigningKey(0x77)
	key.Address = enterpriseAddr(key)
	in := []UTxO{{TxHash: strings.Repeat("e1", 32), Index: 0, Amount: 60_000_000, SigningKey: key}}
	return key, in, v, DefaultScriptParams(testCostModel(t))
}

// publishFixture publishes the escrow validator and returns the reference it
// created together with the outputs a verifier needs to resolve it.
func publishFixture(t *testing.T) (RefScript, []KnownUTxO, PlutusV3Validator, ScriptParams) {
	t.Helper()
	key, in, v, p := publisherFixture(t)
	txBytes, _, ref, _, err := BuildRefPublishTx(in, v, key.Address, p)
	require.NoError(t, err)
	_, err = NewVerifier(p).Verify(txBytes, walletKnown(in), 100)
	require.NoError(t, err)
	known, err := KnownUTxOsFromTx(txBytes)
	require.NoError(t, err)
	return ref, known, v, p
}

func TestPublishTxCarriesTheScriptAndVerifies(t *testing.T) {
	key, in, v, p := publisherFixture(t)
	txBytes, txID, ref, change, err := BuildRefPublishTx(in, v, key.Address, p)
	require.NoError(t, err)
	tx, err := conway.NewConwayTransactionFromCbor(txBytes)
	require.NoError(t, err)
	require.Equal(t, txID, tx.Hash().String())
	require.Len(t, tx.Outputs(), 2)
	s := tx.Outputs()[0].ScriptRef()
	require.NotNil(t, s)
	require.Equal(t, v.Hash, s.Hash().Bytes())
	require.Equal(t, ref.Hash, v.Hash)
	require.Equal(t, uint32(0), ref.UTxO.Index)
	require.Equal(t, txID, ref.UTxO.TxHash)
	require.Equal(t, tx.Outputs()[0].Amount().Uint64(), ref.UTxO.Amount)
	require.Equal(t, tx.Outputs()[1].Amount().Uint64(), change)
	requireSignedBy(t, txBytes, key)
	_, err = NewVerifier(p).Verify(txBytes, walletKnown(in), 100)
	require.NoError(t, err)
}

func TestPublishTxRejectsInputsThatCannotFundIt(t *testing.T) {
	key, in, v, p := publisherFixture(t)
	in[0].Amount = 5_000_000
	_, _, _, _, err := BuildRefPublishTx(in, v, key.Address, p)
	require.Error(t, err)
}

func TestPublishTxRejectsScriptOverTheTransactionLimit(t *testing.T) {
	key, in, v, p := publisherFixture(t)
	v.Code = bytes.Repeat([]byte{1}, int(conway.MaxRefScriptSizePerTx)+1)
	_, _, _, _, err := BuildRefPublishTx(in, v, key.Address, p)
	require.Error(t, err)
}

func refSpendFixture(t *testing.T) (ScriptTxSpec, []KnownUTxO, RefScript, ScriptParams) {
	t.Helper()
	ref, refKnown, v, p := publishFixture(t)
	spec, known, _ := escrowInitiationFixture(t)
	spec.Validator = v
	spec.ReferenceScript = &ref
	spec.Fee = 0
	spec.ChangeIndex = 0
	spec.WalletInputs = nil
	spec.Outputs = []ScriptOutput{{Address: spec.Outputs[0].Address, Lovelace: 5_000_000}}
	return spec, append(known, refKnown...), ref, p
}

func TestReferenceSpendEvaluatesOffline(t *testing.T) {
	spec, known, ref, p := refSpendFixture(t)
	txBytes, _, err := PlanScriptTx(spec, NewVerifier(p), p, known, 100)
	require.NoError(t, err)
	tx, err := conway.NewConwayTransactionFromCbor(txBytes)
	require.NoError(t, err)
	require.Empty(t, tx.WitnessSet.PlutusV3Scripts(), "the validator is not in the witness set")
	refs := tx.ReferenceInputs()
	require.Len(t, refs, 1)
	require.Equal(t, ref.UTxO.TxHash, refs[0].Id().String())
	used, err := NewVerifier(p).Verify(txBytes, known, 100)
	require.NoError(t, err)
	require.Len(t, used, 1)

	spec.ReferenceScript = nil
	witnessTx, _, err := PlanScriptTx(spec, NewVerifier(p), p, known, 100)
	require.NoError(t, err)
	require.Less(t, len(txBytes), len(witnessTx)-3000)
}

func TestReferenceSpendFeePaysTheReferenceScriptFee(t *testing.T) {
	spec, known, ref, p := refSpendFixture(t)
	txBytes, _, err := PlanScriptTx(spec, NewVerifier(p), p, known, 100)
	require.NoError(t, err)
	tx, err := conway.NewConwayTransactionFromCbor(txBytes)
	require.NoError(t, err)
	fee := tx.Fee().Uint64()
	require.GreaterOrEqual(t, fee, refScriptFee(uint64(len(ref.Script)), p))

	// A fee that omits the reference-script term must fail the ledger rule.
	spec.ChangeIndex = -1
	spec.Fee = minFeeFor(len(txBytes), common.ExUnits{Memory: 600_000, Steps: 300_000_000}, p, 0)
	spec.Outputs[0].Lovelace = 5_000_000 - spec.Fee
	_, _, err = PlanScriptTx(spec, NewVerifier(p), p, known, 100)
	require.Error(t, err)
	require.Contains(t, err.Error(), "FeeTooSmall")
}

func TestReferenceSpendRejectsMismatchedScript(t *testing.T) {
	spec, _, ref, _ := refSpendFixture(t)
	bad := ref
	bad.Hash = bytes.Repeat([]byte{9}, 28)
	spec.ReferenceScript = &bad
	_, _, err := BuildScriptTx(spec)
	require.Error(t, err)

	same := ref
	same.UTxO = spec.ScriptInputs[0].UTxO
	spec.ReferenceScript = &same
	_, _, err = BuildScriptTx(spec)
	require.Error(t, err, "the reference input must not be spent")
}

// The evaluator skips a redeemer whose script it cannot find, so a reference
// holding another script must fail verification, not pass with nothing run.
func TestVerifierRejectsReferenceHoldingAnotherScript(t *testing.T) {
	spec, known, ref, p := refSpendFixture(t)
	for i := range known {
		if known[i].Ref == ref.UTxO.TxHash+"#0" {
			known[i].ScriptV3 = alwaysSucceedsScript()
		}
	}
	txBytes, _, err := BuildScriptTx(withFee(spec, 3_000_000))
	require.NoError(t, err)
	_, err = NewVerifier(p).Verify(txBytes, known, 100)
	require.Error(t, err)
}

// Spent inputs count toward the limit as well as reference inputs.
func TestVerifierRejectsReferenceScriptsOverTheTransactionLimit(t *testing.T) {
	spec, known, _, p := refSpendFixture(t)
	key := spec.Collateral[0].SigningKey
	heavy := UTxO{TxHash: strings.Repeat("f7", 32), Index: 0, Amount: 20_000_000, SigningKey: key}
	known = append(known, KnownUTxO{
		Ref: heavy.TxHash + "#0", Address: enterpriseAddr(key), Lovelace: heavy.Amount,
		ScriptV3: bytes.Repeat([]byte{1}, int(conway.MaxRefScriptSizePerTx)),
	})
	spec.WalletInputs = []UTxO{heavy}
	for i := range known {
		if strings.HasSuffix(known[i].Ref, "#2") {
			known[i].Lovelace = 30_000_000
		}
	}
	spec.ChangeIndex = -1
	spec.Fee = 8_000_000
	spec.Outputs = []ScriptOutput{{Address: spec.Outputs[0].Address, Lovelace: 17_000_000}}
	txBytes, _, err := BuildScriptTx(spec)
	require.NoError(t, err)
	_, err = NewVerifier(p).Verify(txBytes, known, 100)
	require.Error(t, err)
	require.Contains(t, err.Error(), "reference-script size per transaction too large")
}

func withFee(spec ScriptTxSpec, fee uint64) ScriptTxSpec {
	spec.ChangeIndex = -1
	spec.Fee = fee
	spec.Outputs = []ScriptOutput{{Address: spec.Outputs[0].Address, Lovelace: 5_000_000 - fee}}
	return spec
}

func TestKnownUTxOsFromTxCarriesTheReferenceScript(t *testing.T) {
	key, in, v, p := publisherFixture(t)
	txBytes, _, _, _, err := BuildRefPublishTx(in, v, key.Address, p)
	require.NoError(t, err)
	known, err := KnownUTxOsFromTx(txBytes)
	require.NoError(t, err)
	require.Equal(t, v.Code, known[0].ScriptV3)
	require.Nil(t, known[1].ScriptV3)
}
