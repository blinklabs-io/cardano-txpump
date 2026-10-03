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

// Package genesis parses network parameters from testnet-generation-tool
// specifications and generated Shelley genesis files.
package genesis

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// testnetParams holds the required testnet parameters (YAML document 1).
type testnetParams struct {
	PoolCount       int    `yaml:"poolCount"`
	NetworkMagic    uint32 `yaml:"networkMagic"`
	SystemStartUnix int64  `yaml:"systemStartUnix"`
}

// byronOverride holds the Byron genesis overrides (YAML document 2).
type byronOverride struct {
	ProtocolConsts struct {
		K uint64 `yaml:"k"`
	} `yaml:"protocolConsts"`
}

// shelleyOverride holds the Shelley genesis overrides (YAML document 3).
type shelleyOverride struct {
	EpochLength      uint64  `yaml:"epochLength"`
	SlotLength       float64 `yaml:"slotLength"`
	ActiveSlotsCoeff float64 `yaml:"activeSlotsCoeff"`
	SecurityParam    uint64  `yaml:"securityParam"`
}

type shelleyGenesis struct {
	NetworkMagic     uint32    `json:"networkMagic"`
	EpochLength      uint64    `json:"epochLength"`
	SlotLength       float64   `json:"slotLength"`
	ActiveSlotsCoeff float64   `json:"activeSlotsCoeff"`
	SecurityParam    uint64    `json:"securityParam"`
	SystemStart      time.Time `json:"systemStart"`
}

// Config holds parsed Cardano network parameters.
type Config struct {
	PoolCount        int
	NetworkMagic     uint32
	EpochLength      uint64
	SlotLength       float64
	ActiveSlotsCoeff float64
	SecurityParam    uint64
	SystemStartUnix  int64
}

// Load reads a generated Shelley genesis JSON file or testnet.yaml from the
// given path and returns the parsed Config.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path) //nolint:gosec // trusted config path
	if err != nil {
		return nil, fmt.Errorf("genesis.Load: read %s: %w", path, err)
	}
	cfg, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("genesis.Load: parse %s: %w", path, err)
	}
	return cfg, nil
}

// countDocs returns the number of YAML documents in data.
func countDocs(data []byte) (int, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	n := 0
	for {
		var v interface{}
		if err := dec.Decode(&v); err != nil {
			if errors.Is(err, io.EOF) {
				return n, nil
			}
			return n, err
		}
		n++
	}
}

// Parse parses the raw bytes of a testnet.yaml into a Config.
func Parse(data []byte) (*Config, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) > 0 && trimmed[0] == '{' {
		var shelley shelleyGenesis
		if err := json.Unmarshal(trimmed, &shelley); err != nil {
			return nil, fmt.Errorf("genesis.Parse: Shelley genesis JSON: %w", err)
		}
		if shelley.SystemStart.IsZero() {
			return nil, errors.New("genesis.Parse: Shelley genesis systemStart is missing")
		}
		cfg := &Config{
			PoolCount:        1,
			NetworkMagic:     shelley.NetworkMagic,
			EpochLength:      shelley.EpochLength,
			SlotLength:       shelley.SlotLength,
			ActiveSlotsCoeff: shelley.ActiveSlotsCoeff,
			SecurityParam:    shelley.SecurityParam,
			SystemStartUnix:  shelley.SystemStart.Unix(),
		}
		if err := validateConfig(cfg); err != nil {
			return nil, err
		}
		return cfg, nil
	}

	// Validate that there are enough YAML documents before decoding.
	// The file must contain at least 3 documents: testnet params, Byron
	// overrides, and Shelley overrides.
	n, countErr := countDocs(data)
	if countErr != nil {
		return nil, fmt.Errorf("genesis.Parse: %w", countErr)
	}
	if n < 3 {
		return nil, fmt.Errorf(
			"genesis.Parse: expected at least 3 YAML documents, got %d",
			n,
		)
	}

	dec := yaml.NewDecoder(bytes.NewReader(data))

	var params testnetParams
	if err := dec.Decode(&params); err != nil {
		return nil, fmt.Errorf("genesis.Parse: testnet params (doc 1): %w", err)
	}

	var byron byronOverride
	if err := dec.Decode(&byron); err != nil {
		return nil, fmt.Errorf(
			"genesis.Parse: byron overrides (doc 2): %w",
			err,
		)
	}

	var shelley shelleyOverride
	if err := dec.Decode(&shelley); err != nil {
		return nil, fmt.Errorf(
			"genesis.Parse: shelley overrides (doc 3): %w",
			err,
		)
	}

	// SecurityParam can come from either Shelley or Byron (k).
	// Prefer the Shelley value; fall back to Byron k.
	secParam := shelley.SecurityParam
	if secParam == 0 && byron.ProtocolConsts.K > 0 {
		secParam = byron.ProtocolConsts.K
	}

	cfg := &Config{
		PoolCount:        params.PoolCount,
		NetworkMagic:     params.NetworkMagic,
		EpochLength:      shelley.EpochLength,
		SlotLength:       shelley.SlotLength,
		ActiveSlotsCoeff: shelley.ActiveSlotsCoeff,
		SecurityParam:    secParam,
		SystemStartUnix:  params.SystemStartUnix,
	}

	if err := validateConfig(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

func validateConfig(cfg *Config) error {
	if cfg.EpochLength == 0 {
		return errors.New("genesis.Parse: epochLength must be > 0")
	}
	if cfg.SlotLength <= 0 {
		return errors.New("genesis.Parse: slotLength must be > 0")
	}
	if cfg.ActiveSlotsCoeff <= 0 {
		return errors.New("genesis.Parse: activeSlotsCoeff must be > 0")
	}
	if cfg.PoolCount <= 0 {
		return errors.New("genesis.Parse: poolCount must be > 0")
	}
	return nil
}
