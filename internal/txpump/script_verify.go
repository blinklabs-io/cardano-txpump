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
	"math/big"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/blinklabs-io/gouroboros/cbor"
	"github.com/blinklabs-io/gouroboros/ledger/babbage"
	"github.com/blinklabs-io/gouroboros/ledger/common"
	"github.com/blinklabs-io/gouroboros/ledger/conway"
	"github.com/blinklabs-io/gouroboros/ledger/shelley"
)

// ScriptParams are the protocol parameters script transactions are built and
// verified against.
type ScriptParams struct {
	MinFeeA, MinFeeB     uint
	MemPrice, StepPrice  *big.Rat
	MaxTxExUnits         common.ExUnits
	MaxTxSize            uint
	MaxValueSize         uint
	MaxCollateralInputs  uint
	CollateralPercentage uint
	AdaPerUtxoByte       uint64
	CostModel            []int64
	// MinFeeRefScriptCostPerByte is the per-byte price of the first
	// reference-script tier; nil means the mainnet value, 15. The tier size
	// (25600 bytes) and the 6/5 growth between tiers are ledger constants.
	MinFeeRefScriptCostPerByte *big.Rat
	SystemStart                time.Time
	SlotLength                 time.Duration
}

// DefaultScriptParams returns mainnet-like Conway parameters, which the
// devnet genesis files use unless they override them.
func DefaultScriptParams(costModel []int64) ScriptParams {
	return ScriptParams{
		MinFeeA:              44,
		MinFeeB:              155381,
		MemPrice:             big.NewRat(577, 10000),
		StepPrice:            big.NewRat(721, 10000000),
		MaxTxExUnits:         common.ExUnits{Memory: 14_000_000, Steps: 10_000_000_000},
		MaxTxSize:            16384,
		MaxValueSize:         5000,
		MaxCollateralInputs:  3,
		CollateralPercentage: 150,
		AdaPerUtxoByte:       4310,
		CostModel:            costModel,
		SlotLength:           time.Second,

		MinFeeRefScriptCostPerByte: big.NewRat(defaultRefScriptCostPerByte, 1),
	}
}

const defaultRefScriptCostPerByte = 15

func (p ScriptParams) refScriptCost() *big.Rat {
	if p.MinFeeRefScriptCostPerByte == nil {
		return big.NewRat(defaultRefScriptCostPerByte, 1)
	}
	return p.MinFeeRefScriptCostPerByte
}

// KnownUTxO is a transaction output the verifier may resolve an input to.
type KnownUTxO struct {
	Ref      string // "<txhash>#<index>"
	Address  []byte
	Lovelace uint64
	Datum    []byte // inline datum CBOR, nil if none
	// ScriptV3 is the Plutus V3 script (in witness-set form) the output
	// carries as a reference script, nil if none.
	ScriptV3 []byte
}

// SyntheticWalletUTxO describes a wallet UTxO as an output at address, for a
// wallet input whose creating transaction the verifier has not seen.
func SyntheticWalletUTxO(u UTxO, address []byte) KnownUTxO {
	return KnownUTxO{
		Ref:      u.TxHash + "#" + strconv.FormatUint(uint64(u.Index), 10),
		Address:  append([]byte(nil), address...),
		Lovelace: u.Amount,
	}
}

// KnownUTxOsFromTx returns the outputs of a transaction this process built.
func KnownUTxOsFromTx(txBytes []byte) ([]KnownUTxO, error) {
	tx, err := conway.NewConwayTransactionFromCbor(txBytes)
	if err != nil {
		return nil, fmt.Errorf("known utxos: %w", err)
	}
	id := tx.Hash().String()
	outs := tx.Outputs()
	known := make([]KnownUTxO, 0, len(outs))
	for i, o := range outs {
		addr, err := o.Address().Bytes()
		if err != nil {
			return nil, fmt.Errorf("known utxos: output %d address: %w", i, err)
		}
		k := KnownUTxO{
			Ref:      id + "#" + strconv.Itoa(i),
			Address:  addr,
			Lovelace: o.Amount().Uint64(),
		}
		if d := o.Datum(); d != nil {
			k.Datum = append([]byte(nil), d.Cbor()...)
		}
		if s := o.ScriptRef(); s != nil {
			if v, ok := common.PlutusScriptVersion(s); !ok || v != 2 {
				return nil, fmt.Errorf("known utxos: output %d carries a reference script other than Plutus V3", i)
			}
			k.ScriptV3 = append([]byte(nil), s.RawScriptBytes()...)
		}
		known = append(known, k)
	}
	return known, nil
}

