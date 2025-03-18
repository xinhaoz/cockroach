// Copyright 2024 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package sslocal

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cockroachdb/cockroach/pkg/sql/clusterunique"
	"github.com/cockroachdb/cockroach/pkg/sql/contention/contentionutils"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlstats"
	"github.com/cockroachdb/cockroach/pkg/util/stop"
)

// defaultFlushInterval specifies a default for the amount of time an ingester
// will go before flushing its contents to the registry.
const defaultFlushInterval = time.Second * 1

type SQLStatsSink interface {
	// ObserveTransaction is called by the ingester to pass along a transaction event (possibly nil) and its
	// statementsBySessionID.
	// Note that the sink should transform the transaction and statementsBySessionID into the appropriate format
	// as these objects will be returned to the pool.
	ObserveTransaction(ctx context.Context, transaction *sqlstats.RecordedTxnStats, statements []*sqlstats.RecordedStmtStats)
}

// SQLStatsIngester collects and buffers statements and transactions per session
// before passing them to the sinks when we receive complete information for a transaction.
// Built around contentionutils.ConcurrentBufferGuard.
type SQLStatsIngester struct {
	guard struct {
		*contentionutils.ConcurrentBufferGuard
		eventBuffer *eventBuffer
	}

	opts struct {
		// noTimedFlush prevents time-triggered flushes from being scheduled.
		noTimedFlush bool
		// flushInterval is an optional override flush interval
		// a value of zero will be set to the 500ms default.
		flushInterval time.Duration
	}

	// We buffer ingested statementsBySessionID by session id.
	statementsBySessionID map[clusterunique.ID]*statementBuf
	resetStatementsBuf    atomic.Bool

	sinks []SQLStatsSink

	eventBufferCh chan eventBufChPayload

	closeCh      chan struct{}
	testingKnobs *sqlstats.TestingKnobs
}

type eventBufChPayload struct {
	resetStatementsBuf bool
	events             *eventBuffer
}

type statementBuf []*sqlstats.RecordedStmtStats

func (b *statementBuf) append(statement *sqlstats.RecordedStmtStats) {
	*b = append(*b, statement)
}

func (b *statementBuf) release() {
	for i, n := 0, len(*b); i < n; i++ {
		(*b)[i] = nil
	}
	*b = (*b)[:0]
	statementsBufPool.Put(b)
}

var statementsBufPool = sync.Pool{
	New: func() interface{} {
		return new(statementBuf)
	},
}

// SQLStatsIngester buffers the "events" it sees (via IngestStatement
// and IngestTransaction) and passes them along to the underlying registry
// once its buffer is full. (Or once a timeout has passed, for low-traffic
// clusters and tests.)
//
// The bufferSize was set at 8192 after experimental micro-benchmarking ramping
// up the number of goroutines writing through the ingester concurrently.
// Performance was deemed acceptable under 10,000 concurrent goroutines.
const bufferSize = 8192

type eventBuffer [bufferSize]event

var eventBufferPool = sync.Pool{
	New: func() interface{} { return new(eventBuffer) },
}

// event is a single event that can be observed by the ingester.
// Only one of the fields will be non-nil.
type event struct {
	sessionID   clusterunique.ID
	transaction *sqlstats.RecordedTxnStats
	statement   *sqlstats.RecordedStmtStats
}

type BufferOpt func(i *SQLStatsIngester)

// WithoutTimedFlush prevents the ConcurrentBufferIngester from performing
// timed flushes to the underlying registry. Generally only useful for
// testing purposes.
func WithoutTimedFlush() BufferOpt {
	return func(i *SQLStatsIngester) {
		i.opts.noTimedFlush = true
	}
}

// WithFlushInterval allows for the override of the default flush interval
func WithFlushInterval(intervalMS int) BufferOpt {
	return func(i *SQLStatsIngester) {
		i.opts.flushInterval = time.Millisecond * time.Duration(intervalMS)
	}
}

func (i *SQLStatsIngester) Start(ctx context.Context, stopper *stop.Stopper, opts ...BufferOpt) {
	for _, opt := range opts {
		opt(i)
	}
	_ = stopper.RunAsyncTask(ctx, "sql-stats-ingester", func(ctx context.Context) {

		for {
			select {
			case payload := <-i.eventBufferCh:
				i.ingest(ctx, payload.events) // note that ingest clears the buffer
				if payload.resetStatementsBuf {
					i.statementsBySessionID = make(map[clusterunique.ID]*statementBuf)
				}
				eventBufferPool.Put(payload.events)
			case <-stopper.ShouldQuiesce():
				close(i.closeCh)
				return
			}
		}
	})

	if !i.opts.noTimedFlush {
		flushInterval := i.opts.flushInterval
		if flushInterval == 0 {
			flushInterval = defaultFlushInterval
		}
		// This task eagerly flushes partial buffers into the channel, to avoid
		// delays identifying insights in low-traffic clusters and tests.
		_ = stopper.RunAsyncTask(ctx, "insights-ingester-flush", func(ctx context.Context) {
			ticker := time.NewTicker(flushInterval)

			for {
				select {
				case <-ticker.C:
					i.guard.ForceSync()
				case <-stopper.ShouldQuiesce():
					ticker.Stop()
					return
				}
			}
		})
	}
}

