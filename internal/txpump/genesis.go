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
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/blinklabs-io/gouroboros/ledger/common"
)

// GenesisUTxO represents a single pre-funded UTxO from the genesis
// configuration, as produced by the testnet-generation-tool.
type GenesisUTxO struct {
	TxHash  string `json:"txHash"`
	Index   uint32 `json:"index"`
	Amount  uint64 `json:"amount"`
	Address string `json:"address,omitempty"`
}

// LoadGenesisUTxOs reads pre-funded UTxOs from a JSON file or directory
// produced by the testnet-generation-tool configurator.
//
// If path is a directory, supported UTxO JSON files are read and unrelated
// JSON files are skipped. If path is a file, it is read directly. Supported
// formats are an array of objects with txHash, index, amount, and optional
// address fields or a Shelley genesis file containing initialFunds.
func LoadGenesisUTxOs(path string) ([]UTxO, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat genesis UTxO path %s: %w", path, err)
	}

	var files []string
	if info.IsDir() {
		entries, dirErr := os.ReadDir(path)
		if dirErr != nil {
			return nil, fmt.Errorf("read genesis UTxO dir %s: %w", path, dirErr)
		}
		for _, e := range entries {
			if !e.IsDir() && filepath.Ext(e.Name()) == ".json" {
				files = append(files, filepath.Join(path, e.Name()))
			}
		}
	} else {
		files = []string{path}
	}

	var utxos []UTxO
	for _, f := range files {
		loaded, supported, loadErr := loadGenesisFile(f)
		if loadErr != nil {
			return nil, fmt.Errorf("load %s: %w", f, loadErr)
		}
		if !supported {
			if !info.IsDir() {
				return nil, fmt.Errorf("load %s: unsupported genesis UTxO JSON format", f)
			}
			continue
		}
		utxos = append(utxos, loaded...)
	}

	if len(utxos) == 0 {
		return nil, fmt.Errorf("no genesis UTxOs found in %s", path)
	}

	return utxos, nil
}

func loadGenesisFile(path string) ([]UTxO, bool, error) {
	data, err := os.ReadFile(path) //nolint:gosec // trusted config path
	if err != nil {
		return nil, false, err
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		if looksLikeGenesisJSON(path, trimmed) {
			return nil, true, fmt.Errorf("empty JSON file")
		}
		return nil, false, nil
	}
	switch trimmed[0] {
	case '[':
		var records []json.RawMessage
		if err := json.Unmarshal(trimmed, &records); err != nil {
			if looksLikeGenesisJSON(path, trimmed) {
				return nil, true, fmt.Errorf("unmarshal UTxO list: %w", err)
			}
			return nil, false, nil
		}
		if len(records) > 0 && !looksLikeUTxOList(records) {
			return nil, false, nil
		}
		var raw []GenesisUTxO
		if err := json.Unmarshal(trimmed, &raw); err != nil {
			return nil, true, fmt.Errorf("unmarshal UTxO list: %w", err)
		}
		utxos := make([]UTxO, len(raw))
		for i, r := range raw {
			hashBytes, decodeErr := hex.DecodeString(r.TxHash)
			if decodeErr != nil || len(hashBytes) != 32 {
				return nil, true, fmt.Errorf(
					"UTxO %d has invalid txHash %q: expected 32-byte hex",
					i, r.TxHash,
				)
			}
			if r.Amount == 0 {
				return nil, true, fmt.Errorf("UTxO %d has zero amount", i)
			}
			var address []byte
			if r.Address != "" {
				address, decodeErr = hex.DecodeString(r.Address)
				if decodeErr != nil || len(address) == 0 {
					return nil, true, fmt.Errorf(
						"UTxO %d has invalid address %q: expected non-empty hex",
						i, r.Address,
					)
				}
			}
			utxos[i] = UTxO{
				TxHash:  r.TxHash,
				Index:   r.Index,
				Amount:  r.Amount,
				Address: address,
			}
		}
		return utxos, true, nil
	case '{':
		var object map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &object); err != nil {
			if looksLikeGenesisJSON(path, trimmed) {
				return nil, true, fmt.Errorf("unmarshal JSON object: %w", err)
			}
			return nil, false, nil
		}
		fundsJSON, ok := object["initialFunds"]
		if !ok {
			return nil, false, nil
		}
		var initialFunds map[string]uint64
		if err := json.Unmarshal(fundsJSON, &initialFunds); err != nil {
			return nil, true, fmt.Errorf("unmarshal Shelley initialFunds: %w", err)
		}
		if len(initialFunds) == 0 {
			return nil, true, nil
		}
		utxos, err := utxosFromShelleyInitialFunds(initialFunds)
		return utxos, true, err
	default:
		if !json.Valid(trimmed) {
			if looksLikeGenesisJSON(path, trimmed) {
				return nil, true, fmt.Errorf("invalid JSON")
			}
			return nil, false, nil
		}
		return nil, false, nil
	}
}

func looksLikeUTxOList(records []json.RawMessage) bool {
	for _, record := range records {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(record, &fields); err != nil {
			continue
		}
		if _, ok := fields["txHash"]; ok {
			return true
		}
		if _, ok := fields["index"]; ok {
			return true
		}
	}
	return false
}

func looksLikeGenesisJSON(path string, data []byte) bool {
	name := strings.ToLower(filepath.Base(path))
	return strings.Contains(name, "genesis") || strings.Contains(name, "utxo") ||
		bytes.Contains(data, []byte(`"initialFunds"`)) ||
		bytes.Contains(data, []byte(`"txHash"`))
}

func utxosFromShelleyInitialFunds(
	initialFunds map[string]uint64,
) ([]UTxO, error) {
	addresses := make([]string, 0, len(initialFunds))
	for address := range initialFunds {
		addresses = append(addresses, address)
	}
	sort.Strings(addresses)

	utxos := make([]UTxO, 0, len(addresses))
	for _, address := range addresses {
		addrBytes, err := hex.DecodeString(address)
		if err != nil {
			return nil, fmt.Errorf(
				"shelley initialFunds address %q: %w",
				address, err,
			)
		}
		txHash := common.Blake2b256Hash(addrBytes)
		utxos = append(utxos, UTxO{
			TxHash: txHash.String(),
			Index:  0,
			Amount: initialFunds[address],
		})
	}
	return utxos, nil
}
