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
	"encoding/hex"
	"errors"
	"fmt"
	"sort"

	"github.com/blinklabs-io/gouroboros/cbor"
	"github.com/blinklabs-io/gouroboros/ledger/common"
	"github.com/blinklabs-io/plutigo/data"
)

// ScriptInput is a script-locked UTxO to spend and the redeemer it is spent
// with. The UTxO's Address and SigningKey are ignored.
type ScriptInput struct {
	UTxO     UTxO
	Redeemer data.PlutusData
}

// ScriptOutput is one transaction output. A nil Datum means no datum; any
// other datum is attached inline.
type ScriptOutput struct {
	Address  []byte
	Lovelace uint64
	Datum    data.PlutusData
}

// ScriptTxSpec describes a Conway transaction that spends one Plutus V3
// script input.
type ScriptTxSpec struct {
	Validator    PlutusV3Validator
	ScriptInputs []ScriptInput
	// WalletInputs are key-locked inputs; their keys sign the transaction.
	WalletInputs []UTxO
	// Collateral is key-locked and disjoint from the spent inputs; its keys
	// sign the transaction.
	Collateral      []UTxO
	Outputs         []ScriptOutput
	RequiredSigners [][]byte // 28-byte key hashes
	ValidityStart   uint64   // 0: no lower bound
	TTL             uint64   // 0: no upper bound
	// ExUnits is the execution budget declared for the script input.
	ExUnits   common.ExUnits
	Fee       uint64
	CostModel []int64
	// SigningKeys are extra signers, such as the holders of RequiredSigners.
	SigningKeys []*UTxOKey
}

// scriptTxBody is a Conway transaction body that spends a script.
//
// Key 0  = inputs
// Key 1  = outputs
// Key 2  = fee
// Key 3  = ttl
// Key 8  = validity interval start
// Key 11 = script_data_hash
// Key 13 = collateral inputs
// Key 14 = required signers
type scriptTxBody struct {
	Inputs          cbor.Set       `cbor:"0,keyasint"`
	Outputs         []plutusOutput `cbor:"1,keyasint"`
	Fee             uint64         `cbor:"2,keyasint"`
	TTL             uint64         `cbor:"3,keyasint,omitempty"`
	ValidityStart   uint64         `cbor:"8,keyasint,omitempty"`
	ScriptDataHash  []byte         `cbor:"11,keyasint,omitempty"`
	Collateral      cbor.Set       `cbor:"13,keyasint,omitempty"`
	RequiredSigners cbor.Set       `cbor:"14,keyasint,omitempty"`
}

type conwayScriptTx struct {
	cbor.StructAsArray
	Body    scriptTxBody
	Witness map[any]any
	IsValid bool
	AuxData any
}

type sortedInput struct {
	hash   []byte
	index  uint32
	script bool
}

