// Copyright 2026 Blink Labs Software
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package txpump

import (
	"math/big"
	"os"
	"path/filepath"
	"testing"

	"github.com/blinklabs-io/gouroboros/ledger/common"
	"github.com/blinklabs-io/gouroboros/ledger/conway"
	"github.com/stretchr/testify/require"
)

func TestPlanScriptTxDeclaresMeasuredUnitsAndPaysFee(t *testing.T) {
	spec, known, _ := escrowInitiationFixture(t)
	spec.ExUnits = common.ExUnits{}
	spec.ChangeIndex = 0
	spec.Fee = 0
	spec.Outputs[0].Lovelace = 25_000_000 // all input; the planner takes the fee from it
	p := DefaultScriptParams(spec.CostModel)
	v := NewVerifier(p)
	txBytes, txID, err := PlanScriptTx(spec, v, p, known, 100)
	require.NoError(t, err)

	tx, err := conway.NewConwayTransactionFromCbor(txBytes)
	require.NoError(t, err)
	require.Equal(t, tx.Hash().String(), txID)
	used, err := v.Verify(txBytes, known, 100)
	require.NoError(t, err)
	n := 0
	for k, declared := range tx.WitnessSet.Redeemers().Iter() {
		n++
		require.GreaterOrEqual(t, declared.ExUnits.Memory, used[k].Memory)
		require.GreaterOrEqual(t, declared.ExUnits.Steps, used[k].Steps)
		require.Less(t, declared.ExUnits.Memory, p.MaxTxExUnits.Memory/4,
			"units must be the measured ones, not the maximum")
		require.Less(t, declared.ExUnits.Steps, p.MaxTxExUnits.Steps/4)
	}
	require.Equal(t, 1, n)
	ls, err := stubFor(known, p)
	require.NoError(t, err)
	min, err := conway.MinFeeTxWithUtxo(tx, conwayParams(p), ls)
	require.NoError(t, err)
	require.GreaterOrEqual(t, tx.Fee().Uint64(), min)
	require.Equal(t, uint64(25_000_000)-tx.Fee().Uint64(), tx.Outputs()[0].Amount().Uint64(),
		"the fee comes out of the change output")
}

func TestPlanScriptTxFailsClosedOnInvalidScript(t *testing.T) {
	spec, known, _ := escrowInitiationFixture(t)
	spec.RequiredSigners = nil // cancel unsigned: script fails
	spec.ChangeIndex = 0
	spec.Fee = 0
	spec.Outputs[0].Lovelace = 25_000_000
	p := DefaultScriptParams(spec.CostModel)
	_, _, err := PlanScriptTx(spec, NewVerifier(p), p, known, 100)
	require.Error(t, err)
}

func TestPlanScriptTxRejectsChangeBelowMinimum(t *testing.T) {
	spec, known, _ := escrowInitiationFixture(t)
	spec.ChangeIndex = 0
	spec.Fee = 0
	spec.Outputs[0].Lovelace = 1_100_000
	p := DefaultScriptParams(spec.CostModel)
	_, _, err := PlanScriptTx(spec, NewVerifier(p), p, known, 100)
	require.Error(t, err)
}

func TestPlanScriptTxFixedFeeMustCoverMinimum(t *testing.T) {
	spec, known, _ := escrowInitiationFixture(t)
	spec.ChangeIndex = -1
	spec.Fee = 200_000 // below the script transaction's minimum fee
	spec.Outputs[0].Lovelace = 25_000_000 - spec.Fee
	p := DefaultScriptParams(spec.CostModel)
	_, _, err := PlanScriptTx(spec, NewVerifier(p), p, known, 100)
	require.Error(t, err)
}

// A caller-supplied fee is final: one that covers the minimum is accepted
// even when it is below the minimum plus the planner's own margin.
func TestPlanScriptTxFixedFeeAtMinimumPlusSmallMarginIsAccepted(t *testing.T) {
	spec, known, _ := escrowInitiationFixture(t)
	p := DefaultScriptParams(spec.CostModel)
	v := NewVerifier(p)
	spec.ChangeIndex = -1
	spec.Fee = 2_000_000
	spec.Outputs[0].Lovelace = 25_000_000 - spec.Fee
	txBytes, _, err := PlanScriptTx(spec, v, p, known, 100)
	require.NoError(t, err)
	used, err := v.Measure(txBytes, known, 100)
	require.NoError(t, err)
	var units common.ExUnits
	for _, u := range used {
		units = common.ExUnits{
			Memory: min(scaleUp(u.Memory), p.MaxTxExUnits.Memory),
			Steps:  min(scaleUp(u.Steps), p.MaxTxExUnits.Steps),
		}
	}
	spec.Fee = minFeeFor(len(txBytes), units, p, 0) * 102 / 100
	spec.Outputs[0].Lovelace = 25_000_000 - spec.Fee
	_, _, err = PlanScriptTx(spec, v, p, known, 100)
	require.NoError(t, err)
}

