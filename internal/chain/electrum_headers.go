package chain

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
)

const headerBatchSize = 2016

type headerRange struct {
	first, last chainhash.Hash
	end         uint32
}

// headers accepts short batches, but never empty, oversized or partially parsed
// replies. Header widths can change inside a batch at Blake2b activation.
func (e *Electrum) headers(ctx context.Context, start, count uint32) ([][]byte, error) {
	var reply struct {
		Count uint32 `json:"count"`
		Hex   string `json:"hex"`
	}
	if count == 0 || count > headerBatchSize || uint64(start)+uint64(count)-1 > uint64(^uint32(0)) {
		return nil, errors.New("invalid header range")
	}
	if err := e.Call(ctx, "blockchain.block.headers", &reply, start, count); err != nil {
		return nil, err
	}
	if reply.Count == 0 || reply.Count > count {
		return nil, errors.New("invalid header batch count")
	}
	raw, err := hex.DecodeString(reply.Hex)
	if err != nil {
		return nil, err
	}
	var result [][]byte
	for i := uint32(0); i < reply.Count; i++ {
		size := e.headerSize(start + i)
		if len(raw) < size {
			return nil, errors.New("truncated header batch")
		}
		h := raw[:size]
		if err := e.validateHeader(h, start+i); err != nil {
			return nil, err
		}
		result = append(result, h)
		raw = raw[size:]
	}
	if len(raw) != 0 {
		return nil, errors.New("trailing header batch data")
	}
	return result, nil
}

func linkedHeaders(parent, child []byte) bool {
	hash, err := HeaderHash(parent)
	return err == nil && len(child) >= 36 && bytes.Equal(hash[:], child[4:36])
}

// connectHeaders proves every link from an observed block to the subscribed
// tip. It does not establish difficulty transitions or most-work canonicality.
// Cache verified batches so a caller's polling deadline does not discard all
// progress. Recheck both ends before resuming; no confirmation count is exposed
// until the full range reaches the subscribed tip.
func (e *Electrum) connectHeaders(ctx context.Context, start uint32, first []byte, end uint32, last []byte) error {
	e.rangeMu.Lock()
	defer e.rangeMu.Unlock()
	if start > end {
		return errors.New("invalid header range")
	}
	firstHash, err := HeaderHash(first)
	if err != nil {
		return err
	}
	position, previous := start, first
	if cached, ok := e.ranges[start]; ok && cached.first == firstHash && cached.end <= end {
		anchor := last
		if cached.end != end {
			anchor, err = e.header(ctx, cached.end)
			if err != nil {
				return err
			}
		}
		hash, _ := HeaderHash(anchor)
		if hash == cached.last {
			position, previous = cached.end, anchor
		}
	}
	for position < end {
		batch, err := e.headers(ctx, position+1, min(uint32(headerBatchSize), end-position))
		if err != nil {
			return err
		}
		for _, h := range batch {
			if !linkedHeaders(previous, h) {
				return errors.New("disconnected Electrum headers")
			}
			previous = h
			position++
		}
		if e.ranges == nil || len(e.ranges) >= 128 {
			e.ranges = make(map[uint32]headerRange)
		}
		hash, _ := HeaderHash(previous)
		e.ranges[start] = headerRange{first: firstHash, last: hash, end: position}
	}
	if !bytes.Equal(previous, last) {
		return errors.New("header range does not reach subscribed tip")
	}
	return nil
}
