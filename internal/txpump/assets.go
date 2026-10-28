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

	"github.com/blinklabs-io/gouroboros/cbor"
	"github.com/blinklabs-io/gouroboros/ledger/common"
)

// NativeAsset is a quantity of one native asset.
type NativeAsset struct {
	Policy   []byte // 28-byte policy ID
	Name     []byte
	Quantity uint64
}

// assetMap groups assets into the ledger's policy -> name -> quantity form.
// Repeated assets add up.
func assetMap(assets []NativeAsset) map[common.Blake2b224]map[cbor.ByteString]*big.Int {
	m := make(map[common.Blake2b224]map[cbor.ByteString]*big.Int)
	for _, a := range assets {
		var policy common.Blake2b224
		copy(policy[:], a.Policy)
		names := m[policy]
		if names == nil {
			names = make(map[cbor.ByteString]*big.Int)
			m[policy] = names
		}
		name := cbor.NewByteString(a.Name)
		q := new(big.Int).SetUint64(a.Quantity)
		if prev, ok := names[name]; ok {
			q.Add(q, prev)
		}
		names[name] = q
	}
	return m
}

// plutusOutputWire is plutusOutput as it is encoded: the value is a bare
// coin, or [coin, multiasset] when the output carries native assets.
type plutusOutputWire struct {
	Address     []byte `cbor:"0,keyasint"`
	Value       any    `cbor:"1,keyasint"`
	DatumOption []any  `cbor:"2,keyasint,omitempty"`
	ScriptRef   any    `cbor:"3,keyasint,omitempty"`
}

// MarshalCBOR encodes the output with a lovelace-only value unless it has
// assets, so outputs without assets keep the form every other builder emits.
func (o plutusOutput) MarshalCBOR() ([]byte, error) {
	var value any = o.Amount
	if len(o.Assets) > 0 {
		value = []any{o.Amount, assetMap(o.Assets)}
	}
	return cbor.Encode(plutusOutputWire{Address: o.Address, Value: value, DatumOption: o.DatumOption, ScriptRef: o.ScriptRef})
}
