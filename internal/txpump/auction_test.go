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
	"io"
	"log/slog"
	"math/rand"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/blinklabs-io/gouroboros/ledger/common"
	"github.com/blinklabs-io/gouroboros/ledger/conway"
	"github.com/blinklabs-io/plutigo/data"
	"github.com/stretchr/testify/require"
)

const auctionTestStart = 3 * 500

// auctionHarness drives an auctionApp against a wallet of three users and
// checks every submitted transaction with an independent verifier against
// the outputs seen so far, as a node would.
type auctionHarness struct {
	t      *testing.T
	app    *auctionApp
	wallet *Wallet
	users  []*User
	cfg    *Config
	slot   uint64
	known  []KnownUTxO
	txs    [][]byte
	reject func(txBytes []byte) error
	picks  []int
}

func newAuctionHarness(t *testing.T) *auctionHarness {
	t.Helper()
	app, err := newAuctionApp()
	require.NoError(t, err)
	h := &auctionHarness{t: t, app: app, wallet: NewWallet(), slot: auctionTestStart}
	for i, k := range []*UTxOKey{escrowTestKey(0x01), escrowTestKey(0x02), escrowTestKey(0x03)} {
		for j := 0; j < 4; j++ {
			u := UTxO{
				TxHash:     strings.Repeat(hex.EncodeToString([]byte{byte(0x10*(i+1) + j)}), 32),
				Amount:     50_000_000,
				SigningKey: k,
			}
			h.wallet.Add(u)
			h.known = append(h.known, SyntheticWalletUTxO(u, k.Address))
		}
	}
	h.users = usersFromWallet(h.wallet)
	cm := testCostModel(t)
	params := DefaultScriptParams(cm)
	params.SystemStart = time.Unix(1_700_000_000, 0)
	h.cfg = &Config{
		PlutusV3CostModel: cm, ScriptParams: params, EpochLength: 500,
		SlotLength: time.Second, SystemStartUnix: 1_700_000_000,
	}
	app.choose = func(lo, hi int) int {
		if len(h.picks) == 0 {
			return lo
		}
		v := h.picks[0]
		h.picks = h.picks[1:]
		require.True(t, v >= lo && v <= hi, "scripted pick %d outside [%d,%d]", v, lo, hi)
		return v
	}
	app.now = func() time.Time { return time.Unix(0, 0) }
	return h
}

func (h *auctionHarness) ms(slot uint64) int64 { return slotMillis(h.cfg.ScriptParams, slot) }

func (h *auctionHarness) env(user *User) *AppEnv {
	return &AppEnv{
		User:   user,
		Users:  h.users,
		Wallet: h.wallet,
		Cfg:    h.cfg,
		Slot:   h.slot,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Submit: func(txType, txID string, txBytes []byte) error {
			require.Equal(h.t, "auction", txType)
			if h.reject != nil {
				if err := h.reject(txBytes); err != nil {
					return err
				}
			}
			_, err := NewVerifier(h.cfg.ScriptParams).Verify(txBytes, h.known, h.slot)
			require.NoError(h.t, err, "submitted transaction must verify")
			outs, err := KnownUTxOsFromTx(txBytes)
			require.NoError(h.t, err)
			h.known = append(h.known, outs...)
			h.txs = append(h.txs, txBytes)
			return nil
		},
	}
}

func (h *auctionHarness) step(user *User, picks ...int) *conway.ConwayTransaction {
	h.t.Helper()
	h.picks = picks
	before := len(h.txs)
	ok, err := h.app.Step(h.env(user))
	require.NoError(h.t, err)
	require.True(h.t, ok)
	require.Len(h.t, h.txs, before+1)
	tx, err := conway.NewConwayTransactionFromCbor(h.txs[len(h.txs)-1])
	require.NoError(h.t, err)
	return tx
}

// Scripted picks: the first decides start (0) against advance (1-3). A start
// then draws the opening bid and the duration in slots; advancing draws the
// position among those claimable, and a bid draws its increment.
const (
	aStart   = 0
	aAdvance = 1
)

func (h *auctionHarness) start(seller *User) *conway.ConwayTransaction {
	h.t.Helper()
	return h.step(seller, aStart, auctionStartBidMin, auctionDurationMinSlots)
}

func (h *auctionHarness) bid(bidder *User, step int) *conway.ConwayTransaction {
	h.t.Helper()
	return h.step(bidder, aAdvance, 0, step)
}

func (h *auctionHarness) end(actor *User) *conway.ConwayTransaction {
	h.t.Helper()
	return h.step(actor, aAdvance, 0)
}

