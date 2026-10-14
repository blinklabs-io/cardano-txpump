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
	"strings"
	"testing"

	"github.com/blinklabs-io/gouroboros/ledger/common"
	"github.com/blinklabs-io/gouroboros/ledger/conway"
	"github.com/stretchr/testify/require"
)

func scriptTestSpec(t *testing.T, scriptFirst bool) ScriptTxSpec {
	t.Helper()
	v, err := LoadValidator("escrow.json", "escrow.escrow.spend")
	require.NoError(t, err)
	k := testSigningKey(0x42)
	hi := strings.Repeat("ff", 32)
	lo := strings.Repeat("00", 32)
	scriptHash, walletHash := lo, hi
	if !scriptFirst {
		scriptHash, walletHash = hi, lo
	}
	return ScriptTxSpec{
		Validator:       v,
		ScriptInputs:    []ScriptInput{{UTxO: UTxO{TxHash: scriptHash, Index: 0, Amount: 5_000_000}, Redeemer: escrowCancelRedeemer()}},
		WalletInputs:    []UTxO{{TxHash: walletHash, Index: 0, Amount: 20_000_000, SigningKey: k}},
		Collateral:      []UTxO{{TxHash: strings.Repeat("aa", 32), Index: 1, Amount: 10_000_000, SigningKey: k}},
		Outputs:         []ScriptOutput{{Address: append([]byte{0x60}, keyHash(k)...), Lovelace: 24_000_000}},
		RequiredSigners: [][]byte{keyHash(k)},
		ExUnits:         common.ExUnits{Memory: 500_000, Steps: 200_000_000},
		Fee:             1_000_000,
		CostModel:       make([]int64, 251),
		SigningKeys:     []*UTxOKey{k},
	}
}

func TestBuildScriptTxRedeemerIndexFollowsSortedInputs(t *testing.T) {
	for _, scriptFirst := range []bool{true, false} {
		txBytes, _, err := BuildScriptTx(scriptTestSpec(t, scriptFirst))
		require.NoError(t, err)
		tx, err := conway.NewConwayTransactionFromCbor(txBytes)
		require.NoError(t, err)
		var idx uint32
		var found int
		for k := range tx.WitnessSet.Redeemers().Iter() {
			idx, found = k.Index, found+1
		}
		require.Equal(t, 1, found)
		want := uint32(0)
		if !scriptFirst {
			want = 1
		}
		require.Equal(t, want, idx, "scriptFirst=%v", scriptFirst)
	}
}

func TestBuildScriptTxWitnessScriptHashesToValidator(t *testing.T) {
	spec := scriptTestSpec(t, true)
	txBytes, txID, err := BuildScriptTx(spec)
	require.NoError(t, err)
	tx, err := conway.NewConwayTransactionFromCbor(txBytes)
	require.NoError(t, err)
	require.Equal(t, tx.Hash().String(), txID)
	scripts := tx.WitnessSet.PlutusV3Scripts()
	require.Len(t, scripts, 1)
	require.Equal(t, spec.Validator.Hash, scripts[0].Hash().Bytes())
	pp := &conway.ConwayProtocolParameters{}
	pp.ProtocolVersion.Major = 10
	require.NoError(t, conway.UtxoValidateMalformedReferenceScripts(tx, 0, nil, pp))
}

func TestBuildScriptTxRejectsBadSpecs(t *testing.T) {
	base := scriptTestSpec(t, true)
	cases := map[string]func(*ScriptTxSpec){
		"no script input": func(s *ScriptTxSpec) { s.ScriptInputs = nil },
		"two script inputs": func(s *ScriptTxSpec) {
			s.ScriptInputs = append(s.ScriptInputs, ScriptInput{
				UTxO: UTxO{TxHash: strings.Repeat("11", 32), Amount: 1}, Redeemer: escrowCancelRedeemer(),
			})
		},
		"no collateral": func(s *ScriptTxSpec) { s.Collateral = nil },
		"no cost model": func(s *ScriptTxSpec) { s.CostModel = nil },
		"duplicate input": func(s *ScriptTxSpec) {
			s.WalletInputs = append(s.WalletInputs, UTxO{TxHash: s.ScriptInputs[0].UTxO.TxHash, Index: 0, Amount: 1, SigningKey: s.SigningKeys[0]})
		},
		"collateral also an input": func(s *ScriptTxSpec) {
			s.Collateral = []UTxO{s.WalletInputs[0]}
		},
		"collateral not key-locked": func(s *ScriptTxSpec) {
			s.Collateral = []UTxO{{TxHash: strings.Repeat("aa", 32), Index: 1, Amount: 10_000_000}}
		},
		"nil redeemer": func(s *ScriptTxSpec) { s.ScriptInputs[0].Redeemer = nil },
		"two script outputs": func(s *ScriptTxSpec) {
			s.Outputs = []ScriptOutput{
				{Address: s.Validator.Address(), Lovelace: 2_000_000, Datum: escrowCancelRedeemer()},
				{Address: s.Validator.Address(), Lovelace: 2_000_000, Datum: escrowCancelRedeemer()},
				{Address: append([]byte{0x60}, keyHash(s.SigningKeys[0])...), Lovelace: 20_000_000},
			}
		},
	}
	_, _, err := BuildScriptTx(base)
	require.NoError(t, err, "the unmutated spec must build")
	for name, mutate := range cases {
		spec := base
		spec.ScriptInputs = append([]ScriptInput(nil), base.ScriptInputs...)
		spec.WalletInputs = append([]UTxO(nil), base.WalletInputs...)
		spec.Outputs = append([]ScriptOutput(nil), base.Outputs...)
		mutate(&spec)
		_, _, err := BuildScriptTx(spec)
		require.Error(t, err, name)
	}
}
