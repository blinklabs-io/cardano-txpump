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
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blinklabs-io/gouroboros/ledger/common"
	"github.com/stretchr/testify/require"
)

// 30000 bytes spans two 25600-byte tiers: 25600 at 15 lovelace per byte and
// the remaining 4400 at 15*6/5 = 18.
func TestMinFeeForAddsTieredReferenceScriptFee(t *testing.T) {
	p := DefaultScriptParams(nil)
	base := minFeeFor(1000, common.ExUnits{}, p, 0)
	require.Equal(t, uint64(44*1000+155381), base)
	require.Equal(t, base+25600*15+4400*18, minFeeFor(1000, common.ExUnits{}, p, 30000))
	require.Equal(t, base+3504*15, minFeeFor(1000, common.ExUnits{}, p, 3504))
}

func TestMinFeeForUsesConfiguredReferenceScriptCost(t *testing.T) {
	p := DefaultScriptParams(nil)
	p.MinFeeRefScriptCostPerByte = big.NewRat(1, 2)
	base := minFeeFor(1000, common.ExUnits{}, p, 0)
	require.Equal(t, base+1000, minFeeFor(1000, common.ExUnits{}, p, 2000))
}

func TestKnownUTxOScriptIsResolvableThroughTheStub(t *testing.T) {
	v, err := LoadValidator("escrow.json", "escrow.escrow.spend")
	require.NoError(t, err)
	ref := strings.Repeat("cd", 32)
	ls, err := stubFor([]KnownUTxO{{
		Ref: ref + "#0", Address: enterpriseAddr(testSigningKey(0x42)),
		Lovelace: 20_000_000, ScriptV3: v.Code,
	}}, DefaultScriptParams(nil))
	require.NoError(t, err)
	var found bool
	for _, u := range ls.utxos {
		s := u.Output.ScriptRef()
		require.NotNil(t, s)
		require.Equal(t, v.Hash, s.Hash().Bytes())
		require.Len(t, s.RawScriptBytes(), len(v.Code))
		found = true
	}
	require.True(t, found)
}

func TestConwayParamsCarryTheReferenceScriptCost(t *testing.T) {
	p := DefaultScriptParams(make([]int64, 251))
	p.MinFeeRefScriptCostPerByte = big.NewRat(7, 1)
	require.Equal(t, 0, conwayParams(p).MinFeeRefScriptCostPerByte.Cmp(big.NewRat(7, 1)))
	require.Equal(t, 0, conwayParams(DefaultScriptParams(nil)).MinFeeRefScriptCostPerByte.Cmp(big.NewRat(15, 1)))
}

func TestLoadConwayRefScriptCost(t *testing.T) {
	dir := t.TempDir()
	write := func(body string) string {
		path := filepath.Join(dir, "conway.json")
		require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
		return path
	}
	base := DefaultScriptParams(nil)
	p, err := loadConwayScriptParams(write(`{"minFeeRefScriptCostPerByte":20}`), base)
	require.NoError(t, err)
	require.Equal(t, 0, p.MinFeeRefScriptCostPerByte.Cmp(big.NewRat(20, 1)))

	p, err = loadConwayScriptParams(write(`{"minFeeRefScriptCostPerByte":{"numerator":45,"denominator":2}}`), base)
	require.NoError(t, err)
	require.Equal(t, 0, p.MinFeeRefScriptCostPerByte.Cmp(big.NewRat(45, 2)))

	p, err = loadConwayScriptParams(write(`{}`), base)
	require.NoError(t, err)
	require.Equal(t, 0, p.MinFeeRefScriptCostPerByte.Cmp(base.MinFeeRefScriptCostPerByte), "absent keeps the base value")

	for _, bad := range []string{`{"minFeeRefScriptCostPerByte":"x"}`, `{"minFeeRefScriptCostPerByte":-1}`} {
		_, err = loadConwayScriptParams(write(bad), base)
		require.Error(t, err, bad)
	}
}