func (h *auctionHarness) policy() []byte { return h.app.validator.Hash }

// afterExpiry moves to the first slot whose start is strictly past the
// expiration of the only position.
func (h *auctionHarness) afterExpiry() {
	h.t.Helper()
	require.NotEmpty(h.t, h.app.positions)
	for h.ms(h.slot) <= h.app.positions[0].expiration {
		h.slot++
	}
}

func outputAssets(o interface {
	Assets() *common.MultiAsset[common.MultiAssetTypeOutput]
}) []NativeAsset {
	return nativeAssetsOf(o.Assets())
}

func datumOf(t *testing.T, tx *conway.ConwayTransaction, i int) []byte {
	t.Helper()
	d := tx.Outputs()[i].Datum()
	require.NotNil(t, d)
	return d.Cbor()
}

func (h *auctionHarness) wantDatum(seller, bidder []byte, bid, expiration int64, name []byte) []byte {
	b, err := encodePlutusData(auctionDatum(seller, bidder, bid, expiration, h.policy(), name))
	require.NoError(h.t, err)
	return b
}

func paidWithAsset(t *testing.T, tx *conway.ConwayTransaction, addr []byte) (lovelace uint64, assets []NativeAsset) {
	t.Helper()
	for _, o := range tx.Outputs() {
		if b, _ := o.Address().Bytes(); bytes.Equal(b, addr) {
			lovelace += o.Amount().Uint64()
			assets = append(assets, outputAssets(o)...)
		}
	}
	return lovelace, assets
}

func TestAuctionStartMintsTheItemIntoTheAuctionUTxO(t *testing.T) {
	h := newAuctionHarness(t)
	seller := h.users[0]
	tx := h.start(seller)
	require.Len(t, h.app.positions, 1)
	pos := h.app.positions[0]

	var policy common.Blake2b224
	copy(policy[:], h.policy())
	require.Equal(t, int64(1), tx.AssetMint().Asset(policy, pos.assetName).Int64())

	out := tx.Outputs()[0]
	addr, _ := out.Address().Bytes()
	require.Equal(t, h.app.validator.Address(), addr)
	require.Equal(t, uint64(auctionStartBidMin), out.Amount().Uint64())
	require.Equal(t, []NativeAsset{{Policy: h.policy(), Name: pos.assetName, Quantity: 1}}, outputAssets(out))

	require.Equal(t, h.ms(h.slot)+int64(auctionDurationMinSlots)*1000, pos.expiration)
	require.Equal(t,
		h.wantDatum(keyHash(seller.Key), nil, auctionStartBidMin, pos.expiration, pos.assetName),
		datumOf(t, tx, 0))
	require.Equal(t, keyHash(seller.Key), tx.RequiredSigners()[0].Bytes())

	// The validator wants the validity range to end before the expiration.
	require.NotZero(t, tx.TTL())
	require.Less(t, h.ms(tx.TTL()), pos.expiration)
}

func TestAuctionFirstBidReplacesTheOpeningState(t *testing.T) {
	h := newAuctionHarness(t)
	seller, bidder := h.users[0], h.users[1]
	h.start(seller)
	pos := h.app.positions[0]
	h.slot++
	tx := h.bid(bidder, auctionBidStepMin)

	require.Len(t, h.app.positions, 1)
	next := h.app.positions[0]
	wantBid := int64(auctionStartBidMin + auctionBidStepMin)
	require.Equal(t, wantBid, next.bid)
	require.True(t, sameUser(next.bidder, bidder))
	require.Equal(t, h.wantDatum(keyHash(seller.Key), keyHash(bidder.Key), wantBid, pos.expiration, pos.assetName), datumOf(t, tx, 0))
	require.Equal(t, keyHash(bidder.Key), tx.RequiredSigners()[0].Bytes())
	require.Equal(t, uint64(wantBid), tx.Outputs()[0].Amount().Uint64())
	require.Len(t, outputAssets(tx.Outputs()[0]), 1, "the item stays in the auction")
	require.Less(t, h.ms(tx.TTL()), pos.expiration)
	paid, _ := paidWithAsset(t, tx, seller.Key.Address)
	require.Zero(t, paid, "nobody to refund on a first bid")
}

func TestAuctionOutbidRefundsThePreviousBidderExactly(t *testing.T) {
	h := newAuctionHarness(t)
	seller, first, second := h.users[0], h.users[1], h.users[2]
	h.start(seller)
	h.slot++
	h.bid(first, auctionBidStepMin)
	prev := h.app.positions[0].bid
	h.slot++
	tx := h.bid(second, auctionBidStepMin)

	refund, assets := paidWithAsset(t, tx, first.Key.Address)
	require.Equal(t, uint64(prev), refund, "the validator wants the previous bid back to the lovelace")
	require.Empty(t, assets)
	require.Equal(t, prev+int64(auctionBidStepMin), h.app.positions[0].bid)
	require.True(t, sameUser(h.app.positions[0].bidder, second))
}

