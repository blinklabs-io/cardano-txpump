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
	"encoding/hex"
	"encoding/json"
	"math/big"
	"os"
	"testing"

	"github.com/blinklabs-io/gouroboros/cbor"
	"github.com/blinklabs-io/gouroboros/ledger/common"
	"github.com/blinklabs-io/plutigo/data"
	"github.com/blinklabs-io/plutigo/syn"
	"github.com/stretchr/testify/require"
)

func TestFlatRoundTripKeepsEmbeddedBlueprints(t *testing.T) {
	for file, title := range map[string]string{
		"escrow.json":  "escrow.escrow.spend",
		"vesting.json": "vesting.vesting.spend",
	} {
		v, err := LoadValidator(file, title)
		require.NoError(t, err)
		prog, err := decodeValidatorProgram(v.Code)
		require.NoError(t, err)
		again, err := encodeValidatorProgram(prog)
		require.NoError(t, err)
		require.Equal(t, v.Code, again, file)
	}
}

func TestApplyRejectsNoParameters(t *testing.T) {
	v, err := LoadValidator("escrow.json", "escrow.escrow.spend")
	require.NoError(t, err)
	_, err = v.Apply()
	require.Error(t, err)
}

func TestApplyAppendsDataConstant(t *testing.T) {
	v, err := LoadValidator("escrow.json", "escrow.escrow.spend")
	require.NoError(t, err)
	param := data.NewByteString([]byte{1, 2, 3})
	applied, err := v.Apply(param)
	require.NoError(t, err)
	require.NotEqual(t, v.Hash, applied.Hash)
	require.Equal(t, v.Title, applied.Title)

	prog, err := decodeValidatorProgram(applied.Code)
	require.NoError(t, err)
	app, ok := prog.Term.(*syn.Apply[syn.DeBruijn])
	require.True(t, ok)
	c, ok := app.Argument.(*syn.Constant)
	require.True(t, ok)
	d, ok := c.Con.(*syn.Data)
	require.True(t, ok)
	require.Equal(t, param, d.Inner)

	// The wrapper is a single CBOR byte string around the flat program.
	var flat []byte
	_, err = cbor.Decode(applied.Code, &flat)
	require.NoError(t, err)
}

// applyFixture is the output of `aiken blueprint apply` (v1.1.22) on the
// simple-transfer blueprint, so the cross-check does not need aiken at test
// time.
type applyFixture struct {
	Validator string `json:"validator"`
	Title     string `json:"title"`
	Cases     []struct {
		ParameterCBOR string `json:"parameterCBOR"`
		Hash          string `json:"hash"`
		CompiledCode  string `json:"compiledCode"`
	} `json:"cases"`
}

func loadApplyFixture(t *testing.T) (applyFixture, PlutusV3Validator) {
	t.Helper()
	raw, err := os.ReadFile("../../testdata/apply-simple-transfer.json")
	require.NoError(t, err)
	var f applyFixture
	require.NoError(t, json.Unmarshal(raw, &f))
	require.NotEmpty(t, f.Cases)
	v, err := LoadValidator(f.Validator, f.Title)
	require.NoError(t, err)
	return f, v
}

func TestApplyMatchesAikenBlueprintApply(t *testing.T) {
	f, v := loadApplyFixture(t)
	for _, c := range f.Cases {
		raw, err := hex.DecodeString(c.ParameterCBOR)
		require.NoError(t, err)
		param, err := data.Decode(raw)
		require.NoError(t, err)

		applied, err := v.Apply(param)
		require.NoError(t, err)
		require.Equal(t, c.CompiledCode, hex.EncodeToString(applied.Code), c.ParameterCBOR)
		require.Equal(t, c.Hash, hex.EncodeToString(applied.Hash), c.ParameterCBOR)
		require.Equal(t, common.PlutusV3Script(applied.Code).Hash().Bytes(), applied.Hash)
		require.Equal(t, scriptAddressFromHash(applied.Hash), applied.Address())
	}
}

// A parameter encoded any other way than the Data the validator was compiled
// for must give another script, and so fail the cross-check above.
func TestApplyWithWrongParameterEncodingChangesTheHash(t *testing.T) {
	f, v := loadApplyFixture(t)
	c := f.Cases[0]
	raw, err := hex.DecodeString(c.ParameterCBOR)
	require.NoError(t, err)
	good, err := data.Decode(raw)
	require.NoError(t, err)
	key, ok := good.(*data.ByteString)
	require.True(t, ok)

	flipped := append([]byte(nil), key.Inner...)
	flipped[len(flipped)-1] ^= 1
	for name, bad := range map[string]data.PlutusData{
		"last byte flipped":        data.NewByteString(flipped),
		"truncated":                data.NewByteString(key.Inner[:len(key.Inner)-1]),
		"wrapped in a constructor": data.NewConstr(0, good),
		"wrapped in a list":        data.NewList(good),
		"integer":                  data.NewInteger(big.NewInt(1)),
	} {
		applied, err := v.Apply(bad)
		require.NoError(t, err, name)
		require.NotEqual(t, c.Hash, hex.EncodeToString(applied.Hash), name)
		require.NotEqual(t, c.CompiledCode, hex.EncodeToString(applied.Code), name)
	}

	// Applying twice is not applying once: the order and count of parameters
	// are part of the script.
	once, err := v.Apply(good)
	require.NoError(t, err)
	twice, err := v.Apply(good, good)
	require.NoError(t, err)
	require.NotEqual(t, once.Hash, twice.Hash)
	stepwise, err := once.Apply(good)
	require.NoError(t, err)
	require.Equal(t, twice.Hash, stepwise.Hash)
}

func TestApplyCasesGiveDistinctScripts(t *testing.T) {
	f, _ := loadApplyFixture(t)
	seen := map[string]bool{}
	for _, c := range f.Cases {
		require.False(t, seen[c.Hash])
		seen[c.Hash] = true
	}
}

// The token-transfer validator takes three parameters, applied one at a time
// by aiken. Applying them in a different order, or all at once, must land on
// the same script as aiken's sequence.
func TestApplyMultipleParametersMatchesAikenSequence(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/apply-token-transfer.json")
	require.NoError(t, err)
	var f struct {
		Blueprint string `json:"blueprint"`
		Title     string `json:"title"`
		Steps     []struct {
			ParameterCBOR string `json:"parameterCBOR"`
			Hash          string `json:"hash"`
			CompiledCode  string `json:"compiledCode"`
		} `json:"steps"`
	}
	require.NoError(t, json.Unmarshal(raw, &f))
	require.Len(t, f.Steps, 3)
	bp, err := os.ReadFile("../../testdata/" + f.Blueprint)
	require.NoError(t, err)
	v, err := parseValidator(bp, f.Title)
	require.NoError(t, err)

	params := make([]data.PlutusData, len(f.Steps))
	step := v
	for i, s := range f.Steps {
		cborBytes, err := hex.DecodeString(s.ParameterCBOR)
		require.NoError(t, err)
		params[i], err = data.Decode(cborBytes)
		require.NoError(t, err)

		step, err = step.Apply(params[i])
		require.NoError(t, err)
		require.Equal(t, s.CompiledCode, hex.EncodeToString(step.Code), "step %d", i)
		require.Equal(t, s.Hash, hex.EncodeToString(step.Hash), "step %d", i)
	}

	all, err := v.Apply(params...)
	require.NoError(t, err)
	require.Equal(t, step.Hash, all.Hash)

	reversed, err := v.Apply(params[2], params[1], params[0])
	require.NoError(t, err)
	require.NotEqual(t, step.Hash, reversed.Hash, "parameter order is part of the script")
}
