package chain

import (
	"context"
	"encoding/binary"
	"errors"
	"sort"
)

func (r *RPC) MedianTime(ctx context.Context) (uint32, error) {
	var info struct {
		MedianTime uint32 `json:"mediantime"`
	}
	err := r.Call(ctx, "getblockchaininfo", &info)
	return info.MedianTime, err
}
func (e *Electrum) MedianTime(ctx context.Context) (uint32, error) {
	height, err := e.Height(ctx)
	if err != nil {
		return 0, err
	}
	if height < 10 {
		return 0, errors.New("chain too short for median time")
	}
	var times []uint32
	for i := uint32(0); i < 11; i++ {
		h, err := e.header(ctx, height-i)
		if err != nil {
			return 0, err
		}
		stamp := binary.LittleEndian.Uint32(h[68:72])
		if len(h) == 164 && h[110]&4 != 0 {
			stamp += binary.LittleEndian.Uint32(h[104:108])
		}
		times = append(times, stamp)
	}
	sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
	return times[5], nil
}
