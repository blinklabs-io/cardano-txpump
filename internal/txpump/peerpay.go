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

import "errors"

func init() { RegisterApp("peer_payment", peerPayment{}) }

// peerPayment moves ADA between distinct users, so the ledger sees
// transactions whose outputs belong to a different key than their inputs.
type peerPayment struct{}

func (peerPayment) Step(env *AppEnv) (bool, error) {
	var peers []*User
	for _, u := range env.Users {
		if u != env.User {
			peers = append(peers, u)
		}
	}
	if len(peers) == 0 {
		return false, nil
	}
	peer := peers[IntRange(0, len(peers)-1)]

	fee := jitteredFee(MinFee)
	//nolint:gosec // bounded by small constants
	sendAmount := uint64(IntRange(int(minSendAmount), int(minSendAmount)*4))
	inputs, change, err := env.Wallet.SelectCoinsForKey(
		env.User.Key, sendAmount+fee,
	)
	if errors.Is(err, ErrInsufficientFunds) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	// Change below the minimum output would make the node reject the whole
	// transaction, so fold it into the payment.
	if change > 0 && change < minSendAmount {
		sendAmount += change
		change = 0
	}

	txBytes, txID, err := BuildPayment(PaymentParams{
		Inputs:      inputs,
		ToAddr:      peer.Key.Address,
		ChangeAddr:  env.User.Key.Address,
		SendAmount:  sendAmount,
		Change:      change,
		Fee:         fee,
		WitnessKeys: []*UTxOKey{env.User.Key},
	})
	if err != nil {
		env.Wallet.ReturnUTxOs(inputs)
		return false, err
	}
	if err := env.Submit("peer_payment", txID, txBytes); err != nil {
		env.Wallet.ReturnUTxOs(inputs)
		env.Logger.Warn("tx rejected", "tx_id", txID, "err", err)
		return false, nil
	}

	outputs := []UTxO{{
		TxHash: txID, Index: 0, Amount: sendAmount, SigningKey: peer.Key,
	}}
	if change > 0 {
		outputs = append(outputs, UTxO{
			TxHash: txID, Index: 1, Amount: change, SigningKey: env.User.Key,
		})
	}
	env.Wallet.RecordAccepted(txID, inputs, outputs, env.Cfg.confirmationDelay())
	return true, nil
}
