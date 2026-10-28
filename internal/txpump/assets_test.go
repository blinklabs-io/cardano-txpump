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
	"strings"
	"testing"

	"github.com/blinklabs-io/gouroboros/cbor"
	"github.com/blinklabs-io/gouroboros/ledger/babbage"
	"github.com/blinklabs-io/gouroboros/ledger/common"
	"github.com/stretchr/testify/require"
)

func testAsset(qty uint64) NativeAsset {
	return NativeAsset{Policy: make([]byte, 28), Name: []byte("lot"), Quantity: qty}
}

func decodeOutput(t *testing.T, o plutusOutput) *babbage.BabbageTransactionOutput {
	t.Helper()
	raw, err := cbor.Encode(o)
	require.NoError(t, err)
	var out babbage.BabbageTransactionOutput
	_, err = cbor.Decode(raw, &out)
	require.NoError(t, err)
	return &out
}

func TestPlutusOutputEncodesNativeAssets(t *testing.T) {
	out := decodeOutput(t, plutusOutput{
		Address: enterpriseAddr(testSigningKey(1)),
		Amount:  2_000_000,
		Assets:  []NativeAsset{testAsset(1)},
	})
	require.Equal(t, uint64(2_000_000), out.Amount().Uint64())
	ma := out.Assets()
	require.NotNil(t, ma)
	var policy common.Blake2b224
	require.Equal(t, big.NewInt(1), ma.Asset(policy, []byte("lot")))
}

func TestPlutusOutputWithoutAssetsKeepsLovelaceOnlyForm(t *testing.T) {
	raw, err := cbor.Encode(plutusOutput{Address: []byte{0x60}, Amount: 7})
	require.NoError(t, err)
	want, err := cbor.Encode(map[uint64]any{0: []byte{0x60}, 1: uint64(7)})
	require.NoError(t, err)
	require.Equal(t, want, raw)
}

func TestKnownUTxOsFromTxCarriesNativeAssets(t *testing.T) {
	spec, _, key := escrowInitiationFixture(t)
	spec.Outputs = []ScriptOutput{
		{Address: enterpriseAddr(key), Lovelace: 5_000_000, Assets: []NativeAsset{testAsset(3)}},
		{Address: enterpriseAddr(key), Lovelace: 18_000_000},
	}
	txBytes, _, err := BuildScriptTx(spec)
	require.NoError(t, err)
	got, err := KnownUTxOsFromTx(txBytes)
	require.NoError(t, err)
	require.Equal(t, []NativeAsset{testAsset(3)}, got[0].Assets)
	require.Empty(t, got[1].Assets)
}

func TestStubLedgerResolvesNativeAssets(t *testing.T) {
	ls, err := stubFor([]KnownUTxO{{
		Ref: strings.Repeat("aa", 32) + "#0", Address: enterpriseAddr(testSigningKey(1)), Lovelace: 2_000_000, Assets: []NativeAsset{testAsset(1)},
	}}, DefaultScriptParams(nil))
	require.NoError(t, err)
	for _, u := range ls.utxos {
		require.NotNil(t, u.Output.Assets())
	}
}
