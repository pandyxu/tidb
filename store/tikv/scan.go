// Copyright 2016 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package tikv

import (
	"github.com/golang/protobuf/proto"
	"github.com/juju/errors"
	"github.com/ngaut/log"
	pb "github.com/pingcap/kvproto/pkg/kvrpcpb"
	"github.com/pingcap/tidb/kv"
	"github.com/pingcap/tidb/terror"
)

// Scanner support tikv scan
type Scanner struct {
	snapshot     *tikvSnapshot
	batchSize    int
	valid        bool
	cache        []*pb.RowValue
	idx          int
	nextStartKey []byte
	eof          bool
}

func newScanner(snapshot *tikvSnapshot, startKey []byte, batchSize int) (*Scanner, error) {
	// It must be > 1. Otherwise scanner won't skipFirst.
	if batchSize <= 1 {
		batchSize = scanBatchSize
	}
	scanner := &Scanner{
		snapshot:     snapshot,
		batchSize:    batchSize,
		valid:        true,
		nextStartKey: startKey,
	}
	err := scanner.Next()
	if kv.IsErrNotFound(err) {
		return scanner, nil
	}
	return scanner, errors.Trace(err)
}

// Valid return valid.
func (s *Scanner) Valid() bool {
	return s.valid
}

// Key return key.
func (s *Scanner) Key() kv.Key {
	if s.valid {
		return s.cache[s.idx].GetRowKey()
	}
	return nil
}

// Value return value.
func (s *Scanner) Value() []byte {
	if s.valid {
		return defaultRowValue(s.cache[s.idx])
	}
	return nil
}

// Next return next element.
func (s *Scanner) Next() error {
	if !s.valid {
		return errors.New("scanner iterator is invalid")
	}
	for {
		s.idx++
		if s.idx >= len(s.cache) {
			if s.eof {
				s.Close()
				return kv.ErrNotExist
			}
			err := s.getData()
			if err != nil {
				s.Close()
				return errors.Trace(err)
			}
			if s.idx >= len(s.cache) {
				continue
			}
		}
		if err := s.resolveCurrentLock(); err != nil {
			s.Close()
			return errors.Trace(err)
		}
		if len(s.Value()) == 0 {
			// nil stands for NotExist, go to next KV pair.
			continue
		}
		return nil
	}
}

// Close close iterator.
func (s *Scanner) Close() {
	s.valid = false
}

func (s *Scanner) startTS() uint64 {
	return s.snapshot.version.Ver
}

func (s *Scanner) resolveCurrentLock() error {
	current := s.cache[s.idx]
	if current.GetError() == nil {
		return nil
	}
	var backoffErr error
	for backoff := txnLockBackoff(); backoffErr == nil; backoffErr = backoff() {
		val, err := s.snapshot.handleKeyError(current.GetError())
		if err != nil {
			if terror.ErrorEqual(err, errInnerRetryable) {
				continue
			}
			return errors.Trace(err)
		}
		current.Error = nil
		current.Columns = defaultColumn
		current.Values = [][]byte{val}
		return nil
	}
	return errors.Annotate(backoffErr, txnRetryableMark)
}

func (s *Scanner) getData() error {
	log.Debugf("txn getData nextStartKey[%q], txn %d", s.nextStartKey, s.startTS())

	var backoffErr error
	for backoff := regionMissBackoff(); backoffErr == nil; backoffErr = backoff() {
		region, err := s.snapshot.store.regionCache.GetRegion(s.nextStartKey)
		if err != nil {
			return errors.Trace(err)
		}
		req := &pb.Request{
			Type: pb.MessageType_CmdScan.Enum(),
			CmdScanReq: &pb.CmdScanRequest{
				StartRow: defaultRow(s.nextStartKey),
				Limit:    proto.Uint32(uint32(s.batchSize)),
				Ts:       proto.Uint64(s.startTS()),
			},
		}
		resp, err := s.snapshot.store.SendKVReq(req, region.VerID())
		if err != nil {
			return errors.Trace(err)
		}
		if regionErr := resp.GetRegionError(); regionErr != nil {
			log.Warnf("scanner getData failed: %s", regionErr)
			continue
		}
		cmdScanResp := resp.GetCmdScanResp()
		if cmdScanResp == nil {
			return errors.Trace(errBodyMissing)
		}

		rows := cmdScanResp.GetRows()
		// Check if row contains error, it should be a Lock.
		for _, row := range rows {
			if keyErr := row.GetError(); keyErr != nil {
				lock, err := extractLockInfoFromKeyErr(keyErr)
				if err != nil {
					return errors.Trace(err)
				}

				row.RowKey = lock.GetRow()
			}
		}

		s.cache, s.idx = rows, 0
		if len(rows) < s.batchSize {
			// No more data in current Region. Next getData() starts
			// from current Region's endKey.
			s.nextStartKey = region.EndKey()
			if len(region.EndKey()) == 0 {
				// Current Region is the last one.
				s.eof = true
			}
			return nil
		}
		// next getData() starts from the last key in kvPairs (but skip
		// it by appending a '\x00' to the key). Note that next getData()
		// may get an empty response if the Region in fact does not have
		// more data.
		lastKey := rows[len(rows)-1].GetRowKey()
		s.nextStartKey = kv.Key(lastKey).Next()
		return nil
	}
	return errors.Annotate(backoffErr, txnRetryableMark)
}