func TestAuctionEndPaysSellerAndDeliversItemToWinner(t *testing.T) {
	for name, actorIdx := range map[string]int{"seller": 0, "winner": 1, "stranger": 2} {
		t.Run(name, func(t *testing.T) {
			h := newAuctionHarness(t)
			seller, winner := h.users[0], h.users[1]
			h.start(seller)
			h.slot++
			h.bid(winner, auctionBidStepMin)
			pos := h.app.positions[0]
			h.afterExpiry()
			tx := h.end(h.users[actorIdx])

			require.Empty(t, h.app.positions)
			require.Equal(t, h.slot, tx.ValidityIntervalStart())
			require.Greater(t, h.ms(tx.ValidityIntervalStart()), pos.expiration)
			addr, _ := tx.Outputs()[0].Address().Bytes()
			require.Equal(t, seller.Key.Address, addr, "the seller's payment is the first output at their address")
			require.GreaterOrEqual(t, tx.Outputs()[0].Amount().Uint64(), uint64(pos.bid))
			_, got := paidWithAsset(t, tx, winner.Key.Address)
			require.Equal(t, []NativeAsset{{Policy: h.policy(), Name: pos.assetName, Quantity: 1}}, got)
			_, gotSeller := paidWithAsset(t, tx, seller.Key.Address)
			require.Empty(t, gotSeller)
			for _, o := range tx.Outputs() {
				b, _ := o.Address().Bytes()
				require.NotEqual(t, h.app.validator.Address(), b, "no auction UTxO remains")
			}
		})
	}
}

func TestAuctionEndWithoutBidsReturnsItemToSeller(t *testing.T) {
	h := newAuctionHarness(t)
	seller := h.users[0]
	h.start(seller)
	pos := h.app.positions[0]
	h.afterExpiry()
	tx := h.end(seller)

	require.Empty(t, h.app.positions)
	require.Equal(t, keyHash(seller.Key), tx.RequiredSigners()[0].Bytes())
	lovelace, assets := paidWithAsset(t, tx, seller.Key.Address)
	require.Equal(t, []NativeAsset{{Policy: h.policy(), Name: pos.assetName, Quantity: 1}}, assets)
	require.Greater(t, lovelace, uint64(auctionStartBidMin-1_000_000))
}

func TestAuctionOnlyTheSellerClosesAnAuctionWithoutBids(t *testing.T) {
	h := newAuctionHarness(t)
	h.start(h.users[0])
	h.afterExpiry()
	// The stranger has nothing to close, so the step opens an auction.
	h.step(h.users[1], aAdvance, auctionStartBidMin, auctionDurationMinSlots)
	require.Len(t, h.app.positions, 2)
}

func TestAuctionBiddingClosesBeforeTheValidityWindowCanEndAfterExpiry(t *testing.T) {
	h := newAuctionHarness(t)
	h.start(h.users[0])
	exp := h.app.positions[0].expiration

	// The last slot whose bid window still ends by the expiration.
	last := uint64(0)
	for s := uint64(auctionTestStart); h.ms(s+auctionBidTTLSlots) <= exp; s++ {
		last = s
	}
	h.slot = last
	h.bid(h.users[1], auctionBidStepMin)

	h.slot = last + 1
	h.step(h.users[2], aAdvance, auctionStartBidMin, auctionDurationMinSlots)
	require.Len(t, h.app.positions, 2, "no bid once its window could reach the expiration")
}

func TestAuctionEndWaitsPastTheExpirationInstant(t *testing.T) {
	h := newAuctionHarness(t)
	seller := h.users[0]
	h.start(seller)
	exp := h.app.positions[0].expiration
	for h.ms(h.slot) < exp {
		h.slot++
	}
	require.Equal(t, exp, h.ms(h.slot))
	h.step(seller, aAdvance, auctionStartBidMin, auctionDurationMinSlots)
	require.Len(t, h.app.positions, 2, "a lower bound equal to the expiration fails the validator")
}

