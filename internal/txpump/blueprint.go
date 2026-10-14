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
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/blinklabs-io/gouroboros/ledger/common"
)

//go:embed blueprints/*.json
var blueprintFS embed.FS

// PlutusV3Validator is one compiled validator from an embedded CIP-57
// blueprint.
type PlutusV3Validator struct {
	Title string
	// Code is the CBOR-wrapped Flat program, exactly as it is carried in a
	// witness set and hashed by the ledger.
	Code []byte
	// Hash is the 28-byte ledger script hash of Code.
	Hash []byte
}

type blueprintFile struct {
	Validators []struct {
		Title        string `json:"title"`
		CompiledCode string `json:"compiledCode"`
		Hash         string `json:"hash"`
	} `json:"validators"`
}

// LoadValidator returns the validator named title from the embedded
// blueprint file (a name under blueprints/).
func LoadValidator(file, title string) (PlutusV3Validator, error) {
	raw, err := blueprintFS.ReadFile("blueprints/" + file)
	if err != nil {
		return PlutusV3Validator{}, fmt.Errorf("blueprint %q: %w", file, err)
	}
	return parseValidator(raw, title)
}

// parseValidator checks the stated hash against the ledger hash of the code,
// so a blueprint edited or generated for another version cannot be used
// silently.
func parseValidator(raw []byte, title string) (PlutusV3Validator, error) {
	var bp blueprintFile
	if err := json.Unmarshal(raw, &bp); err != nil {
		return PlutusV3Validator{}, err
	}
	for _, v := range bp.Validators {
		if v.Title != title {
			continue
		}
		code, err := hex.DecodeString(v.CompiledCode)
		if err != nil {
			return PlutusV3Validator{}, fmt.Errorf("validator %q: %w", title, err)
		}
		want, err := hex.DecodeString(v.Hash)
		if err != nil {
			return PlutusV3Validator{}, fmt.Errorf("validator %q: %w", title, err)
		}
		got := common.PlutusV3Script(code).Hash().Bytes()
		if !bytes.Equal(got, want) {
			return PlutusV3Validator{}, fmt.Errorf(
				"validator %q: blueprint hash %x does not match code hash %x",
				title, want, got,
			)
		}
		return PlutusV3Validator{Title: title, Code: code, Hash: got}, nil
	}
	return PlutusV3Validator{}, fmt.Errorf("validator %q not found", title)
}

// Address returns the validator's testnet enterprise script address.
func (v PlutusV3Validator) Address() []byte { return scriptAddressFromHash(v.Hash) }
