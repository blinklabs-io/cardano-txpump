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
	"testing"

	"github.com/blinklabs-io/gouroboros/ledger/common"
	"github.com/blinklabs-io/plutigo/data"
	"github.com/stretchr/testify/require"
)

func TestEscrowDataConstructors(t *testing.T) {
	vkh := bytes.Repeat([]byte{0x11}, 28)
	other := bytes.Repeat([]byte{0x22}, 28)

	init, err := encodePlutusData(escrowInitiationDatum(keyAddressData(vkh), 5_000_000))
	require.NoError(t, err)
	// Aiken constructors encode as tagged indefinite lists.
	require.True(t, bytes.HasPrefix(init, []byte{0xd8, 0x79, 0x9f}), "Initiation is Constr 0")
	require.Equal(t, byte(0xff), init[len(init)-1])

	active, err := encodePlutusData(escrowActiveDatum(keyAddressData(vkh), 5_000_000, keyAddressData(other), 3_000_000))
	require.NoError(t, err)
	require.True(t, bytes.HasPrefix(active, []byte{0xd8, 0x7a, 0x9f}), "ActiveEscrow is Constr 1")

	for want, rd := range map[byte]data.PlutusData{
		0x79: escrowDepositRedeemer(keyAddressData(other), 3_000_000),
		0x7a: escrowCancelRedeemer(),
		0x7b: escrowCompleteRedeemer(),
	} {
		b, err := encodePlutusData(rd)
		require.NoError(t, err)
		require.Equal(t, []byte{0xd8, want}, b[:2])
	}

	back, err := data.Decode(init)
	require.NoError(t, err)
	reenc, err := data.Encode(back)
	require.NoError(t, err)
	require.Equal(t, init, reenc)
}

func TestKeyHashIsBlake2b224OfVKey(t *testing.T) {
	k := testSigningKey(0x42)
	require.Equal(t, common.Blake2b224Hash(k.VKey).Bytes(), keyHash(k))
}

// The validator compares whole addresses, stake part included, so the datum
// must describe the exact address the parties are paid at.
func TestPlutusAddressDataMatchesAddressForm(t *testing.T) {
	pay := bytes.Repeat([]byte{0x11}, 28)
	stake := bytes.Repeat([]byte{0x22}, 28)

	ent, err := plutusAddressData(append([]byte{0x60}, pay...))
	require.NoError(t, err)
	require.True(t, ent.Equal(keyAddressData(pay)))

	base, err := plutusAddressData(append(append([]byte{0x00}, pay...), stake...))
	require.NoError(t, err)
	want := data.NewConstr(0,
		data.NewConstr(0, data.NewByteString(pay)),
		data.NewConstr(0, data.NewConstr(0, data.NewConstr(0, data.NewByteString(stake)))),
	)
	require.True(t, base.Equal(want))

	for name, addr := range map[string][]byte{
		"script payment": append([]byte{0x70}, pay...),
		"short":          {0x60, 0x01},
		"empty":          nil,
	} {
		_, err := plutusAddressData(addr)
		require.Error(t, err, name)
	}
}