func TestAuctionSellerAndLeaderDoNotBid(t *testing.T) {
	h := newAuctionHarness(t)
	seller, bidder := h.users[0], h.users[1]
	h.start(seller)
	h.slot++
	h.picks = []int{aAdvance, 0}
	p, _ := h.app.claim(seller, h.ms(h.slot), h.ms(h.slot+auctionBidTTLSlots))
	require.Nil(t, p, "the seller does not bid on their own auction")
	h.bid(bidder, auctionBidStepMin)
	h.picks = []int{aAdvance, 0}
	p, _ = h.app.claim(bidder, h.ms(h.slot), h.ms(h.slot+auctionBidTTLSlots))
	require.Nil(t, p, "the leader does not outbid themselves")
}

func (h *auctionHarness) pending(tx *conway.ConwayTransaction) pendingTx {
	h.wallet.mu.Lock()
	defer h.wallet.mu.Unlock()
	p, ok := h.wallet.pending[tx.Hash().String()]
	require.True(h.t, ok, "an accepted transaction stays pending until reconciled")
	return p
}

func TestAuctionDoesNotRecordItemOutputsInTheWallet(t *testing.T) {
	h := newAuctionHarness(t)
	seller, winner := h.users[0], h.users[1]
	h.start(seller)
	h.slot++
	h.bid(winner, auctionBidStepMin)
	h.afterExpiry()
	tx := h.end(winner)

	// An output holding the item must not be offered as plain ADA: spending
	// it without returning the item would burn it and fail the transaction.
	recorded := h.pending(tx).outputs
	require.NotEmpty(t, recorded, "the seller's payment and the change are recorded")
	for _, u := range recorded {
		require.Empty(t, outputAssets(tx.Outputs()[u.Index]), "output %d holds the item", u.Index)
	}
}

// planStart plans an opening transaction for seller with datum d and the
// item in its output, after mutate has altered the spec.
func (h *auctionHarness) planStart(seller *User, d data.PlutusData, mutate func(*ScriptTxSpec)) error {
	h.t.Helper()
	params := h.cfg.ScriptParams
	name := []byte("lot")
	inputs, change, err := h.wallet.SelectCoinsForKey(seller.Key, auctionStartBidMin+auctionFeeBudget)
	require.NoError(h.t, err)
	collateral, _, err := h.wallet.SelectCoinsForKey(seller.Key, auctionCollateral)
	require.NoError(h.t, err)
	defer h.wallet.ReturnUTxOs(append(inputs, collateral...))
	item := []NativeAsset{{Policy: h.policy(), Name: name, Quantity: 1}}
	spec := ScriptTxSpec{
		Validator:    h.app.validator,
		CostModel:    params.CostModel,
		Mint:         item,
		MintRedeemer: auctionMintRedeemer(),
		WalletInputs: inputs,
		Collateral:   collateral,
		Outputs: []ScriptOutput{
			{Address: h.app.validator.Address(), Lovelace: auctionStartBidMin, Assets: item, Datum: d},
			{Address: seller.Key.Address, Lovelace: change + auctionFeeBudget},
		},
		ChangeIndex:     1,
		RequiredSigners: [][]byte{keyHash(seller.Key)},
		SigningKeys:     []*UTxOKey{seller.Key},
		TTL:             h.slot + 10,
	}
	if mutate != nil {
		mutate(&spec)
	}
	_, _, err = PlanScriptTx(spec, NewVerifier(params), params, walletKnown(inputs, collateral), h.slot)
	return err
}

func TestAuctionPlannerRejectsWhatTheMintHandlerRejects(t *testing.T) {
	h := newAuctionHarness(t)
	seller, other := h.users[0], h.users[1]
	exp := h.ms(h.slot) + 120_000
	good := func(bidder []byte, expiration int64, policy []byte) data.PlutusData {
		return auctionDatum(keyHash(seller.Key), bidder, auctionStartBidMin, expiration, policy, []byte("lot"))
	}
	require.NoError(t, h.planStart(seller, good(nil, exp, h.policy()), nil), "the unmutated start must plan")
	// The upper bound is exclusive, so it may equal the expiration.
	require.NoError(t, h.planStart(seller, good(nil, h.ms(h.slot+10), h.policy()), nil), "bound at the expiry")

	cases := map[string]struct {
		datum  data.PlutusData
		mutate func(*ScriptTxSpec)
	}{
		"seller did not sign":    {good(nil, exp, h.policy()), func(s *ScriptTxSpec) { s.RequiredSigners = nil }},
		"another key signed":     {good(nil, exp, h.policy()), func(s *ScriptTxSpec) { s.RequiredSigners = [][]byte{keyHash(other.Key)} }},
		"bidder already set":     {good(keyHash(other.Key), exp, h.policy()), nil},
		"no upper bound":         {good(nil, exp, h.policy()), func(s *ScriptTxSpec) { s.TTL = 0 }},
		"bound after the expiry": {good(nil, h.ms(h.slot+5), h.policy()), nil},
		"item minted elsewhere": {good(nil, exp, h.policy()), func(s *ScriptTxSpec) {
			s.Outputs[1].Assets, s.Outputs[0].Assets = s.Outputs[0].Assets, nil
		}},
		"datum names other item": {good(nil, exp, bytes.Repeat([]byte{9}, 28)), nil},
	}
	for name, c := range cases {
		require.Error(t, h.planStart(seller, c.datum, c.mutate), name)
	}
}

