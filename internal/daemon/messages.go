package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"fiatjaf.com/nostr"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/blakeswap/blakeswap/internal/contract"
	"github.com/blakeswap/blakeswap/internal/protocol"
	"github.com/blakeswap/blakeswap/internal/transport"
	"time"
)

type fundingMessage struct {
	TermsHash string `json:"terms_hash"`
	Raw       string `json:"raw"`
}

func bindFunding(c contract.HTLC, raw string) (contract.HTLC, error) {
	tx, err := contract.Parse(raw)
	if err != nil {
		return c, err
	}
	if len(tx.TxOut) < 1 || tx.Version != 2 {
		return c, errors.New("invalid funding transaction")
	}
	pk, err := c.PkScript()
	if err != nil {
		return c, err
	}
	if tx.TxOut[0].Value != c.Amount || !bytes.Equal(tx.TxOut[0].PkScript, pk) {
		return c, errors.New("funding changes the agreed contract")
	}
	c.TxID = tx.TxHash().String()
	c.Vout = 0
	return c, nil
}
func (e *Engine) handle(from string, m transport.Message) error {
	if m.Type == "tower-query" {
		if err := e.advertiseTower(); err != nil {
			return err
		}
		return e.queue(from, "tower-quote", "", e.s.Towers[e.identity.Public().Hex()])
	}
	if m.Type == "tower-quote" {
		var event nostr.Event
		if err := json.Unmarshal(m.Body, &event); err != nil {
			return err
		}
		if event.PubKey.Hex() != from {
			return errors.New("watchtower quote sender mismatch")
		}
		if _, err := protocol.DecodeTower(event, e.Config.Network, time.Now().Unix()); err != nil {
			return err
		}
		e.ingestTower(event)
		return nil
	}

	if m.Type == "tower-job" {
		var job protocol.Job
		if err := json.Unmarshal(m.Body, &job); err != nil {
			return err
		}
		if job.Network.Normalized() != e.Config.Network {
			return errors.New("tower job network mismatch")
		}
		if job.Owner != from || m.SwapID != job.SwapID {
			return errors.New("job sender mismatch")
		}
		bps := e.ownTower().BPS
		if existing := e.s.TowerJobs[job.ID]; existing != nil {
			bps = existing.Job.BPS // An identical retry must still receive its durable receipt.
		}
		if err := job.Validate(e.ownTower().Scripts, bps); err != nil {
			return err
		}
		if len(e.s.TowerJobs) >= 1000 && e.s.TowerJobs[job.ID] == nil {
			return errors.New("tower job capacity")
		}
		if existing := e.s.TowerJobs[job.ID]; existing != nil {
			if protocol.Digest(existing.Job) != protocol.Digest(job) {
				return errors.New("job ID collision")
			}
		} else {
			if !e.registrationWindow(job.Target.Chain, job.Target.RefundHeight) {
				return errors.New("tower registration outside funding window")
			}
			e.s.TowerJobs[job.ID] = &TowerJob{Job: job}
			if err := e.save(); err != nil {
				return err
			}
		}
		return e.queue(from, "tower-receipt", m.SwapID, protocol.Receipt{JobID: job.ID, Digest: protocol.Digest(job)})
	}
	if e.Config.Mode != "trader" {
		return errors.New("tower does not trade")
	}
	if m.Type == "request" {
		var request protocol.Request
		if err := json.Unmarshal(m.Body, &request); err != nil {
			return err
		}
		o, err := request.Validate(time.Now().Unix())
		if err != nil {
			return err
		}
		if o.Network.Normalized() != e.Config.Network {
			return errors.New("request network mismatch")
		}
		if from != request.Taker || request.ID != m.SwapID || o.Maker != e.identity.Public().Hex() {
			return errors.New("request party mismatch")
		}
		if existing := e.s.Swaps[request.ID]; existing != nil {
			if existing.Role != "maker" || protocol.Digest(existing.Request) != protocol.Digest(request) {
				return errors.New("swap ID collision")
			}
			return e.queue(from, "accepted", m.SwapID, existing.Terms)
		}
		owned, ok := e.s.Offers[o.ID]
		var current protocol.Offer
		currentErr := json.Unmarshal([]byte(owned.Content), &current)
		// Maker-authoritative check-and-reserve under Engine.mu, analogous to
		// Bisq's AVAILABLE -> RESERVED transition. A stale relay copy is never
		// sufficient authorization for another trade.
		if !ok || currentErr != nil || current.Status != "open" || owned.ID != request.OfferEvent.ID {
			return e.queue(from, "rejected", request.ID, map[string]string{"reason": "order is unavailable or changed"})
		}
		if e.balances[o.Sell] < o.SellAmount+protocol.FundingFee {
			return e.queue(from, "rejected", request.ID, map[string]string{"reason": "maker lacks confirmed balance"})
		}
		tower, err := e.selectedTower(o)
		if err != nil {
			return err
		}
		keys, err := e.swapKeys(request.ID)
		if err != nil {
			return err
		}
		terms, err := protocol.NewTermsWithClocks(request, keys, e.heights, e.clocks, tower.PubKey, tower.Scripts)
		if err != nil {
			return err
		}
		s := &Swap{ID: request.ID, Role: "maker", Request: request, Terms: &terms, Long: terms.Long, Short: terms.Short, Receipts: map[string]protocol.Receipt{}, Stage: "awaiting taker funding"}
		e.s.Swaps[s.ID] = s
		o.Status = "reserved"
		o.Reservation = s.ID
		if err = e.publishOffer(o); err != nil {
			return err
		}
		if err := e.save(); err != nil {
			return err
		} // Reservation is durable before acceptance can be sent.
		return e.queue(from, "accepted", s.ID, terms)
	}
	s := e.s.Swaps[m.SwapID]
	if s == nil {
		return errors.New("unknown swap")
	}
	if m.Type == "rejected" {
		if s.Role != "taker" || s.Terms != nil || from != s.Request.OfferEvent.PubKey.Hex() {
			return errors.New("unauthorized or late rejection")
		}
		var reason struct {
			Reason string `json:"reason"`
		}
		if err := json.Unmarshal(m.Body, &reason); err != nil {
			return err
		}
		if len(reason.Reason) > 160 {
			return errors.New("rejection too long")
		}
		s.Stage = "rejected"
		delete(e.s.CoinReservations, "swap/"+s.ID)
		s.Error = reason.Reason
		return nil
	}
	if m.Type == "accepted" {
		if s.Role != "taker" {
			return errors.New("unexpected acceptance")
		}
		var terms protocol.Terms
		if err := json.Unmarshal(m.Body, &terms); err != nil {
			return err
		}
		if err := terms.Validate(); err != nil {
			return err
		}
		if protocol.Digest(terms.Request) != protocol.Digest(s.Request) || from != terms.Offer().Maker {
			return errors.New("acceptance changed request or maker")
		}
		tower, err := e.selectedTower(terms.Offer())
		if err != nil {
			return err
		}
		if terms.Offer().TowerBPS > 0 && (terms.Tower != tower.PubKey || terms.Offer().TowerBPS != tower.BPS || protocol.Digest(terms.TowerScripts) != protocol.Digest(tower.Scripts)) {
			return errors.New("unapproved tower quote")
		}
		if s.Terms != nil {
			if protocol.Digest(s.Terms) != protocol.Digest(terms) {
				return errors.New("terms changed after acceptance")
			}
			return nil
		}
		s.Terms = &terms
		s.Long = terms.Long
		s.Short = terms.Short
		s.Stage = "accepted"
		return nil
	}
	if s.Terms == nil {
		return errors.New("message before accepted terms")
	}
	if m.Type == "tower-receipt" {
		if from != s.Terms.Tower {
			return errors.New("receipt from unselected tower")
		}
		var receipt protocol.Receipt
		if err := json.Unmarshal(m.Body, &receipt); err != nil {
			return err
		}
		for _, job := range s.Jobs {
			if job.ID == receipt.JobID && protocol.Digest(job) == receipt.Digest {
				s.Receipts[job.ID] = receipt
				return nil
			}
		}
		return errors.New("receipt does not commit to a requested job")
	}
	peer := s.Terms.Party("maker")
	if s.Role == "maker" {
		peer = s.Terms.Party("taker")
	}
	if from != peer {
		return errors.New("message from wrong counterparty")
	}
	if m.Type == "long-funded" || m.Type == "short-funded" {
		var f fundingMessage
		if err := json.Unmarshal(m.Body, &f); err != nil {
			return err
		}
		if f.TermsHash != protocol.Digest(s.Terms) {
			return errors.New("funding terms mismatch")
		}
		if m.Type == "long-funded" {
			if s.Role != "maker" {
				return errors.New("unexpected long funding")
			}
			c, err := bindFunding(s.Terms.Long, f.Raw)
			if err != nil {
				return err
			}
			if s.Long.TxID != "" && s.Long.TxID != c.TxID {
				return errors.New("long funding replacement requires new negotiation")
			}
			s.Long = c
			s.LongFunding = f.Raw
		} else {
			if s.Role != "taker" {
				return errors.New("unexpected short funding")
			}
			c, err := bindFunding(s.Terms.Short, f.Raw)
			if err != nil {
				return err
			}
			if s.Short.TxID != "" && s.Short.TxID != c.TxID {
				return errors.New("short funding replacement requires new negotiation")
			}
			s.Short = c
			s.ShortFunding = f.Raw
		}
		return nil
	}
	return errors.New("unknown swap message type")
}

func (e *Engine) registrationWindow(id chain.ID, refund uint32) bool {
	now := e.clocks[id]
	horizon := uint64(protocol.LongBlocks)
	if e.Config.Network != chain.Regtest {
		if e.clocks[chain.BTC] < protocol.TimeLockThreshold || e.clocks[chain.Blake] < protocol.TimeLockThreshold {
			return false
		}
		now = max(e.clocks[chain.BTC], e.clocks[chain.Blake])
		horizon = uint64(protocol.LongSeconds) + uint64(protocol.MaxClockSkew)
	}
	return now > 0 && refund > now && uint64(refund) <= uint64(now)+horizon
}
