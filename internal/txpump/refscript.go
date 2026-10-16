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
	"sync"
	"time"

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
			return nil, "", none, 0, &refPublishFundsError{
				Required: refLovelace + fee + minSendAmount,
				Total:    total,
			}
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

type refPublishFundsError struct {
	Required uint64
	Total    uint64
}

func (e *refPublishFundsError) Error() string {
	return fmt.Sprintf(
		"ref_publish: inputs %d cannot cover the reference output and fee; need at least %d lovelace including minimum change",
		e.Total,
		e.Required,
	)
}

// refPublishAbsentRounds is how many consecutive reconciliations may miss a
// submitted publish, in neither the mempool nor a block, before it is
// treated as lost and the validator is published again.
const refPublishAbsentRounds = 2

type pendingRef struct {
	txID   string
	absent int
}

// RefScripts tracks the validators published as reference scripts. The chain
// is the only source of truth: Sync replaces what is known with what the node
// reports, so a restarted process rediscovers earlier publications instead of
// publishing again.
type RefScripts struct {
	mu      sync.Mutex
	found   map[string]RefScript
	pending map[string]*pendingRef
}

// NewRefScripts returns an empty registry.
func NewRefScripts() *RefScripts {
	return &RefScripts{found: map[string]RefScript{}, pending: map[string]*pendingRef{}}
}

// Lookup returns the published reference for the script with hash.
func (r *RefScripts) Lookup(hash []byte) (RefScript, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ref, ok := r.found[string(hash)]
	return ref, ok
}

// Missing returns the validators that are neither on chain nor being
// published.
func (r *RefScripts) Missing(wanted []PlutusV3Validator) []PlutusV3Validator {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []PlutusV3Validator
	for _, v := range wanted {
		if _, ok := r.found[string(v.Hash)]; ok {
			continue
		}
		if _, ok := r.pending[string(v.Hash)]; ok {
			continue
		}
		out = append(out, v)
	}
	return out
}