// planSpend plans a spend of pos with the given outputs, signers and bounds.
func (h *auctionHarness) planSpend(pos *auctionPosition, actor *User, redeemer data.PlutusData, outputs []ScriptOutput, signers [][]byte, start, ttl uint64, wallet bool) error {
	h.t.Helper()
	params := h.cfg.ScriptParams
	collateral, _, err := h.wallet.SelectCoinsForKey(actor.Key, auctionCollateral)
	require.NoError(h.t, err)
	var inputs []UTxO
	if wallet {
		inputs, _, err = h.wallet.SelectCoinsForKey(actor.Key, 20_000_000)
		require.NoError(h.t, err)
	}
	defer h.wallet.ReturnUTxOs(append(inputs, collateral...))
	var total uint64
	for _, u := range inputs {
		total += u.Amount
	}
	// The last output is the change, which takes whatever the others leave.
	var paid uint64
	for _, o := range outputs[:len(outputs)-1] {
		paid += o.Lovelace
	}
	outputs[len(outputs)-1].Lovelace = total + pos.lovelace - paid
	spec := ScriptTxSpec{
		Validator:       h.app.validator,
		CostModel:       params.CostModel,
		ScriptInputs:    []ScriptInput{{UTxO: pos.utxo, Redeemer: redeemer}},
		WalletInputs:    inputs,
		Collateral:      collateral,
		Outputs:         outputs,
		ChangeIndex:     len(outputs) - 1,
		RequiredSigners: signers,
		SigningKeys:     []*UTxOKey{actor.Key},
		ValidityStart:   start,
		TTL:             ttl,
	}
	known := append(append([]KnownUTxO(nil), pos.known...), walletKnown(inputs, collateral)...)
	_, _, err = PlanScriptTx(spec, NewVerifier(params), params, known, h.slot)
	return err
}

func TestAuctionPlannerRejectsWhatTheBidHandlerRejects(t *testing.T) {
	h := newAuctionHarness(t)
	seller, first, second := h.users[0], h.users[1], h.users[2]
	h.start(seller)
	h.slot++
	h.bid(first, auctionBidStepMin)
	pos := h.app.positions[0]
	item := []NativeAsset{{Policy: h.policy(), Name: pos.assetName, Quantity: 1}}
	sellerKH, expiration := keyHash(seller.Key), pos.expiration

	type bidCase struct {
		bidder   []byte // datum's new highest bidder
		bid      int64
		seller   []byte
		expiry   int64
		name     []byte
		outItem  []NativeAsset
		outCoin  uint64
		refund   uint64
		signers  [][]byte
		ttl      uint64
		noRefund bool
	}
	base := func() bidCase {
		return bidCase{
			bidder: keyHash(second.Key), bid: pos.bid + 1_000_000, seller: sellerKH, expiry: expiration,
			name: pos.assetName, outItem: item, outCoin: uint64(pos.bid) + 1_000_000, refund: uint64(pos.bid),
			signers: [][]byte{keyHash(second.Key)}, ttl: h.slot + 10,
		}
	}
	plan := func(c bidCase) error {
		outs := []ScriptOutput{{
			Address: h.app.validator.Address(), Lovelace: c.outCoin, Assets: c.outItem,
			Datum: auctionDatum(c.seller, c.bidder, c.bid, c.expiry, h.policy(), c.name),
		}}
		if !c.noRefund {
			outs = append(outs, ScriptOutput{Address: first.Key.Address, Lovelace: c.refund})
		}
		outs = append(outs, ScriptOutput{Address: second.Key.Address})
		return h.planSpend(pos, second, auctionBidRedeemer(), outs, c.signers, 0, c.ttl, true)
	}
	require.NoError(t, plan(base()), "the unmutated bid must plan")
	atExpiry := base()
	atExpiry.ttl = h.slot + uint64((expiration-h.ms(h.slot))/1000)
	require.Equal(t, expiration, h.ms(atExpiry.ttl))
	require.NoError(t, plan(atExpiry), "an exclusive bound may equal the expiration")

	mut := map[string]func(*bidCase){
		"bid equal to the highest":   func(c *bidCase) { c.bid = pos.bid },
		"bid below the highest":      func(c *bidCase) { c.bid = pos.bid - 1 },
		"bidder did not sign":        func(c *bidCase) { c.signers = nil },
		"another key signed":         func(c *bidCase) { c.signers = [][]byte{keyHash(first.Key)} },
		"datum bidder is not signer": func(c *bidCase) { c.bidder = keyHash(first.Key) },
		"no upper bound":             func(c *bidCase) { c.ttl = 0 },
		"upper bound after expiry":   func(c *bidCase) { c.ttl = h.slot + uint64((expiration-h.ms(h.slot))/1000) + 5 },
		"seller changed":             func(c *bidCase) { c.seller = keyHash(second.Key) },
		"expiration extended":        func(c *bidCase) { c.expiry++ },
		"item name changed":          func(c *bidCase) { c.name = []byte("other") },
		"item left the auction":      func(c *bidCase) { c.outItem = nil },
		"value drained":              func(c *bidCase) { c.outCoin = uint64(pos.bid) - 1_000_000 },
		"no refund":                  func(c *bidCase) { c.noRefund = true },
		"refund short by one":        func(c *bidCase) { c.refund = uint64(pos.bid) - 1 },
		"refund over by one":         func(c *bidCase) { c.refund = uint64(pos.bid) + 1 },
	}
	for name, m := range mut {
		c := base()
		m(&c)
		require.Error(t, plan(c), name)
	}
}

