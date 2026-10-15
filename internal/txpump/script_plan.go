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
	"math"
	"math/big"

	"github.com/blinklabs-io/gouroboros/cbor"
	"github.com/blinklabs-io/gouroboros/ledger/common"
	"github.com/blinklabs-io/gouroboros/ledger/conway"
)

// declaredUnitsMargin is the headroom declared over measured execution units
// (6/5 = 1.2x), so that a script whose cost depends slightly on the final
// transaction still fits its budget.
const (
	declaredUnitsNum = 6
	declaredUnitsDen = 5
)

// Measure evaluates the scripts of txBytes against the maximum transaction
// budget, without the phase-1 rules, and returns the units each used.
func (v *Verifier) Measure(txBytes []byte, known []KnownUTxO, slot uint64) (map[common.RedeemerKey]common.ExUnits, error) {
	budget := v.params.MaxTxExUnits
	return v.evaluate(txBytes, known, slot, &budget)
}

// PlanScriptTx builds spec with the maximum declared units, measures the
// execution units the script really uses, then rebuilds with 1.2x the
// measured units, a fee covering them, and a fresh script data hash and
// signatures. It returns the final transaction only if it fully verifies.
func PlanScriptTx(spec ScriptTxSpec, v *Verifier, p ScriptParams, known []KnownUTxO, slot uint64) ([]byte, string, error) {
	if spec.ChangeIndex >= len(spec.Outputs) || spec.ChangeIndex < -1 {
		return nil, "", fmt.Errorf("plan: change index %d out of range", spec.ChangeIndex)
	}
	spec.Outputs = append([]ScriptOutput(nil), spec.Outputs...)
	var change uint64
	if spec.ChangeIndex >= 0 {
		change = spec.Outputs[spec.ChangeIndex].Lovelace
	}
	setFee := func(fee uint64) error {
		if spec.ChangeIndex < 0 {
			if fee > spec.Fee {
				return fmt.Errorf("plan: fee %d below the minimum %d", spec.Fee, fee)
			}
			return nil
		}
		if change < fee || change-fee < minSendAmount {
			return fmt.Errorf("plan: change %d cannot cover fee %d and a minimum output", change, fee)
		}
		spec.Fee = fee
		spec.Outputs[spec.ChangeIndex].Lovelace = change - fee
		return nil
	}

	// Pass 1 only measures: it declares the maximum budget and, when the
	// planner owns the fee, no fee at all. Measuring skips the phase-1
	// rules, and a fee for the maximum budget can exceed a small change
	// output that easily pays the fee for the measured units.
	spec.ExUnits = p.MaxTxExUnits
	if spec.ChangeIndex >= 0 {
		spec.Fee = 0
	}
	txBytes, _, err := BuildScriptTx(spec)
	if err != nil {
		return nil, "", err
	}
	used, err := v.Measure(txBytes, known, slot)
	if err != nil {
		return nil, "", err
	}
	if len(used) != 1 {
		return nil, "", fmt.Errorf("plan: measured %d redeemers, want 1", len(used))
	}
	for _, u := range used {
		spec.ExUnits = common.ExUnits{
			Memory: min(scaleUp(u.Memory), p.MaxTxExUnits.Memory),
			Steps:  min(scaleUp(u.Steps), p.MaxTxExUnits.Steps),
		}
	}

	// Pass 2: the fee depends on the size, and the size on the fee's
	// encoding, so rebuild until the fee covers the transaction it is in.
	// Every rebuild recomputes the script data hash over the declared units
	// and re-signs the new body.
	var txID string
	fee := minFeeFor(len(txBytes), spec.ExUnits, p, 0) * 11 / 10
	if spec.ChangeIndex < 0 {
		// The caller's fee is final; Verify enforces the minimum.
		fee = spec.Fee
	}
	for range 4 {
		if err := setFee(fee); err != nil {
			return nil, "", err
		}
		if txBytes, txID, err = BuildScriptTx(spec); err != nil {
			return nil, "", err
		}
		need := minFeeFor(len(txBytes), spec.ExUnits, p, 0)
		if spec.ChangeIndex < 0 || need <= fee {
			if _, err := v.Verify(txBytes, known, slot); err != nil {
				return nil, "", err
			}
			return txBytes, txID, nil
		}
		fee = need * 11 / 10
	}
	return nil, "", errors.New("plan: fee did not converge")
}

func scaleUp(n int64) int64 {
	if n > math.MaxInt64/declaredUnitsNum {
		return math.MaxInt64
	}
	return (n*declaredUnitsNum + declaredUnitsDen - 1) / declaredUnitsDen
}

// minFeeFor is the Conway minimum fee for a transaction of txSize bytes that
// declares ex execution units and whose inputs and reference inputs carry
// refScriptSize bytes of reference scripts in total.
func minFeeFor(txSize int, ex common.ExUnits, p ScriptParams, refScriptSize uint64) uint64 {
	fee := new(big.Rat).SetInt64(int64(p.MinFeeA)*int64(txSize) + int64(p.MinFeeB)) //nolint:gosec // protocol-sized values
	if p.MemPrice != nil {
		fee.Add(fee, new(big.Rat).Mul(p.MemPrice, new(big.Rat).SetInt64(ex.Memory)))
	}
	if p.StepPrice != nil {
		fee.Add(fee, new(big.Rat).Mul(p.StepPrice, new(big.Rat).SetInt64(ex.Steps)))
	}
	q, r := new(big.Int).QuoRem(fee.Num(), fee.Denom(), new(big.Int))
	if r.Sign() > 0 {
		q.Add(q, big.NewInt(1))
	}
	return q.Uint64() + refScriptFee(refScriptSize, p)
}

// refScriptFee is the ledger's tiered reference-script fee, floored once
// over all tiers. It cannot fail for the parameters Verify accepts, so an
// unusable price makes the minimum unreachable instead of free.
func refScriptFee(size uint64, p ScriptParams) uint64 {
	fee, err := conway.CalculateRefScriptFee(
		size,
		&cbor.Rat{Rat: p.refScriptCost()},
		conway.RefScriptCostStride,
		&cbor.Rat{Rat: big.NewRat(6, 5)},
	)
	if err != nil {
		return math.MaxUint64 / 2
	}
	return fee
}
