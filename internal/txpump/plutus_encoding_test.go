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
	"testing"

	"github.com/blinklabs-io/gouroboros/ledger/common"
	"github.com/blinklabs-io/gouroboros/ledger/conway"
	"github.com/blinklabs-io/plutigo/cek"
	"github.com/blinklabs-io/plutigo/data"
	"github.com/stretchr/testify/require"
)

// The script in a witness set is the CBOR-wrapped Flat program, so that is
// what a node decodes, hashes and runs. This builds the real lock and unlock
// pair and checks the script exactly as a ledger sees it, with no wrapping
// added by the test.
func TestPlutusUnlockWitnessScriptIsLedgerDecodable(t *testing.T) {
	key := testSigningKey(0x42)
	changeAddr := append([]byte{0x60}, common.Blake2b224Hash(key.VKey).Bytes()...)
	funding := []UTxO{{
		TxHash: sampleHash, Index: 0, Amount: 20_000_000, SigningKey: key,
	}}
	lockBytes, err := BuildPlutusLockTx(
		funding, alwaysSucceedsScriptHash(), plutusLockAmount, 200_000, changeAddr,
	)
	require.NoError(t, err)
	lockTx, err := conway.NewConwayTransactionFromCbor(lockBytes)
	require.NoError(t, err)
	lockedAddr, err := lockTx.Outputs()[0].Address().Bytes()
	require.NoError(t, err)
	require.Equal(t, scriptAddressFromHash(alwaysSucceedsScriptHash()), lockedAddr)

	locked := UTxO{TxHash: lockTx.Hash().String(), Index: 0, Amount: plutusLockAmount}
	collateral := UTxO{
		TxHash: lockTx.Hash().String(), Index: 1,
		Amount:     20_000_000 - plutusLockAmount - 200_000,
		SigningKey: key,
	}
	unlockBytes, err := BuildPlutusUnlockTx(
		locked, collateral, make([]int64, 251), plutusUnlockFee, changeAddr,
	)
	require.NoError(t, err)
	unlockTx, err := conway.NewConwayTransactionFromCbor(unlockBytes)
	require.NoError(t, err)

	scripts := unlockTx.WitnessSet.PlutusV3Scripts()
	require.Len(t, scripts, 1)
	require.Equal(t, lockedAddr[1:], scripts[0].Hash().Bytes(),
		"witness script must hash to the credential the output is locked to")
	pp := &conway.ConwayProtocolParameters{}
	pp.ProtocolVersion.Major = 10
	require.NoError(t, conway.UtxoValidateMalformedReferenceScripts(
		unlockTx, 0, nil, pp,
	))

	evalContext := cek.NewDefaultEvalContext(
		cek.LanguageVersionV3, cek.ProtoVersion{Major: 10},
	)
	_, err = scripts[0].Evaluate(
		data.NewInteger(big.NewInt(0)), plutusUnlockExUnits, evalContext,
	)
	require.NoError(t, err)
}