func TestAuctionPlannerRejectsWhatTheEndHandlerRejects(t *testing.T) {
	h := newAuctionHarness(t)
	seller, winner, other := h.users[0], h.users[1], h.users[2]
	h.start(seller)
	h.slot++
	h.bid(winner, auctionBidStepMin)
	pos := h.app.positions[0]
	item := []NativeAsset{{Policy: h.policy(), Name: pos.assetName, Quantity: 1}}
	h.afterExpiry()
	after := h.slot

	type endCase struct {
		start     uint64
		itemTo    []byte
		sellerPay uint64
		extraItem bool
		continues bool
	}
	base := endCase{start: after, itemTo: winner.Key.Address, sellerPay: uint64(pos.bid)}
	plan := func(c endCase) error {
		outs := []ScriptOutput{
			{Address: seller.Key.Address, Lovelace: c.sellerPay},
			{Address: c.itemTo, Lovelace: auctionItemLovelace, Assets: item},
		}
		if c.extraItem {
			outs = append(outs, ScriptOutput{Address: other.Key.Address, Lovelace: auctionItemLovelace, Assets: item})
		}
		if c.continues {
			outs = append(outs, ScriptOutput{Address: h.app.validator.Address(), Lovelace: 2_000_000, Datum: auctionDatum(nil, nil, 0, 0, nil, nil)})
		}
		outs = append(outs, ScriptOutput{Address: other.Key.Address})
		return h.planSpend(pos, other, auctionEndRedeemer(), outs, nil, c.start, 0, true)
	}
	require.NoError(t, plan(base), "the unmutated end must plan")

	mut := map[string]func(*endCase){
		"lower bound at the expiry": func(c *endCase) { c.start = after - uint64((h.ms(after)-pos.expiration)/1000) },
		"lower bound before":        func(c *endCase) { c.start = after - 10 },
		"no lower bound":            func(c *endCase) { c.start = 0 },
		"item to the wrong user":    func(c *endCase) { c.itemTo = other.Key.Address },
		"seller underpaid":          func(c *endCase) { c.sellerPay = uint64(pos.bid) - 1 },
		"item split across outputs": func(c *endCase) { c.extraItem = true },
		"auction continues":         func(c *endCase) { c.continues = true },
	}
	for name, m := range mut {
		c := base
		m(&c)
		require.Error(t, plan(c), name)
	}
}

func TestAuctionPlannerRejectsWhatTheEndHandlerRejectsWithoutBids(t *testing.T) {
	h := newAuctionHarness(t)
	seller, other := h.users[0], h.users[1]
	h.start(seller)
	pos := h.app.positions[0]
	item := []NativeAsset{{Policy: h.policy(), Name: pos.assetName, Quantity: 1}}
	h.afterExpiry()

	plan := func(itemTo []byte, signers [][]byte, start uint64) error {
		outs := []ScriptOutput{
			{Address: itemTo, Lovelace: auctionItemLovelace, Assets: item},
			{Address: seller.Key.Address},
		}
		return h.planSpend(pos, seller, auctionEndRedeemer(), outs, signers, start, 0, false)
	}
	require.NoError(t, plan(seller.Key.Address, [][]byte{keyHash(seller.Key)}, h.slot))
	require.Error(t, plan(other.Key.Address, [][]byte{keyHash(seller.Key)}, h.slot), "item not returned to the seller")
	require.Error(t, plan(seller.Key.Address, nil, h.slot), "the seller must sign")
	require.Error(t, plan(seller.Key.Address, [][]byte{keyHash(other.Key)}, h.slot), "another key signed")
	require.Error(t, plan(seller.Key.Address, [][]byte{keyHash(seller.Key)}, 0), "no lower bound")
}

