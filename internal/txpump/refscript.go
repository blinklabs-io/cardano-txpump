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
	"errors"
	"fmt"

	"github.com/blinklabs-io/gouroboros/cbor"
	"github.com/blinklabs-io/gouroboros/ledger/common"
	"github.com/blinklabs-io/gouroboros/ledger/conway"
)

// RefScript is a published Plutus V3 validator: an output at Address whose
// script_ref carries Script.
type RefScript struct {
	UTxO UTxO // Amount is the output's lovelace; Address is where it sits
	// Hash is the ledger script hash of Script.
	Hash []byte
	// Script is the validator in witness-set form.
	Script []byte
}

// Known describes the reference output to the offline verifier.
func (r RefScript) Known() KnownUTxO {
	k := SyntheticWalletUTxO(r.UTxO, r.UTxO.Address)
	k.ScriptV3 = append([]byte(nil), r.Script...)
	return k
}

// refOutputLovelace is the smallest amount an output at address holding code
// as a reference script may carry: the ledger prices the whole serialized
// output, script included, so the amount and its own encoding are solved
// together.
func refOutputLovelace(address, code []byte, p ScriptParams) (uint64, error) {
	ref, err := encodeScriptRefV3(code)
	if err != nil {
		return 0, err
	}
	amount := uint64(1_000_000)
	for range 4 {
		raw, err := cbor.Encode(plutusOutput{Address: address, Amount: amount, ScriptRef: ref})
		if err != nil {
			return 0, fmt.Errorf("ref_publish: output encoding failed: %w", err)
		}
		need := (constantOutputOverhead + uint64(len(raw))) * p.AdaPerUtxoByte
		if need <= amount {
			return amount, nil
		}
		amount = need
	}
	return 0, errors.New("ref_publish: output minimum did not converge")
}

// constantOutputOverhead is the fixed per-output byte count the ledger adds
// to the serialized size when pricing the minimum output value.
const constantOutputOverhead = 160

// BuildRefPublishTx builds the key-signed transaction that publishes v as a
// reference script at address, the first output, with the rest of inputs
// returned to address as change. It returns the published reference and the
// change paid after the fee.
func BuildRefPublishTx(
	inputs []UTxO,
	v PlutusV3Validator,
	address []byte,
	p ScriptParams,
) ([]byte, string, RefScript, uint64, error) {
	var none RefScript
	if len(v.Code) == 0 || len(v.Hash) != 28 {
		return nil, "", none, 0, errors.New("ref_publish: validator required")
	}
	if !bytes.Equal(common.PlutusV3Script(v.Code).Hash().Bytes(), v.Hash) {
		return nil, "", none, 0, errors.New("ref_publish: validator hash does not match its code")
	}
	if uint64(len(v.Code)) > conway.MaxRefScriptSizePerTx {
		return nil, "", none, 0, fmt.Errorf(
			"ref_publish: script is %d bytes, over the %d-byte per-transaction reference-script limit",
			len(v.Code), conway.MaxRefScriptSizePerTx,
		)
	}
	scriptRef, err := encodeScriptRefV3(v.Code)
	if err != nil {
		return nil, "", none, 0, err
	}
	refLovelace, err := refOutputLovelace(address, v.Code, p)
	if err != nil {
		return nil, "", none, 0, err
	}
	inputSet, total, err := plutusInputs("ref_publish", inputs)
	if err != nil {
		return nil, "", none, 0, err
	}
	keys := make([]*UTxOKey, 0, len(inputs))
	for _, u := range inputs {
		keys = append(keys, u.SigningKey)
	}
	fee := MinFee
	for range 4 {
		if total < refLovelace+fee || total-refLovelace-fee < minSendAmount {
			return nil, "", none, 0, fmt.Errorf(
				"ref_publish: inputs %d cannot cover the reference output %d and fee %d",
				total, refLovelace, fee,
			)
		}
		change := total - refLovelace - fee
		body := txBodyPlutus{
			Inputs: inputSet,
			Outputs: []plutusOutput{
				{Address: address, Amount: refLovelace, ScriptRef: scriptRef},
				{Address: address, Amount: change},
			},
			Fee: fee,
		}
		txBytes, err := encodePlutusTx("ref_publish", body, map[any]any{}, keys)
		if err != nil {
			return nil, "", none, 0, err
		}
		need := minFeeFor(len(txBytes), common.ExUnits{}, p, 0)
		if need <= fee {
			bodyBytes, err := cbor.Encode(body)
			if err != nil {
				return nil, "", none, 0, err
			}
			txID := common.Blake2b256Hash(bodyBytes).String()
			ref := RefScript{
				UTxO:   UTxO{TxHash: txID, Index: 0, Amount: refLovelace, Address: append([]byte(nil), address...)},
				Hash:   append([]byte(nil), v.Hash...),
				Script: append([]byte(nil), v.Code...),
			}
			return txBytes, txID, ref, change, nil
		}
		fee = need * 11 / 10
	}
	return nil, "", none, 0, errors.New("ref_publish: fee did not converge")
}
