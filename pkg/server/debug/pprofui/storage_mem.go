// Copyright 2018 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package pprofui

import (
	"bytes"
	"fmt"
	"io"
	"sort"
	"sync/atomic"
	"time"

	"github.com/cockroachdb/cockroach/pkg/util/syncutil"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
	"github.com/cockroachdb/errors"
)

type record struct {
	id string
	t  time.Time
	// isProfileProto is true when the profile record is stored in protobuf format
	// outlined in https://github.com/google/pprof/tree/main/proto#overview.
	isProfileProto bool
	b              []byte
}

// A MemStorage is a Storage implementation that holds recent profiles in memory.
type MemStorage struct {
	mu struct {
		syncutil.Mutex
		records []record // sorted by record.t
	}
	idGen        int32         // accessed atomically
	keepDuration time.Duration // zero for disabled
	keepNumber   int           // zero for disabled
}

func (s *MemStorage) GetRecords() []record {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]record(nil), s.mu.records...)
}

var _ Storage = &MemStorage{}

// NewMemStorage creates a MemStorage that retains the most recent n records
// as long as they are less than d old.
//
// Records are dropped only when there is activity (i.e. an old record will
// only be dropped the next time the storage is accessed).
func NewMemStorage(n int, d time.Duration) *MemStorage {
	return &MemStorage{
		keepNumber:   n,
		keepDuration: d,
	}
}

// ID implements Storage.
func (s *MemStorage) ID() string {
	return fmt.Sprint(atomic.AddInt32(&s.idGen, 1))
}

func (s *MemStorage) cleanLocked() {
	if l, m := len(s.mu.records), s.keepNumber; l > m && m != 0 {
		s.mu.records = append([]record(nil), s.mu.records[l-m:]...)
	}
	now := timeutil.Now()
	if pos := sort.Search(len(s.mu.records), func(i int) bool {
		return s.mu.records[i].t.Add(s.keepDuration).After(now)
	}); pos < len(s.mu.records) && s.keepDuration != 0 {
		s.mu.records = append([]record(nil), s.mu.records[pos:]...)
	}
}

// Store implements Storage.
func (s *MemStorage) Store(id string, isProfileProto bool, write func(io.Writer) error) error {
	var b bytes.Buffer
	if err := write(&b); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mu.records = append(s.mu.records, record{id: id, t: timeutil.Now(), b: b.Bytes(), isProfileProto: isProfileProto})
	sort.Slice(s.mu.records, func(i, j int) bool {
		return s.mu.records[i].t.Before(s.mu.records[j].t)
	})
	s.cleanLocked()
	return nil
}

// Get implements Storage.
func (s *MemStorage) Get(id string, read func(bool, io.Reader) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, v := range s.mu.records {
		if v.id == id {
			return read(v.isProfileProto, bytes.NewReader(v.b))
		}
	}
	return errors.Errorf("profile not found; it may have expired, please regenerate the profile.\n" +
		"To generate profile for a node, use the profile generation link from the Advanced Debug page.\n" +
		"Attempting to generate a profile by modifying the node query parameter in the URL will not work.",
	)
}