// stubLedger implements the ledger state the Conway UTxO rules and the Plutus
// evaluator read for a script spend. Any other method panics through the nil
// embedded interface, which the verifier turns into an error.
type stubLedger struct {
	common.LedgerState
	utxos   map[string]common.Utxo
	start   time.Time
	slotLen time.Duration
}

func (s *stubLedger) UtxoById(in common.TransactionInput) (common.Utxo, error) {
	u, ok := s.utxos[in.String()]
	if !ok {
		return common.Utxo{}, fmt.Errorf("utxo %s not found", in.String())
	}
	return u, nil
}

func (s *stubLedger) SlotToTime(slot uint64) (time.Time, error) {
	return s.start.Add(time.Duration(slot) * s.slotLen), nil //nolint:gosec // devnet slots are small
}

func (s *stubLedger) TimeToSlot(t time.Time) (uint64, error) {
	if t.Before(s.start) {
		return 0, errors.New("time before system start")
	}
	return uint64(t.Sub(s.start) / s.slotLen), nil
}

func (s *stubLedger) NetworkId() uint { return 0 }

func (s *stubLedger) CostModels() map[common.PlutusLanguage]common.CostModel { return nil }

// encodeScriptRefV3 returns the script_ref encoding of a Plutus V3 script.
func encodeScriptRefV3(code []byte) (cbor.Tag, error) {
	inner, err := cbor.Encode([]any{uint64(common.ScriptRefTypePlutusV3), code})
	if err != nil {
		return cbor.Tag{}, fmt.Errorf("script ref encoding failed: %w", err)
	}
	return cbor.Tag{Number: 24, Content: inner}, nil
}

// stubFor builds the ledger stub from known outputs.
func stubFor(known []KnownUTxO, p ScriptParams) (*stubLedger, error) {
	slotLen := p.SlotLength
	if slotLen <= 0 {
		slotLen = time.Second
	}
	ls := &stubLedger{
		utxos:   make(map[string]common.Utxo, len(known)),
		start:   p.SystemStart,
		slotLen: slotLen,
	}
	for _, k := range known {
		hash, idxStr, ok := strings.Cut(k.Ref, "#")
		if !ok {
			return nil, fmt.Errorf("known utxo %q: missing #index", k.Ref)
		}
		idx, err := strconv.ParseInt(idxStr, 10, 32)
		if err == nil && idx < 0 {
			err = errors.New("negative index")
		}
		if err != nil {
			return nil, fmt.Errorf("known utxo %q: %w", k.Ref, err)
		}
		out := plutusOutput{Address: k.Address, Amount: k.Lovelace}
		if k.Datum != nil {
			out.DatumOption = []any{uint64(1), cbor.Tag{Number: 24, Content: k.Datum}}
		}
		if k.ScriptV3 != nil {
			ref, err := encodeScriptRefV3(k.ScriptV3)
			if err != nil {
				return nil, fmt.Errorf("known utxo %q: %w", k.Ref, err)
			}
			out.ScriptRef = ref
		}
		// Decoding the wire form yields an output exactly as a node would
		// hold it, including the stored CBOR of the inline datum.
		raw, err := cbor.Encode(out)
		if err != nil {
			return nil, fmt.Errorf("known utxo %q: %w", k.Ref, err)
		}
		var o babbage.BabbageTransactionOutput
		if _, err := cbor.Decode(raw, &o); err != nil {
			return nil, fmt.Errorf("known utxo %q: %w", k.Ref, err)
		}
		id := shelley.NewShelleyTransactionInput(hash, int(idx))
		ls.utxos[id.String()] = common.Utxo{Id: id, Output: &o}
	}
	return ls, nil
}

// conwayParams derives the Conway protocol parameters the rules read.
func conwayParams(p ScriptParams) *conway.ConwayProtocolParameters {
	pp := &conway.ConwayProtocolParameters{}
	pp.ProtocolVersion.Major = 10
	pp.MinFeeA, pp.MinFeeB = p.MinFeeA, p.MinFeeB
	pp.MaxTxSize = p.MaxTxSize
	pp.MaxValueSize = p.MaxValueSize
	pp.MaxCollateralInputs = p.MaxCollateralInputs
	pp.CollateralPercentage = p.CollateralPercentage
	pp.AdaPerUtxoByte = p.AdaPerUtxoByte
	pp.MaxTxExUnits = p.MaxTxExUnits
	pp.ExecutionCosts = common.ExUnitPrice{
		MemPrice:  ratOrZero(p.MemPrice),
		StepPrice: ratOrZero(p.StepPrice),
	}
	pp.MinFeeRefScriptCostPerByte = &cbor.Rat{Rat: new(big.Rat).Set(p.refScriptCost())}
	pp.CostModels = map[uint][]int64{plutusV3: p.CostModel}
	return pp
}