func TestScriptParamsFromAlonzoGenesisPartialExUnitsKeepBase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "alonzo.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"maxTxExUnits":{"exUnitsMem":12000000}}`), 0o600))
	base := DefaultScriptParams(nil)
	p, err := loadAlonzoScriptParams(path, base)
	require.NoError(t, err)
	require.Equal(t, int64(12_000_000), p.MaxTxExUnits.Memory)
	require.Equal(t, base.MaxTxExUnits.Steps, p.MaxTxExUnits.Steps)
}

func TestScriptParamsFromAlonzoGenesisRejectsNonPositiveExUnits(t *testing.T) {
	for _, body := range []string{
		`{"maxTxExUnits":{"exUnitsMem":0,"exUnitsSteps":1}}`,
		`{"maxTxExUnits":{"exUnitsMem":1,"exUnitsSteps":-5}}`,
	} {
		path := filepath.Join(t.TempDir(), "alonzo.json")
		require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
		_, err := loadAlonzoScriptParams(path, DefaultScriptParams(nil))
		require.Error(t, err, body)
	}
}

func TestScriptParamsFromAlonzoGenesis(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "alonzo.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"executionPrices":{"prMem":0.0577,"prSteps":0.0000721},"maxTxExUnits":{"exUnitsMem":12000000,"exUnitsSteps":9000000000},"collateralPercentage":175,"maxCollateralInputs":2}`), 0o600))
	p, err := loadAlonzoScriptParams(path, DefaultScriptParams(nil))
	require.NoError(t, err)
	require.Equal(t, int64(12_000_000), p.MaxTxExUnits.Memory)
	require.Equal(t, int64(9_000_000_000), p.MaxTxExUnits.Steps)
	require.Equal(t, uint(175), p.CollateralPercentage)
	require.Equal(t, uint(2), p.MaxCollateralInputs)
	require.Equal(t, 0, p.MemPrice.Cmp(ratFrom(t, "577/10000")))
	require.Equal(t, 0, p.StepPrice.Cmp(ratFrom(t, "721/10000000")))
}

func TestScriptParamsFromAlonzoGenesisRationalForm(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "alonzo.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"executionPrices":{"prMem":{"numerator":577,"denominator":10000},"prSteps":{"numerator":721,"denominator":10000000}}}`), 0o600))
	base := DefaultScriptParams(nil)
	p, err := loadAlonzoScriptParams(path, base)
	require.NoError(t, err)
	require.Equal(t, 0, p.MemPrice.Cmp(ratFrom(t, "577/10000")))
	require.Equal(t, base.MaxTxExUnits, p.MaxTxExUnits, "absent fields keep the base value")
	require.Equal(t, base.CollateralPercentage, p.CollateralPercentage)
}

func TestScriptParamsFromAlonzoGenesisRejectsBadFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "alonzo.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"executionPrices":{"prMem":"x"}}`), 0o600))
	_, err := loadAlonzoScriptParams(path, DefaultScriptParams(nil))
	require.Error(t, err)
	_, err = loadAlonzoScriptParams(filepath.Join(dir, "missing.json"), DefaultScriptParams(nil))
	require.Error(t, err)
}

func ratFrom(t *testing.T, s string) *big.Rat {
	t.Helper()
	r, ok := new(big.Rat).SetString(s)
	require.True(t, ok)
	return r
}

// The measuring pass declares the maximum budget only to measure; on a
// network with a large budget its fee would exceed a small locked value,
// although the final transaction declares far less and pays far less.
func TestPlanScriptTxSmallValueUnderLargeBudget(t *testing.T) {
	spec, known, key := escrowInitiationFixture(t)
	datum, err := encodePlutusData(escrowInitiationDatum(keyAddressData(keyHash(key)), escrowAmountMin))
	require.NoError(t, err)
	known[0].Lovelace, known[0].Datum = escrowAmountMin, datum
	spec.ScriptInputs[0].UTxO.Amount = escrowAmountMin
	spec.WalletInputs = nil
	spec.Fee = 0
	spec.ChangeIndex = 0
	spec.Outputs = []ScriptOutput{{Address: enterpriseAddr(key), Lovelace: escrowAmountMin}}
	p := DefaultScriptParams(spec.CostModel)
	p.MaxTxExUnits.Memory = 20_000_000
	v := NewVerifier(p)
	txBytes, _, err := PlanScriptTx(spec, v, p, known, 100)
	require.NoError(t, err)
	_, err = v.Verify(txBytes, known, 100)
	require.NoError(t, err)
}