func TestAuctionAbandonsPositionOnMissingInputRejection(t *testing.T) {
	h := newAuctionHarness(t)
	seller, bidder := h.users[0], h.users[1]
	h.start(seller)
	pos := h.app.positions[0]
	h.slot++
	balance := h.wallet.Balance()

	h.reject = func([]byte) error { return errors.New("mempool full") }
	h.picks = []int{aAdvance, 0, auctionBidStepMin}
	ok, err := h.app.Step(h.env(bidder))
	require.NoError(t, err)
	require.False(t, ok)
	require.Len(t, h.app.positions, 1, "an unrelated rejection keeps the position")
	require.False(t, h.app.positions[0].busy)
	require.Equal(t, balance, h.wallet.Balance(), "inputs and collateral are returned")

	h.reject = func([]byte) error {
		return errors.New("BadInputsUTxO TxId " + pos.utxo.TxHash + " TxIx 0")
	}
	h.picks = []int{aAdvance, 0, auctionBidStepMin}
	ok, err = h.app.Step(h.env(bidder))
	require.NoError(t, err)
	require.False(t, ok)
	require.Empty(t, h.app.positions)
	require.Equal(t, balance, h.wallet.Balance())
}

func TestAuctionRepeatedRejectionsDropPosition(t *testing.T) {
	h := newAuctionHarness(t)
	h.start(h.users[0])
	h.slot++
	h.reject = func([]byte) error { return errors.New("unrecognised failure") }
	for i := 0; i < auctionMaxRejections; i++ {
		require.Len(t, h.app.positions, 1, "attempt %d", i)
		h.picks = []int{aAdvance, 0, auctionBidStepMin}
		ok, err := h.app.Step(h.env(h.users[1]))
		require.NoError(t, err)
		require.False(t, ok)
	}
	require.Empty(t, h.app.positions)
}

func TestAuctionRejectedStartReturnsTheWalletInputs(t *testing.T) {
	h := newAuctionHarness(t)
	balance := h.wallet.Balance()
	h.reject = func([]byte) error { return errors.New("mempool full") }
	h.picks = []int{aStart, auctionStartBidMin, auctionDurationMinSlots}
	ok, err := h.app.Step(h.env(h.users[0]))
	require.NoError(t, err)
	require.False(t, ok)
	require.Empty(t, h.app.positions)
	require.Equal(t, balance, h.wallet.Balance())
}

func TestAuctionClaimTakesPositionOnce(t *testing.T) {
	h := newAuctionHarness(t)
	h.start(h.users[0])
	h.slot++
	now, cutoff := h.ms(h.slot), h.ms(h.slot+auctionBidTTLSlots)
	h.picks = []int{aAdvance, 0}
	first, action := h.app.claim(h.users[1], now, cutoff)
	require.NotNil(t, first)
	require.Equal(t, auctionBid, action)
	h.picks = []int{aAdvance, 0}
	second, action := h.app.claim(h.users[2], now, cutoff)
	require.Nil(t, second, "a claimed position is not offered again")
	require.Equal(t, auctionStart, action)
}

func TestAuctionKeepsNewPositionUnspendableUntilConfirmed(t *testing.T) {
	h := newAuctionHarness(t)
	h.start(h.users[0])
	h.slot++
	h.app.positions[0].availableAt = h.app.now().Add(time.Hour)
	h.picks = []int{aAdvance, 0}
	p, _ := h.app.claim(h.users[1], h.ms(h.slot), h.ms(h.slot+auctionBidTTLSlots))
	require.Nil(t, p)
}

func TestAuctionSkipsUserWithoutFunds(t *testing.T) {
	h := newAuctionHarness(t)
	h.start(h.users[0])
	h.slot++
	h.wallet = NewWallet()
	h.wallet.Add(UTxO{TxHash: strings.Repeat("dd", 32), Amount: 1_000_000, SigningKey: h.users[1].Key})
	balance := h.wallet.Balance()
	h.picks = []int{aAdvance, 0, auctionBidStepMin}
	ok, err := h.app.Step(h.env(h.users[1]))
	require.NoError(t, err)
	require.False(t, ok)
	require.Equal(t, balance, h.wallet.Balance())
	require.Len(t, h.app.positions, 1)
	require.False(t, h.app.positions[0].busy)
}