// BuildScriptTx encodes and signs the transaction spec describes and returns
// it with its transaction ID.
func BuildScriptTx(spec ScriptTxSpec) ([]byte, string, error) {
	if len(spec.ScriptInputs) == 0 {
		return nil, "", errors.New("script_tx: a script input is required")
	}
	// Each template validator checks a single script input and a single
	// continuing output at its address; a second one would either be
	// rejected on chain or, worse, let one redeemer settle two positions.
	if len(spec.ScriptInputs) > 1 {
		return nil, "", errors.New("script_tx: exactly one script input is supported")
	}
	if len(spec.Validator.Code) == 0 || len(spec.Validator.Hash) != 28 {
		return nil, "", errors.New("script_tx: validator required")
	}
	if len(spec.CostModel) == 0 {
		return nil, "", errors.New("script_tx: PlutusV3 cost model required")
	}
	if len(spec.Collateral) == 0 {
		return nil, "", errors.New("script_tx: collateral required")
	}
	for _, c := range spec.Collateral {
		if !c.SigningKey.canSign() {
			return nil, "", errors.New("script_tx: collateral must be key-locked")
		}
	}
	scriptAddr := spec.Validator.Address()
	scriptOutputs := 0
	for _, o := range spec.Outputs {
		if bytes.Equal(o.Address, scriptAddr) {
			scriptOutputs++
		}
	}
	if scriptOutputs > 1 {
		return nil, "", errors.New("script_tx: at most one output at the script address is supported")
	}

	seen := make(map[string]struct{})
	var inputs []sortedInput
	add := func(u UTxO, script bool) error {
		k := utxoKey(u)
		if _, dup := seen[k]; dup {
			return fmt.Errorf("script_tx: input %s used twice", k)
		}
		seen[k] = struct{}{}
		h, err := decodeTxHash(u.TxHash)
		if err != nil {
			return err
		}
		inputs = append(inputs, sortedInput{hash: h, index: u.Index, script: script})
		return nil
	}
	for _, si := range spec.ScriptInputs {
		if si.Redeemer == nil {
			return nil, "", errors.New("script_tx: script input needs a redeemer")
		}
		if err := add(si.UTxO, true); err != nil {
			return nil, "", err
		}
	}
	for _, u := range spec.WalletInputs {
		if err := add(u, false); err != nil {
			return nil, "", err
		}
	}
	collateralSet := make(cbor.Set, 0, len(spec.Collateral))
	for _, c := range spec.Collateral {
		if _, dup := seen[utxoKey(c)]; dup {
			return nil, "", fmt.Errorf("script_tx: collateral %s is also spent", utxoKey(c))
		}
		h, err := decodeTxHash(c.TxHash)
		if err != nil {
			return nil, "", err
		}
		collateralSet = append(collateralSet, txBodyInput{Hash: h, Idx: c.Index})
	}

	// The ledger orders the input set by (transaction ID, index), and a spend
	// redeemer's index is its input's position in that order, not the order
	// the inputs were supplied in.
	sort.Slice(inputs, func(i, j int) bool {
		if c := bytes.Compare(inputs[i].hash, inputs[j].hash); c != 0 {
			return c < 0
		}
		return inputs[i].index < inputs[j].index
	})
	inputSet := make(cbor.Set, 0, len(inputs))
	var redeemerIndex uint64
	for i, in := range inputs {
		inputSet = append(inputSet, txBodyInput{Hash: in.hash, Idx: in.index})
		if in.script {
			redeemerIndex = uint64(i)
		}
	}

	outputs := make([]plutusOutput, 0, len(spec.Outputs))
	for _, o := range spec.Outputs {
		out := plutusOutput{Address: o.Address, Amount: o.Lovelace}
		if o.Datum != nil {
			datum, err := encodePlutusData(o.Datum)
			if err != nil {
				return nil, "", fmt.Errorf("script_tx: datum encoding failed: %w", err)
			}
			out.DatumOption = []any{uint64(1), cbor.Tag{Number: 24, Content: datum}}
		}
		outputs = append(outputs, out)
	}

	redeemers, err := encodeSpendRedeemers(redeemerIndex, spec.ScriptInputs[0].Redeemer, spec.ExUnits)
	if err != nil {
		return nil, "", err
	}
	langViews, err := common.EncodeLangViews(
		map[uint]struct{}{plutusV3: {}},
		map[uint][]int64{plutusV3: spec.CostModel},
	)
	if err != nil {
		return nil, "", fmt.Errorf("script_tx: language views: %w", err)
	}
	// Datums are inline, so the script data hash has no datums term.
	scriptDataHash := common.Blake2b256Hash(append(append([]byte{}, redeemers...), langViews...))

	var signers cbor.Set
	for _, s := range spec.RequiredSigners {
		if len(s) != 28 {
			return nil, "", fmt.Errorf("script_tx: required signer has %d bytes", len(s))
		}
		signers = append(signers, s)
	}

	body := scriptTxBody{
		Inputs:          inputSet,
		Outputs:         outputs,
		Fee:             spec.Fee,
		TTL:             spec.TTL,
		ValidityStart:   spec.ValidityStart,
		ScriptDataHash:  scriptDataHash.Bytes(),
		Collateral:      collateralSet,
		RequiredSigners: signers,
	}
	bodyBytes, err := cbor.Encode(body)
	if err != nil {
		return nil, "", fmt.Errorf("script_tx: body encoding failed: %w", err)
	}
	keys := append([]*UTxOKey(nil), spec.SigningKeys...)
	for _, u := range spec.WalletInputs {
		keys = append(keys, u.SigningKey)
	}
	for _, c := range spec.Collateral {
		keys = append(keys, c.SigningKey)
	}
	witness := BuildWitnessMap(bodyBytes, keys...)
	witness[uint64(5)] = cbor.RawMessage(redeemers)
	witness[uint64(7)] = [][]byte{spec.Validator.Code}
	txBytes, err := cbor.Encode(conwayScriptTx{
		Body:    body,
		Witness: witness,
		IsValid: true,
	})
	if err != nil {
		return nil, "", fmt.Errorf("script_tx: CBOR encoding failed: %w", err)
	}
	return txBytes, common.Blake2b256Hash(bodyBytes).String(), nil
}

// encodeSpendRedeemers returns the Conway map-form redeemers for one spend:
// {[0, index]: [data, [mem, steps]]}.
func encodeSpendRedeemers(index uint64, redeemer data.PlutusData, ex common.ExUnits) ([]byte, error) {
	key, err := cbor.Encode([]uint64{uint64(common.RedeemerTagSpend), index})
	if err != nil {
		return nil, fmt.Errorf("script_tx: redeemer key encoding failed: %w", err)
	}
	rd, err := encodePlutusData(redeemer)
	if err != nil {
		return nil, fmt.Errorf("script_tx: redeemer encoding failed: %w", err)
	}
	value, err := cbor.Encode([]any{cbor.RawMessage(rd), []int64{ex.Memory, ex.Steps}})
	if err != nil {
		return nil, fmt.Errorf("script_tx: redeemer encoding failed: %w", err)
	}
	return append(append([]byte{0xa1}, key...), value...), nil
}

func decodeTxHash(h string) ([]byte, error) {
	b, err := hex.DecodeString(h)
	if err != nil {
		return nil, fmt.Errorf("script_tx: invalid tx hash %q: %w", h, err)
	}
	if len(b) != 32 {
		return nil, fmt.Errorf("script_tx: tx hash %q has %d bytes", h, len(b))
	}
	return b, nil
}
