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
	"fmt"
	"math/big"

	"github.com/blinklabs-io/gouroboros/ledger/common"
	"github.com/blinklabs-io/plutigo/data"
)

// keyHash returns the payment key hash (Blake2b-224 of the verification key).
func keyHash(k *UTxOKey) []byte { return common.Blake2b224Hash(k.VKey).Bytes() }

// keyAddressData is the Plutus Address of an enterprise key address:
// Address{VerificationKey vkh, None}.
func keyAddressData(vkh []byte) data.PlutusData {
	return data.NewConstr(0,
		data.NewConstr(0, data.NewByteString(vkh)),
		data.NewConstr(1),
	)
}

// plutusAddressData converts a key-payment Shelley address (enterprise, or
// base with a key stake credential) to its Plutus Address form. Validators
// compare whole addresses, so the datum must describe exactly the address the
// parties are paid at, stake part included.
func plutusAddressData(addr []byte) (data.PlutusData, error) {
	if len(addr) == 0 {
		return nil, fmt.Errorf("plutus address: empty address")
	}
	switch addr[0] >> 4 {
	case 0x6:
		if len(addr) != 29 {
			return nil, fmt.Errorf("plutus address: enterprise address has %d bytes", len(addr))
		}
		return keyAddressData(addr[1:29]), nil
	case 0x0:
		if len(addr) != 57 {
			return nil, fmt.Errorf("plutus address: base address has %d bytes", len(addr))
		}
		return data.NewConstr(0,
			data.NewConstr(0, data.NewByteString(addr[1:29])),
			// Some(Inline(VerificationKey stake))
			data.NewConstr(0, data.NewConstr(0, data.NewConstr(0, data.NewByteString(addr[29:57])))),
		), nil
	default:
		return nil, fmt.Errorf("plutus address: unsupported address header %#x", addr[0])
	}
}

// lovelaceValueData is an ADA-only Plutus value: {"": {"": n}}.
func lovelaceValueData(n uint64) data.PlutusData {
	return data.NewMap([][2]data.PlutusData{{
		data.NewByteString([]byte{}),
		data.NewMap([][2]data.PlutusData{{
			data.NewByteString([]byte{}), data.NewInteger(new(big.Int).SetUint64(n)),
		}}),
	}})
}

// Escrow datum and redeemer constructor indices follow the declaration order
// in the template's EscrowDatum and EscrowRedeemer types; a wrong index makes
// the validator fail to decode its datum or redeemer.

func escrowInitiationDatum(initiator data.PlutusData, lovelace uint64) data.PlutusData {
	return data.NewConstr(0, initiator, lovelaceValueData(lovelace))
}

func escrowActiveDatum(initiator data.PlutusData, initLovelace uint64, recipient data.PlutusData, recLovelace uint64) data.PlutusData {
	return data.NewConstr(1,
		initiator, lovelaceValueData(initLovelace),
		recipient, lovelaceValueData(recLovelace))
}

func escrowDepositRedeemer(recipient data.PlutusData, lovelace uint64) data.PlutusData {
	return data.NewConstr(0, recipient, lovelaceValueData(lovelace))
}

func escrowCancelRedeemer() data.PlutusData { return data.NewConstr(1) }

func escrowCompleteRedeemer() data.PlutusData { return data.NewConstr(2) }

func encodePlutusData(pd data.PlutusData) ([]byte, error) { return data.Encode(pd) }
