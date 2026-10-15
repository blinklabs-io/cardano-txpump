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
	"errors"
	"fmt"

	"github.com/blinklabs-io/gouroboros/cbor"
	"github.com/blinklabs-io/gouroboros/ledger/common"
	"github.com/blinklabs-io/plutigo/data"
	"github.com/blinklabs-io/plutigo/syn"
)

// Apply returns the validator with params applied, in order, to its compiled
// program, the way `aiken blueprint apply` does: each parameter becomes a
// Data constant the program term is applied to. The script hash, and so the
// address, changes with every parameter value.
func (v PlutusV3Validator) Apply(params ...data.PlutusData) (PlutusV3Validator, error) {
	if len(params) == 0 {
		return PlutusV3Validator{}, errors.New("apply: no parameters")
	}
	prog, err := decodeValidatorProgram(v.Code)
	if err != nil {
		return PlutusV3Validator{}, fmt.Errorf("apply %q: %w", v.Title, err)
	}
	for _, p := range params {
		if p == nil {
			return PlutusV3Validator{}, fmt.Errorf("apply %q: nil parameter", v.Title)
		}
		prog.Term = &syn.Apply[syn.DeBruijn]{
			Function: prog.Term,
			Argument: &syn.Constant{Con: &syn.Data{Inner: p}},
		}
	}
	code, err := encodeValidatorProgram(prog)
	if err != nil {
		return PlutusV3Validator{}, fmt.Errorf("apply %q: %w", v.Title, err)
	}
	return PlutusV3Validator{
		Title: v.Title,
		Code:  code,
		Hash:  common.PlutusV3Script(code).Hash().Bytes(),
	}, nil
}

// decodeValidatorProgram unwraps the CBOR byte string around a compiled
// program and decodes the Flat program inside.
func decodeValidatorProgram(code []byte) (*syn.Program[syn.DeBruijn], error) {
	var flat []byte
	if _, err := cbor.Decode(code, &flat); err != nil {
		return nil, fmt.Errorf("unwrap script: %w", err)
	}
	return syn.DecodeDeBruijn(flat)
}

func encodeValidatorProgram(prog *syn.Program[syn.DeBruijn]) ([]byte, error) {
	flat, err := syn.Encode(prog)
	if err != nil {
		return nil, err
	}
	return cbor.Encode(flat)
}
