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
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/big"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/blinklabs-io/cardano-txpump/internal/genesis"
)

const maxStartupTimeoutSeconds = int64(^uint64(0)>>1) / int64(time.Second)

// Config holds all runtime configuration for txpump.
// Values are read from environment variables; defaults apply when a variable
// is absent or empty.
type Config struct {
	// NodeAddr is the primary N2C socket or TCP address to connect to.
	// Default: /ipc/node.socket
	NodeAddr string

	// NetworkMagic is the Cardano network magic number.
	// Default: 42 (devnet)
	NetworkMagic uint32

	// TxCountMin / TxCountMax bound the random batch size.
	// Default: 1 / 10
	TxCountMin int
	TxCountMax int

	// CooldownMin / CooldownMax (milliseconds) bound the inter-batch pause.
	// Default: 500 / 2000
	CooldownMin int
	CooldownMax int

	// ConfirmationSlots is the number of slots newly submitted outputs remain
	// unavailable for coin selection. Zero permits immediate chained spending.
	ConfirmationSlots uint64

	// SlotLength is the wall-clock duration of one network slot. It is loaded
	// from the genesis file and used with ConfirmationSlots.
	SlotLength time.Duration

	// Types is the set of transaction types to generate.
	// Recognised values: "payment", "delegation", "governance", "plutus".
	// Default: ["payment","delegation","governance","plutus"]
	Types []string

	// Users restricts the loaded signing keys this instance uses to these
	// zero-based positions in the sorted genesis.<n>.skey file list. Nil keeps
	// every key. Instances given disjoint sets never spend each other's UTxOs.
	Users []UserIndexRange

	// TransactionEra selects the era used for payment transaction encoding.
	// Conway supports all transaction types; Dijkstra currently supports
	// payment transactions only.
	TransactionEra string

	// LogDir is the directory for structured log files.
	// Default: /logs
	LogDir string

	// FallbackAddr is a secondary N2C address tried when the primary fails.
	// Empty string means no fallback.
	FallbackAddr string

	// StartupTimeout is the maximum time allowed to establish a connection and
	// successfully submit the first transaction. A zero value disables the
	// startup deadline; the default is 60 seconds.
	StartupTimeout time.Duration

	// GenesisUTxOFile is a path to a directory or JSON file containing initial
	// UTxO entries. Signing keys named genesis.<n>.skey and their matching
	// .vkey/.addr.info files are loaded from this directory or the file's parent.
	GenesisUTxOFile string

	// GenesisFile is the path to a generated Shelley genesis JSON file or the
	// testnet.yaml specification. When set, network parameters are read from it.
	// YAML specs must include a resolved systemStartUnix; use generated Shelley
	// JSON when systemStart is relative, such as "now".
	GenesisFile string

	// DelegationPoolKeyHash is the optional hex-encoded 28-byte pool key hash
	// (pool ID) that delegation transactions delegate to. Delegation is
	// disabled when it is empty. The stake credential is the funding input's
	// payment key, so txpump can witness it.
	DelegationPoolKeyHash string

	// EpochLength is the number of slots per epoch. Default: 500.
	EpochLength uint64

	// ScriptAppsFromEpoch is the first epoch in which script apps such as
	// escrow and vesting run. Default: 3.
	ScriptAppsFromEpoch uint64

	// SystemStartUnix is the genesis system start time in Unix seconds.
	SystemStartUnix int64

	// PlutusV3CostModel is the protocol's PlutusV3 cost model, read from the
	// Conway genesis named by TXPUMP_CONWAY_GENESIS_FILE. Plutus unlocks
	// commit to it in their script data hash and are skipped without it.
	PlutusV3CostModel []int64

	// AlonzoGenesisFile is the Alonzo genesis named by
	// TXPUMP_ALONZO_GENESIS_FILE. Its execution prices, execution-unit
	// limit and collateral settings override the ScriptParams defaults.
	AlonzoGenesisFile string

	// ScriptParams are the protocol parameters script apps build and verify
	// transactions against.
	ScriptParams ScriptParams

	// ReferenceScripts selects where script apps get their validator:
	// ReferenceScriptsOff carries it in every witness set, On publishes it
	// once and spends through a reference input, Mixed picks per spend.
	ReferenceScripts string
}

// Values of TXPUMP_REFERENCE_SCRIPTS.
const (
	ReferenceScriptsOff   = "off"
	ReferenceScriptsOn    = "on"
	ReferenceScriptsMixed = "mixed"
)

func (c *Config) referenceScriptsEnabled() bool {
	return c != nil && (c.ReferenceScripts == ReferenceScriptsOn || c.ReferenceScripts == ReferenceScriptsMixed)
}