func ratOrZero(r *big.Rat) *cbor.Rat {
	if r == nil {
		return &cbor.Rat{Rat: new(big.Rat)}
	}
	return &cbor.Rat{Rat: new(big.Rat).Set(r)}
}

// Verifier checks script transactions offline with gouroboros's Conway rules
// and Plutus evaluator before they are submitted.
type Verifier struct {
	params ScriptParams
	pp     *conway.ConwayProtocolParameters
}

// NewVerifier returns a verifier for p.
func NewVerifier(p ScriptParams) *Verifier {
	return &Verifier{params: p, pp: conwayParams(p)}
}

// Verify runs every Conway UTxO rule and then the Plutus scripts of txBytes,
// resolving inputs and collateral only through known, and returns the
// execution units each redeemer used.
func (v *Verifier) Verify(txBytes []byte, known []KnownUTxO, slot uint64) (map[common.RedeemerKey]common.ExUnits, error) {
	return v.evaluate(txBytes, known, slot, nil)
}

func (v *Verifier) evaluate(txBytes []byte, known []KnownUTxO, slot uint64, budget *common.ExUnits) (map[common.RedeemerKey]common.ExUnits, error) {
	if len(v.params.CostModel) == 0 {
		return nil, errors.New("verify: PlutusV3 cost model required")
	}
	tx, err := conway.NewConwayTransactionFromCbor(txBytes)
	if err != nil {
		return nil, fmt.Errorf("verify: decode: %w", err)
	}
	ls, err := stubFor(known, v.params)
	if err != nil {
		return nil, fmt.Errorf("verify: %w", err)
	}
	if budget == nil {
		for _, rule := range conway.UtxoValidationRules {
			if err := runRule(rule, tx, slot, ls, v.pp); err != nil {
				return nil, err
			}
		}
	}
	used, err := runEvaluator(tx, ls, v.pp, budget)
	if err != nil {
		return nil, err
	}
	// The evaluator skips a redeemer whose script it cannot find, so an
	// empty result for a transaction with redeemers means nothing ran.
	redeemers := 0
	for range tx.WitnessSet.Redeemers().Iter() {
		redeemers++
	}
	if len(used) != redeemers {
		return nil, fmt.Errorf("verify: %d of %d redeemers evaluated", len(used), redeemers)
	}
	return used, nil
}

// evaluatePlutus is the Plutus evaluator; tests replace it.
var evaluatePlutus = conway.EvaluatePlutusScripts

// runEvaluator runs the Plutus evaluator, turning a panic (such as a stub
// ledger method the evaluator needs but the verifier lacks) into an error so
// that verification fails closed.
func runEvaluator(tx common.Transaction, ls common.LedgerState, pp *conway.ConwayProtocolParameters, budget *common.ExUnits) (used map[common.RedeemerKey]common.ExUnits, err error) {
	defer func() {
		if r := recover(); r != nil {
			used, err = nil, fmt.Errorf("verify: plutus evaluation needs ledger state the verifier lacks: %v", r)
		}
	}()
	used, err = evaluatePlutus(tx, ls, pp, budget)
	if err != nil {
		return nil, fmt.Errorf("verify: plutus: %w", err)
	}
	return used, nil
}

func runRule(rule common.UtxoValidationRuleFunc, tx common.Transaction, slot uint64, ls common.LedgerState, pp common.ProtocolParameters) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("verify: rule %s needs ledger state the verifier lacks: %v", ruleName(rule), r)
		}
	}()
	if err := rule(tx, slot, ls, pp); err != nil {
		return fmt.Errorf("verify: rule %s: %w", ruleName(rule), err)
	}
	return nil
}

func ruleName(f any) string {
	fn := runtime.FuncForPC(reflect.ValueOf(f).Pointer())
	if fn == nil {
		return "unknown"
	}
	n := fn.Name()
	return n[strings.LastIndex(n, ".")+1:]
}
