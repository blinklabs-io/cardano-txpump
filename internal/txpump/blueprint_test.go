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
	"testing"

	"github.com/blinklabs-io/gouroboros/ledger/common"
	"github.com/stretchr/testify/require"
)

func TestLoadValidatorEscrow(t *testing.T) {
	v, err := LoadValidator("escrow.json", "escrow.escrow.spend")
	require.NoError(t, err)
	require.Equal(t, "8add2e6b0b8708570e2aa3421e9a001e12e7f66e544f24c28bb9cf4f", hex.EncodeToString(v.Hash))
	// The hash is the ledger's, computed over the CBOR-wrapped program.
	require.Equal(t, common.PlutusV3Script(v.Code).Hash().Bytes(), v.Hash)
	addr := v.Address()
	require.Len(t, addr, 29)
	require.Equal(t, byte(0x70), addr[0])
	require.Equal(t, v.Hash, addr[1:])
}

func TestLoadValidatorVesting(t *testing.T) {
	v, err := LoadValidator("vesting.json", "vesting.vesting.spend")
	require.NoError(t, err)
	require.Equal(t, "0e1470667b6c1ad3fb65e4cccfbfd8dd1d687be97a4192c05b8521a4", hex.EncodeToString(v.Hash))
	require.Equal(t, common.PlutusV3Script(v.Code).Hash().Bytes(), v.Hash)
}

func TestLoadValidatorAuction(t *testing.T) {
	spend, err := LoadValidator("auction.json", "auction.auction.spend")
	require.NoError(t, err)
	require.Equal(t, "0136e9acacc40eee2deb228a1a3bb5e15381b5d93c2d8845b65d8385", hex.EncodeToString(spend.Hash))
	// The mint handler is the same program, so the auction address and the
	// policy of its NFT share one hash.
	mint, err := LoadValidator("auction.json", "auction.auction.mint")
	require.NoError(t, err)
	require.Equal(t, spend.Hash, mint.Hash)
}

func TestLoadValidatorRejectsUnknownTitleAndFile(t *testing.T) {
	_, err := LoadValidator("escrow.json", "nope")
	require.Error(t, err)
	_, err = LoadValidator("missing.json", "x")
	require.Error(t, err)
}

func TestLoadValidatorRejectsHashMismatch(t *testing.T) {
	_, err := parseValidator([]byte(`{"validators":[{"title":"t","compiledCode":"450101002499","hash":"00"}]}`), "t")
	require.Error(t, err)
}