func TestAuctionKeepsCollateralReservedUntilReconciled(t *testing.T) {
	h := newAuctionHarness(t)
	h.start(h.users[0])
	h.slot++
	bid := h.bid(h.users[1], auctionBidStepMin)
	collateral := bid.Collateral()
	require.Len(t, collateral, 1)
	ref := collateral[0].Id().String() + ":" + strconv.FormatUint(uint64(collateral[0].Index()), 10)
	var reserved []string
	for _, u := range h.pending(bid).inputs {
		reserved = append(reserved, utxoKey(u))
	}
	require.Contains(t, reserved, ref)
}

func TestAuctionGating(t *testing.T) {
	h := newAuctionHarness(t)
	env := h.env(h.users[0])
	env.Slot = 2*500 + 499
	ok, err := h.app.Step(env)
	require.NoError(t, err)
	require.False(t, ok, "epoch 2 is too early")

	h.cfg.PlutusV3CostModel = nil
	ok, err = h.app.Step(h.env(h.users[0]))
	require.NoError(t, err)
	require.False(t, ok, "no cost model")

	h = newAuctionHarness(t)
	h.cfg.ScriptParams.SystemStart = time.Time{}
	ok, err = h.app.Step(h.env(h.users[0]))
	require.NoError(t, err)
	require.False(t, ok, "without a system start the expiration cannot be set")
	require.Empty(t, h.txs)
}

// The validator compares whole addresses with a key-only address, so a user
// paid at a base address could never be matched.
func TestAuctionSkipsUsersWithStakeAddresses(t *testing.T) {
	h := newAuctionHarness(t)
	k := escrowTestKey(0x09)
	k.Address = append(append([]byte{0x00}, keyHash(k)...), keyHash(k)...)
	// Two coins, so that funds and collateral are both available.
	for _, b := range []string{"ee", "ef"} {
		u := UTxO{TxHash: strings.Repeat(b, 32), Amount: 50_000_000, SigningKey: k}
		h.wallet.Add(u)
		h.known = append(h.known, SyntheticWalletUTxO(u, k.Address))
	}
	h.picks = []int{aStart, auctionStartBidMin, auctionDurationMinSlots}
	ok, err := h.app.Step(h.env(&User{ID: "base", Key: k}))
	require.NoError(t, err)
	require.False(t, ok)
	require.Empty(t, h.txs)
}

func TestAuctionEnabledNeedsTwoUsers(t *testing.T) {
	app, err := newAuctionApp()
	require.NoError(t, err)
	require.False(t, app.Enabled(1))
	require.True(t, app.Enabled(2))
}

func TestAuctionIsRegistered(t *testing.T) {
	_, ok := lookupApp("auction")
	require.True(t, ok)
}

// A long random run exercises every transition with real choices; the
// harness verifies each submitted transaction against the chain it has seen.
func TestAuctionRandomRunVerifiesEveryStep(t *testing.T) {
	h := newAuctionHarness(t)
	for i, u := range h.users {
		for j := 0; j < 80; j++ {
			c := UTxO{
				TxHash:     strings.Repeat(hex.EncodeToString([]byte{byte(0x40 + i), byte(j)}), 16),
				Amount:     50_000_000,
				SigningKey: u.Key,
			}
			h.wallet.Add(c)
			h.known = append(h.known, SyntheticWalletUTxO(c, u.Key.Address))
		}
	}
	rng := rand.New(rand.NewSource(7))
	h.app.choose = func(lo, hi int) int { return lo + rng.Intn(hi-lo+1) }

	counts := map[string]int{}
	for range 150 {
		h.slot += uint64(rng.Intn(25))
		before := len(h.txs)
		ok, err := h.app.Step(h.env(h.users[rng.Intn(len(h.users))]))
		require.NoError(t, err)
		if !ok {
			require.Len(t, h.txs, before)
			continue
		}
		tx, err := conway.NewConwayTransactionFromCbor(h.txs[len(h.txs)-1])
		require.NoError(t, err)
		switch {
		case tx.AssetMint() != nil:
			counts["start"]++
		case tx.ValidityIntervalStart() != 0:
			counts["end"]++
		default:
			counts["bid"]++
		}
	}
	t.Logf("steps: %v", counts)
	require.Positive(t, counts["start"])
	require.Positive(t, counts["bid"])
	require.Positive(t, counts["end"])
}