// LoadConfig reads configuration from environment variables and returns a
// fully-populated Config struct.
func LoadConfig() (*Config, error) {
	cfg := &Config{
		NodeAddr:          envString("TXPUMP_NODE_ADDR", "/ipc/node.socket"),
		NetworkMagic:      42,
		TxCountMin:        1,
		TxCountMax:        10,
		CooldownMin:       500,
		CooldownMax:       2000,
		ConfirmationSlots: 30,
		SlotLength:        time.Second,
		Types: []string{
			"payment",
			"delegation",
			"governance",
			"plutus",
		},
		TransactionEra: envString("TXPUMP_TRANSACTION_ERA", "conway"),
		LogDir:         envString("TXPUMP_LOG_DIR", "/logs"),
		FallbackAddr:   envString("TXPUMP_FALLBACK_ADDR", ""),
		StartupTimeout: 60 * time.Second,
		GenesisUTxOFile: envString(
			"TXPUMP_GENESIS_UTXO_FILE", "",
		),
		GenesisFile: envString("TXPUMP_GENESIS_FILE", ""),
		DelegationPoolKeyHash: envString(
			"TXPUMP_DELEGATION_POOL_KEY_HASH", "",
		),
		EpochLength:         500,
		ScriptAppsFromEpoch: 3,
		ReferenceScripts:    envString("TXPUMP_REFERENCE_SCRIPTS", ReferenceScriptsOff),
	}

	if v := os.Getenv("TXPUMP_NETWORK_MAGIC"); v != "" {
		n, err := strconv.ParseUint(v, 10, 32)
		if err != nil {
			return nil, fmt.Errorf(
				"TXPUMP_NETWORK_MAGIC: invalid uint32 %q: %w", v, err,
			)
		}
		cfg.NetworkMagic = uint32(n)
	}

	if v := os.Getenv("TXPUMP_TX_COUNT_MIN"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return nil, fmt.Errorf(
				"TXPUMP_TX_COUNT_MIN: must be a positive integer, got %q", v,
			)
		}
		cfg.TxCountMin = n
	}

	if v := os.Getenv("TXPUMP_TX_COUNT_MAX"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return nil, fmt.Errorf(
				"TXPUMP_TX_COUNT_MAX: must be a positive integer, got %q", v,
			)
		}
		cfg.TxCountMax = n
	}

	if v := os.Getenv("TXPUMP_COOLDOWN_MIN"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return nil, fmt.Errorf(
				"TXPUMP_COOLDOWN_MIN: must be a non-negative integer, got %q",
				v,
			)
		}
		cfg.CooldownMin = n
	}

	if v := os.Getenv("TXPUMP_COOLDOWN_MAX"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return nil, fmt.Errorf(
				"TXPUMP_COOLDOWN_MAX: must be a non-negative integer, got %q",
				v,
			)
		}
		cfg.CooldownMax = n
	}

	if v := os.Getenv("TXPUMP_CONFIRMATION_SLOTS"); v != "" {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			return nil, fmt.Errorf(
				"TXPUMP_CONFIRMATION_SLOTS: must be a non-negative integer, got %q",
				v,
			)
		}
		cfg.ConfirmationSlots = n
	}

	if v := os.Getenv("TXPUMP_STARTUP_TIMEOUT"); v != "" {
		timeout, err := parseStartupTimeout(v)
		if err != nil {
			return nil, err
		}
		cfg.StartupTimeout = timeout
	}

	if v := os.Getenv("TXPUMP_SCRIPT_APPS_FROM_EPOCH"); v != "" {
		epoch, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			return nil, fmt.Errorf(
				"TXPUMP_SCRIPT_APPS_FROM_EPOCH: must be a non-negative integer, got %q",
				v,
			)
		}
		cfg.ScriptAppsFromEpoch = epoch
	}

	if v := os.Getenv("TXPUMP_TYPES"); v != "" {
		cfg.Types = splitComma(v)
	}

	if v := os.Getenv("TXPUMP_USERS"); v != "" {
		users, err := parseUserIndexes(v)
		if err != nil {
			return nil, err
		}
		cfg.Users = users
	}

	// Load genesis config if provided. Genesis values override defaults
	// but explicit env vars take precedence.
	if cfg.GenesisFile != "" {
		gcfg, loadErr := genesis.Load(cfg.GenesisFile)
		if loadErr != nil {
			return nil, fmt.Errorf("TXPUMP_GENESIS_FILE: %w", loadErr)
		}
		if gcfg.EpochLength == 0 {
			return nil, errors.New(
				"TXPUMP_GENESIS_FILE: genesis has epochLength=0, " +
					"which is invalid",
			)
		}
		if gcfg.SystemStartUnix == 0 {
			return nil, errors.New(
				"TXPUMP_GENESIS_FILE: systemStartUnix is unresolved; " +
					"provide it in the testnet YAML or use generated Shelley genesis JSON",
			)
		}
		cfg.EpochLength = gcfg.EpochLength
		cfg.SlotLength = time.Duration(gcfg.SlotLength * float64(time.Second))
		cfg.SystemStartUnix = gcfg.SystemStartUnix
		if os.Getenv("TXPUMP_NETWORK_MAGIC") == "" {
			cfg.NetworkMagic = gcfg.NetworkMagic
		}
	}

	if path := os.Getenv("TXPUMP_CONWAY_GENESIS_FILE"); path != "" {
		costModel, loadErr := loadPlutusV3CostModel(path)
		if loadErr != nil {
			return nil, fmt.Errorf("TXPUMP_CONWAY_GENESIS_FILE: %w", loadErr)
		}
		cfg.PlutusV3CostModel = costModel
	}

	cfg.ScriptParams = DefaultScriptParams(cfg.PlutusV3CostModel)
	cfg.ScriptParams.SlotLength = cfg.SlotLength
	if path := os.Getenv("TXPUMP_CONWAY_GENESIS_FILE"); path != "" {
		params, loadErr := loadConwayScriptParams(path, cfg.ScriptParams)
		if loadErr != nil {
			return nil, fmt.Errorf("TXPUMP_CONWAY_GENESIS_FILE: %w", loadErr)
		}
		cfg.ScriptParams = params
	}
	if cfg.SystemStartUnix != 0 {
		cfg.ScriptParams.SystemStart = time.Unix(cfg.SystemStartUnix, 0)
	}
	cfg.AlonzoGenesisFile = envString("TXPUMP_ALONZO_GENESIS_FILE", "")
	if cfg.AlonzoGenesisFile != "" {
		params, loadErr := loadAlonzoScriptParams(cfg.AlonzoGenesisFile, cfg.ScriptParams)
		if loadErr != nil {
			return nil, fmt.Errorf("TXPUMP_ALONZO_GENESIS_FILE: %w", loadErr)
		}
		cfg.ScriptParams = params
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

// loadAlonzoScriptParams overlays the script parameters present in an Alonzo
// genesis file on base; absent fields keep base's values.
func loadAlonzoScriptParams(path string, base ScriptParams) (ScriptParams, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // trusted config path
	if err != nil {
		return ScriptParams{}, err
	}
	var g struct {
		ExecutionPrices *struct {
			PrMem   json.RawMessage `json:"prMem"`
			PrSteps json.RawMessage `json:"prSteps"`
		} `json:"executionPrices"`
		MaxTxExUnits *struct {
			Mem   *int64 `json:"exUnitsMem"`
			Steps *int64 `json:"exUnitsSteps"`
		} `json:"maxTxExUnits"`
		CollateralPercentage *uint `json:"collateralPercentage"`
		MaxCollateralInputs  *uint `json:"maxCollateralInputs"`
	}
	if err := json.Unmarshal(raw, &g); err != nil {
		return ScriptParams{}, fmt.Errorf("parse %s: %w", path, err)
	}
	p := base
	if g.ExecutionPrices != nil {
		if g.ExecutionPrices.PrMem != nil {
			if p.MemPrice, err = parseGenesisRat(g.ExecutionPrices.PrMem); err != nil {
				return ScriptParams{}, fmt.Errorf("%s: prMem: %w", path, err)
			}
		}
		if g.ExecutionPrices.PrSteps != nil {
			if p.StepPrice, err = parseGenesisRat(g.ExecutionPrices.PrSteps); err != nil {
				return ScriptParams{}, fmt.Errorf("%s: prSteps: %w", path, err)
			}
		}
	}
	if g.MaxTxExUnits != nil {
		if m := g.MaxTxExUnits.Mem; m != nil {
			if *m <= 0 {
				return ScriptParams{}, fmt.Errorf("%s: exUnitsMem must be positive", path)
			}
			p.MaxTxExUnits.Memory = *m
		}
		if s := g.MaxTxExUnits.Steps; s != nil {
			if *s <= 0 {
				return ScriptParams{}, fmt.Errorf("%s: exUnitsSteps must be positive", path)
			}
			p.MaxTxExUnits.Steps = *s
		}
	}
	if g.CollateralPercentage != nil {
		p.CollateralPercentage = *g.CollateralPercentage
	}
	if g.MaxCollateralInputs != nil {
		p.MaxCollateralInputs = *g.MaxCollateralInputs
	}
	return p, nil
}

// loadConwayScriptParams overlays the script parameters present in a Conway
// genesis file on base; absent fields keep base's values.
func loadConwayScriptParams(path string, base ScriptParams) (ScriptParams, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // trusted config path
	if err != nil {
		return ScriptParams{}, err
	}
	var g struct {
		MinFeeRefScriptCostPerByte json.RawMessage `json:"minFeeRefScriptCostPerByte"`
	}
	if err := json.Unmarshal(raw, &g); err != nil {
		return ScriptParams{}, fmt.Errorf("parse %s: %w", path, err)
	}
	p := base
	if g.MinFeeRefScriptCostPerByte != nil {
		cost, err := parseGenesisRat(g.MinFeeRefScriptCostPerByte)
		if err != nil {
			return ScriptParams{}, fmt.Errorf("%s: minFeeRefScriptCostPerByte: %w", path, err)
		}
		if cost.Sign() < 0 {
			return ScriptParams{}, fmt.Errorf("%s: minFeeRefScriptCostPerByte must not be negative", path)
		}
		p.MinFeeRefScriptCostPerByte = cost
	}
	return p, nil
}

// parseGenesisRat reads a genesis price written either as a decimal number
// or as {"numerator": n, "denominator": d}.
func parseGenesisRat(raw json.RawMessage) (*big.Rat, error) {
	var nd struct {
		Numerator   *big.Int `json:"numerator"`
		Denominator *big.Int `json:"denominator"`
	}
	if err := json.Unmarshal(raw, &nd); err == nil && nd.Numerator != nil && nd.Denominator != nil {
		if nd.Denominator.Sign() == 0 {
			return nil, errors.New("zero denominator")
		}
		return new(big.Rat).SetFrac(nd.Numerator, nd.Denominator), nil
	}
	var num json.Number
	if err := json.Unmarshal(raw, &num); err != nil {
		return nil, fmt.Errorf("not a number: %s", raw)
	}
	r, ok := new(big.Rat).SetString(num.String())
	if !ok {
		return nil, fmt.Errorf("not a number: %s", raw)
	}
	return r, nil
}

// loadPlutusV3CostModel reads plutusV3CostModel from a Conway genesis file.
func loadPlutusV3CostModel(path string) ([]int64, error) {
	data, err := os.ReadFile(path) //nolint:gosec // trusted config path
	if err != nil {
		return nil, err
	}
	var conwayGenesis struct {
		PlutusV3CostModel []int64 `json:"plutusV3CostModel"`
	}
	if err := json.Unmarshal(data, &conwayGenesis); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if len(conwayGenesis.PlutusV3CostModel) == 0 {
		return nil, fmt.Errorf("%s has no plutusV3CostModel", path)
	}
	return conwayGenesis.PlutusV3CostModel, nil
}

func (c *Config) confirmationDelay() time.Duration {
	if c.ConfirmationSlots == 0 || c.SlotLength <= 0 {
		return 0
	}
	slotNanos := uint64(c.SlotLength)
	if c.ConfirmationSlots > uint64(math.MaxInt64)/slotNanos {
		return time.Duration(math.MaxInt64)
	}
	// The guard above bounds the product at math.MaxInt64.
	return time.Duration(c.ConfirmationSlots * slotNanos) //nolint:gosec
}

func parseStartupTimeout(value string) (time.Duration, error) {
	seconds, err := strconv.ParseInt(value, 10, 64)
	if err != nil || seconds < 0 || seconds > maxStartupTimeoutSeconds {
		return 0, fmt.Errorf(
			"TXPUMP_STARTUP_TIMEOUT: must be a non-negative integer (seconds) <= %d, got %q",
			maxStartupTimeoutSeconds,
			value,
		)
	}
	return time.Duration(seconds) * time.Second, nil
}

// validate checks that the config values are internally consistent.
func (c *Config) validate() error {
	if c.TxCountMin > c.TxCountMax {
		return fmt.Errorf(
			"TXPUMP_TX_COUNT_MIN (%d) must be <= TXPUMP_TX_COUNT_MAX (%d)",
			c.TxCountMin, c.TxCountMax,
		)
	}
	if c.CooldownMin > c.CooldownMax {
		return fmt.Errorf(
			"TXPUMP_COOLDOWN_MIN (%d) must be <= TXPUMP_COOLDOWN_MAX (%d)",
			c.CooldownMin, c.CooldownMax,
		)
	}
	if len(c.Types) == 0 {
		return errors.New("TXPUMP_TYPES must not be empty")
	}
	if c.TransactionEra != "conway" && c.TransactionEra != "dijkstra" {
		return fmt.Errorf(
			"TXPUMP_TRANSACTION_ERA must be conway or dijkstra, got %q",
			c.TransactionEra,
		)
	}
	for _, t := range c.Types {
		if _, isApp := lookupApp(t); !coreTypes[t] && !isApp {
			valid := append(
				[]string{"payment", "delegation", "governance", "plutus"},
				registeredAppNames()...,
			)
			return fmt.Errorf(
				"unknown transaction type: %q (valid: %s)",
				t, strings.Join(valid, ", "),
			)
		}
	}
	if c.TransactionEra == "dijkstra" {
		for _, txType := range c.Types {
			if txType != "payment" {
				return fmt.Errorf(
					"TXPUMP_TRANSACTION_ERA=dijkstra supports payment only, got %q",
					txType,
				)
			}
		}
	}
	switch c.ReferenceScripts {
	case "", ReferenceScriptsOff, ReferenceScriptsOn, ReferenceScriptsMixed:
	default:
		return fmt.Errorf(
			"TXPUMP_REFERENCE_SCRIPTS must be off, on or mixed, got %q",
			c.ReferenceScripts,
		)
	}
	if c.DelegationPoolKeyHash != "" {
		if _, err := decodeConfiguredHash(
			"TXPUMP_DELEGATION_POOL_KEY_HASH",
			c.DelegationPoolKeyHash,
		); err != nil {
			return err
		}
	}
	return nil
}

func (c *Config) delegationEnabled() bool {
	return c != nil && c.DelegationPoolKeyHash != ""
}

// envString returns the value of the named environment variable, or the
// provided default if the variable is absent or empty.
func envString(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}

// splitComma splits a comma-separated string, trimming whitespace from each
// element, and discards empty elements.
func splitComma(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// credentialHashLen is the length of a Cardano credential hash
// (Blake2b-224), which is what every configured stake and pool key hash is.
const credentialHashLen = 28

func decodeConfiguredHash(
	name string,
	value string,
) ([]byte, error) {
	decoded, err := hex.DecodeString(value)
	if err != nil {
		return nil, fmt.Errorf("%s: invalid hex %q: %w", name, value, err)
	}
	if len(decoded) != credentialHashLen {
		return nil, fmt.Errorf(
			"%s: expected %d bytes, got %d",
			name, credentialHashLen, len(decoded),
		)
	}
	return decoded, nil
}

// UserIndexRange represents an inclusive range of user indexes.
type UserIndexRange struct {
	First int
	Last  int
}

// parseUserIndexes parses a comma-separated list of indexes and inclusive
// ranges such as "0,2-3" without expanding ranges before the key count is known.
func parseUserIndexes(value string) ([]UserIndexRange, error) {
	indexes := make([]UserIndexRange, 0, strings.Count(value, ",")+1)
	for _, part := range strings.Split(value, ",") {
		part = strings.TrimSpace(part)
		lo, hi, isRange := strings.Cut(part, "-")
		first, err := parseUserIndex(value, lo)
		if err != nil {
			return nil, err
		}
		last := first
		if isRange {
			if last, err = parseUserIndex(value, hi); err != nil {
				return nil, err
			}
			if last < first {
				return nil, fmt.Errorf(
					"TXPUMP_USERS: range %q is reversed in %q", part, value,
				)
			}
		}
		indexes = append(indexes, UserIndexRange{First: first, Last: last})
	}
	sort.Slice(indexes, func(i, j int) bool {
		if indexes[i].First == indexes[j].First {
			return indexes[i].Last < indexes[j].Last
		}
		return indexes[i].First < indexes[j].First
	})
	merged := indexes[:0]
	for _, indexRange := range indexes {
		if len(merged) > 0 {
			previous := &merged[len(merged)-1]
			if indexRange.First <= previous.Last ||
				(previous.Last < int(^uint(0)>>1) &&
					indexRange.First == previous.Last+1) {
				if indexRange.Last > previous.Last {
					previous.Last = indexRange.Last
				}
				continue
			}
		}
		merged = append(merged, indexRange)
	}
	return merged, nil
}

func parseUserIndex(value, s string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 0 {
		return 0, fmt.Errorf(
			"TXPUMP_USERS: %q in %q must be a non-negative integer",
			s, value,
		)
	}
	return n, nil
}

// ParseLogLevel maps a TXPUMP_LOG_LEVEL value to a slog level. An empty value
// is info.
func ParseLogLevel(value string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return slog.LevelInfo, fmt.Errorf(
		"TXPUMP_LOG_LEVEL: must be debug, info, warn or error, got %q", value,
	)
}
