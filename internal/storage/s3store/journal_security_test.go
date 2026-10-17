package s3store

import (
	"errors"
	"math"
	"testing"
)

func TestDecodeBatchRejectsUntrustedCounts(t *testing.T) {
	for _, h := range []batchHeader{
		{N: math.MaxInt, LSN: [2]uint64{1, math.MaxInt}},
		{N: -1, LSN: [2]uint64{1, 0}},
		{N: 2, LSN: [2]uint64{math.MaxUint64, 0}},
		{N: 1, LSN: [2]uint64{1, 1}},
	} {
		data := encodeBatch(&h, nil)
		if _, _, err := decodeBatch(data); !errors.Is(err, ErrCorruptJournal) {
			t.Fatalf("header %+v: %v", h, err)
		}
	}
}

func TestDecodeBatchRecordCount(t *testing.T) {
	for _, payload := range []string{`{"lsn":1,"r":{}}`, "{\"lsn\":1,\"r\":{}}\n"} {
		h := batchHeader{N: 1, LSN: [2]uint64{1, 1}}
		if _, recs, err := decodeBatch(encodeBatch(&h, []byte(payload))); err != nil || len(recs) != 1 {
			t.Fatalf("valid batch: %v, %v", recs, err)
		}
		h.N, h.LSN[1] = 0, 0
		if _, _, err := decodeBatch(encodeBatch(&h, []byte(payload))); !errors.Is(err, ErrCorruptJournal) {
			t.Fatalf("extra record accepted: %v", err)
		}
	}
}

func FuzzDecodeBatch(f *testing.F) {
	h := batchHeader{N: 1, LSN: [2]uint64{1, 1}}
	f.Add(encodeBatch(&h, []byte("{\"lsn\":1,\"r\":{}}\n")))
	h.N, h.LSN[1] = math.MaxInt, math.MaxInt
	f.Add(encodeBatch(&h, nil))
	f.Fuzz(func(t *testing.T, data []byte) { _, _, _ = decodeBatch(data) })
}