// Clear flushes the underlying buffer, and signals the underlying registry
// to clear any remaining cached data afterward. This is an async operation.
func (i *SQLStatsIngester) Clear() {
	i.guard.ForceSyncExec(func() {
		// Our flush function defined on the guard is responsible for setting resetStatementsBuf back to 0.
		i.resetStatementsBuf.Store(true)
	})
}

func (i *SQLStatsIngester) ingest(ctx context.Context, events *eventBuffer) {
	for idx, e := range events {
		// Because an eventBuffer is a fixed-size array, rather than a slice,
		// we do not know how full it is until we hit a nil entry.
		if e == (event{}) {
			break
		}
		if e.statement != nil {
			i.processStatement(e.statement.SessionID, e.statement)
			// When under an outer transaction, we don't have a txn to associate
			// the stmts with so we can send immediately to the sinks.
			if e.statement.UnderOuterTxn {
				i.processTransaction(ctx, e.statement.SessionID, nil)
			}
		} else if e.transaction != nil {
			i.processTransaction(ctx, e.transaction.SessionID, e.transaction)
		} else {
			i.clearSession(e.sessionID)
		}
		events[idx] = event{}
	}
}

func (i *SQLStatsIngester) IngestStatement(statement *sqlstats.RecordedStmtStats) {
	i.guard.AtomicWrite(func(writerIdx int64) {
		i.guard.eventBuffer[writerIdx] = event{
			sessionID: statement.SessionID,
			statement: statement,
		}
	})
}

func (i *SQLStatsIngester) IngestTransaction(transaction *sqlstats.RecordedTxnStats) {
	i.guard.AtomicWrite(func(writerIdx int64) {
		i.guard.eventBuffer[writerIdx] = event{
			transaction: transaction,
		}
	})
}

// ClearSession sends a signal to the underlying registry to clear any cached
// data associated with the given sessionID. This is an async operation.
func (i *SQLStatsIngester) ClearSession(sessionID clusterunique.ID) {
	i.guard.AtomicWrite(func(writerIdx int64) {
		i.guard.eventBuffer[writerIdx] = event{
			sessionID: sessionID,
		}
	})
}

func NewSQLStatsIngester(sinks ...SQLStatsSink) *SQLStatsIngester {
	i := &SQLStatsIngester{
		// A channel size of 1 is sufficient to avoid unnecessarily
		// synchronizing producer (our clients) and consumer (the underlying
		// registry): moving from 0 to 1 here resulted in a 25% improvement
		// in the micro-benchmarks, but further increases had no effect.
		// Otherwise, we rely solely on the size of the eventBuffer for
		// adjusting our carrying capacity.
		eventBufferCh:         make(chan eventBufChPayload, 2),
		closeCh:               make(chan struct{}),
		statementsBySessionID: make(map[clusterunique.ID]*statementBuf),
		sinks:                 sinks,
	}

	i.guard.eventBuffer = eventBufferPool.Get().(*eventBuffer)
	i.guard.ConcurrentBufferGuard = contentionutils.NewConcurrentBufferGuard(
		func() int64 {
			return bufferSize
		},
		func(currentWriterIndex int64) {
			clearBuf := i.resetStatementsBuf.Load()
			if clearBuf {
				defer func() {
					i.resetStatementsBuf.Store(false)
				}()
			}
			select {
			case i.eventBufferCh <- eventBufChPayload{
				resetStatementsBuf: clearBuf,
				events:             i.guard.eventBuffer,
			}:
			case <-i.closeCh:
			}
			i.guard.eventBuffer = eventBufferPool.Get().(*eventBuffer)
		},
	)
	return i
}

// clearSession removes the session from the registry and releases the
// associated statement buffer.
func (i *SQLStatsIngester) clearSession(sessionID clusterunique.ID) {
	if b, ok := i.statementsBySessionID[sessionID]; ok {
		delete(i.statementsBySessionID, sessionID)
		b.release()
	}
}

func (i *SQLStatsIngester) processStatement(
	sessionID clusterunique.ID, statement *sqlstats.RecordedStmtStats,
) {
	b, ok := i.statementsBySessionID[sessionID]
	if !ok {
		b = statementsBufPool.Get().(*statementBuf)
		i.statementsBySessionID[sessionID] = b
	}
	b.append(statement)
}

// processTransaction takes the provided transaction and statementsBySessionID associated with the
// session and passes them to the registered sinks. It is possible that transaction is nil here.
func (i *SQLStatsIngester) processTransaction(
	ctx context.Context, sessionID clusterunique.ID, transaction *sqlstats.RecordedTxnStats,
) {
	statements, ok := func() (*statementBuf, bool) {
		statements, ok := i.statementsBySessionID[sessionID]
		if !ok {
			return nil, false
		}
		delete(i.statementsBySessionID, sessionID)
		return statements, true
	}()
	if !ok {
		return
	}
	defer statements.release()

	if len(*statements) == 0 {
		return
	}

	// Set the transaction fingerprint ID for each statement.
	// These values are only known at the time of the transaction.
	for _, s := range *statements {
		s.TransactionFingerprintID = transaction.FingerprintID
		s.ImplicitTxn = transaction.ImplicitTxn
	}

	for _, sink := range i.sinks {
		sink.ObserveTransaction(ctx, transaction, *statements)
	}
}