// Sync replaces the known references with snapshot, the publisher address's
// outputs that carry a reference script. An output counts only if the hash of
// the script it carries is one of wanted; the lowest output reference wins
// when several carry the same script. presence reports which submitted
// transactions the node still holds.
func (r *RefScripts) Sync(snapshot []UTxO, wanted []PlutusV3Validator, presence map[string]bool) {
	want := make(map[string]struct{}, len(wanted))
	for _, v := range wanted {
		want[string(v.Hash)] = struct{}{}
	}
	found := make(map[string]RefScript)
	for _, u := range snapshot {
		if len(u.ScriptRef) == 0 {
			continue
		}
		hash := common.PlutusV3Script(u.ScriptRef).Hash().Bytes()
		if _, ok := want[string(hash)]; !ok {
			continue
		}
		if cur, dup := found[string(hash)]; dup && utxoKey(cur.UTxO) <= utxoKey(u) {
			continue
		}
		stored := u
		stored.ScriptRef = nil
		found[string(hash)] = RefScript{
			UTxO:   stored,
			Hash:   hash,
			Script: append([]byte(nil), u.ScriptRef...),
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.found = found
	for hash, p := range r.pending {
		if _, ok := found[hash]; ok {
			delete(r.pending, hash)
			continue
		}
		if presence[p.txID] {
			p.absent = 0
			continue
		}
		p.absent++
		if p.absent >= refPublishAbsentRounds {
			delete(r.pending, hash)
		}
	}
}

// Publish publishes v from the wallet's reserved key and submits the
// transaction. It returns false, with a nil error, when nothing was
// submitted: the validator is already published or in flight, or the node
// rejected the transaction. A publisher that cannot pay is an error.
func (r *RefScripts) Publish(
	v PlutusV3Validator,
	w *Wallet,
	p ScriptParams,
	slot uint64,
	delay time.Duration,
	submit func(txID string, txBytes []byte) error,
) (bool, error) {
	if len(r.Missing([]PlutusV3Validator{v})) == 0 {
		return false, nil
	}
	key := w.ReservedKey()
	if key == nil {
		return false, ErrKeyReserved
	}
	lovelace, err := refOutputLovelace(key.Address, v.Code, p)
	if err != nil {
		return false, err
	}
	target := lovelace + minSendAmount + MinFee
	for {
		inputs, _, err := w.SelectReservedCoins(target)
		if err != nil {
			return false, fmt.Errorf("publish %s: %w", v.Title, err)
		}
		txBytes, txID, _, change, err := BuildRefPublishTx(inputs, v, key.Address, p)
		if err != nil {
			var fundsErr *refPublishFundsError
			if errors.As(err, &fundsErr) && fundsErr.Required > target {
				w.ReturnUTxOs(inputs)
				target = fundsErr.Required
				continue
			}
			w.ReturnUTxOs(inputs)
			return false, fmt.Errorf("publish %s: %w", v.Title, err)
		}
		if _, err := NewVerifier(p).Verify(txBytes, walletKnown(inputs), slot); err != nil {
			w.ReturnUTxOs(inputs)
			return false, fmt.Errorf("publish %s: %w", v.Title, err)
		}
		if err := submit(txID, txBytes); err != nil {
			w.ReturnUTxOs(inputs)
			return false, nil
		}
		r.mu.Lock()
		r.pending[string(v.Hash)] = &pendingRef{txID: txID}
		r.mu.Unlock()
		w.RecordAccepted(txID, inputs, []UTxO{{
			TxHash: txID, Index: 1, Amount: change, SigningKey: key,
		}}, delay)
		return true, nil
	}
}

// setupReferenceScripts designates the wallet key that owns the published
// reference scripts and holds it out of normal traffic. The last key in
// address order is used, so the choice is stable across runs.
func (p *Pump) setupReferenceScripts() error {
	if !p.cfg.referenceScriptsEnabled() {
		return nil
	}
	keys := p.wallet.Keys()
	if len(keys) < 2 {
		return fmt.Errorf(
			"TXPUMP_REFERENCE_SCRIPTS=%s needs at least two wallet keys, one to own the published scripts and one user; found %d",
			p.cfg.ReferenceScripts, len(keys),
		)
	}
	userCount := len(keys) - 1
	for _, name := range p.cfg.Types {
		app, ok := lookupApp(name)
		if !ok {
			continue
		}
		if _, usesReferenceScripts := app.(refScriptApp); !usesReferenceScripts {
			continue
		}
		if !appEnabled(app, userCount) {
			return fmt.Errorf(
				"TXPUMP_REFERENCE_SCRIPTS=%s needs enough wallet keys for reference workload %q; found %d",
				p.cfg.ReferenceScripts, name, len(keys),
			)
		}
	}
	p.wallet.ReserveKey(keys[len(keys)-1])
	p.refs = NewRefScripts()
	return nil
}

// splitReferenceSnapshot separates the publisher's reference-script outputs
// from the rest of snapshot. They must never reach the wallet, where coin
// selection could spend them; every other output has its script dropped.
func splitReferenceSnapshot(snapshot []UTxO, publisher []byte) (rest, refs []UTxO) {
	for _, u := range snapshot {
		if len(u.ScriptRef) > 0 && bytes.Equal(u.Address, publisher) {
			refs = append(refs, u)
			continue
		}
		u.ScriptRef = nil
		rest = append(rest, u)
	}
	return rest, refs
}

// referenceValidators returns the validators of the enabled apps that spend
// through reference scripts.
func (p *Pump) referenceValidators() []PlutusV3Validator {
	var out []PlutusV3Validator
	seen := map[string]struct{}{}
	for _, t := range p.cfg.Types {
		app, ok := lookupApp(t)
		if !ok || !appEnabled(app, len(p.users)) {
			continue
		}
		ra, ok := app.(refScriptApp)
		if !ok {
			continue
		}
		for _, v := range ra.ReferenceValidators() {
			if _, dup := seen[string(v.Hash)]; dup {
				continue
			}
			seen[string(v.Hash)] = struct{}{}
			out = append(out, v)
		}
	}
	return out
}

// publishReferenceScripts publishes each validator that is neither on chain
// nor in flight. It runs after the snapshot Sync of the same round, so a
// validator already on chain is never published again. Apps wait for the
// Plutus V3 cost model, so nothing is published without one.
func (p *Pump) publishReferenceScripts(client *NodeClient, batchSize int) int {
	params := scriptParamsFor(p.cfg)
	if len(params.CostModel) == 0 {
		return 0
	}
	submitted := 0
	for _, v := range p.refs.Missing(p.referenceValidators()) {
		ok, err := p.refs.Publish(
			v, p.wallet, params, p.currentSlot(), p.cfg.confirmationDelay(),
			func(txID string, txBytes []byte) error {
				return p.submitAppTx(client, batchSize, "ref_publish", txID, txBytes)
			},
		)
		if err != nil {
			p.logger.Error("reference script publish failed", "script", v.Title, "err", err)
			return submitted
		}
		if !ok {
			return submitted
		}
		submitted++
		p.logger.Info("reference script published", "script", v.Title)
	}
	return submitted
}
