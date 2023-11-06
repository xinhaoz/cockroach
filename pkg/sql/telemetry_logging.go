// Copyright 2021 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package sql

import (
	"context"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/cockroachdb/cockroach/pkg/settings"
	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	"github.com/cockroachdb/cockroach/pkg/sql/clusterunique"
	"github.com/cockroachdb/cockroach/pkg/sql/execstats"
	"github.com/cockroachdb/cockroach/pkg/util/syncutil"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
	"github.com/cockroachdb/cockroach/pkg/util/tracing"
)

// Default value used to designate the maximum frequency at which events
// are logged to the telemetry channel.
const defaultMaxEventFrequency = 8

var TelemetryMaxStatementEventFrequency = settings.RegisterIntSetting(
	settings.ApplicationLevel,
	"sql.telemetry.query_sampling.max_event_frequency",
	"the max event frequency at which we sample executions for telemetry, "+
		"note that it is recommended that this value shares a log-line limit of 10 "+
		" logs per second on the telemetry pipeline with all other telemetry events. "+
		"If sampling mode is set to 'transaction', all statements associated with a single "+
		"transaction are counted as 1 unit.",
	defaultMaxEventFrequency,
	settings.NonNegativeInt,
	settings.WithPublic,
)

var telemetryTransactionSamplingFrequency = settings.RegisterIntSetting(
	settings.ApplicationLevel,
	"sql.telemetry.transaction_sampling.frequency",
	"desc", // TODO (xinhaoz)
	defaultMaxEventFrequency,
	settings.NonNegativeInt,
	settings.WithPublic,
)

var telemetryQueriesPerTransactionMax = settings.RegisterIntSetting(
	settings.ApplicationLevel,
	"sql.telemetry.transaction_sampling.statement_events_per_transaction.max",
	"desc", // TODO (xinhaoz)
	20,
	settings.NonNegativeInt,
	settings.WithPublic,
)

var telemetryInternalQueriesEnabled = settings.RegisterBoolSetting(
	settings.ApplicationLevel,
	"sql.telemetry.query_sampling.internal.enabled",
	"when set to true, internal queries will be sampled in telemetry logging",
	false,
	settings.WithPublic)

var telemetryInternalConsoleQueriesEnabled = settings.RegisterBoolSetting(
	settings.ApplicationLevel,
	"sql.telemetry.query_sampling.internal_console.enabled",
	"when set to true, all internal queries used to populated the UI Console"+
		"will be logged into telemetry",
	true,
)

const (
	telemetryModeStatement = iota
	telemetryModeTransaction
)

var telemetrySamplingMode = settings.RegisterEnumSetting(
	settings.ApplicationLevel,
	"sql.telemetry.query_sampling.mode",
	"the execution level used for telemetry sampling. If set to 'statement', events "+
		"are sampled at the statement execution level. If set to 'transaction', events are "+
		"sampled at the txn execution level, i.e. all statements for a txn will be logged "+
		"and are counted together as one sampled event (events are still emitted one per "+
		"statement)",
	"statement",
	map[int64]string{
		telemetryModeStatement:   "statement",
		telemetryModeTransaction: "transaction",
	},
	settings.WithPublic,
)

var telemetryTrackedTxnsLimit = settings.RegisterIntSetting(
	settings.ApplicationLevel,
	"sql.telemetry.txn_mode.tracking_limit",
	"the maximum number of transactions tracked at one time for which we will send "+
		"all statements to telemetry",
	10000,
	settings.NonNegativeInt,
	settings.WithPublic,
)

// We need to track the transaction execution for transaction telemetry logging.
// We can't use the transaction execution id since it is not available for BEGIN
// statements, so we use the session id and txn counter to track the transaction through
// its execution.
type telemetryTransactionID string

func createTelemetryTransactionID(
	sessionID clusterunique.ID, txnCounter int,
) telemetryTransactionID {
	return telemetryTransactionID(sessionID.String() + "/" + strconv.Itoa(txnCounter))
}

// TelemetryLoggingMetrics keeps track of the last time at which an event
// was logged to the telemetry channel, and the number of skipped queries
// since the last logged event.
type TelemetryLoggingMetrics struct {
	st *cluster.Settings

	mu struct {
		syncutil.RWMutex
		// The timestamp of the last emitted execution event.
		lastEmittedTime time.Time

		// observedTxnExecutions is a map of transaction id to number of statements emitted
		// to telemetry. It is used to track txn executions that are currently being logged
		// and its statement events. When the sampling mode is set to "transaction", we log all stmts
		// for a txn up to a max of sql.telemetry.transaction_sampling.statements_events_per_transaction.max.
		// Entries are removed upon completing execution.
		observedTxnExecutions map[telemetryTransactionID]int64
	}

	Knobs *TelemetryLoggingTestingKnobs

	// skippedQueryCount is used to produce the count of non-sampled queries.
	skippedQueryCount atomic.Uint64

	// skippedTransactionCount is used to produce the count of non-sampled transactions.
	skippedTransactionCount atomic.Uint64
}

func newTelemetryLoggingMetrics(
	knobs *TelemetryLoggingTestingKnobs, st *cluster.Settings,
) *TelemetryLoggingMetrics {
	t := TelemetryLoggingMetrics{Knobs: knobs, st: st}
	t.mu.observedTxnExecutions = make(map[telemetryTransactionID]int64)
	return &t
}

// TelemetryLoggingTestingKnobs provides hooks and knobs for unit tests.
type TelemetryLoggingTestingKnobs struct {
	// getTimeNow allows tests to override the timeutil.Now() function used
	// when updating rolling query counts.
	getTimeNow func() time.Time
	// getQueryLevelMetrics allows tests to override the recorded query level stats.
	getQueryLevelStats func() execstats.QueryLevelStats
	// getTracingStatus allows tests to override whether the current query has tracing
	// enabled or not. Queries with tracing enabled are always sampled to telemetry.
	getTracingStatus func() bool
}

func NewTelemetryLoggingTestingKnobs(
	getTimeNowFunc func() time.Time,
	getQueryLevelStatsFunc func() execstats.QueryLevelStats,
	getTracingStatusFunc func() bool,
) *TelemetryLoggingTestingKnobs {
	return &TelemetryLoggingTestingKnobs{
		getTimeNow:         getTimeNowFunc,
		getQueryLevelStats: getQueryLevelStatsFunc,
		getTracingStatus:   getTracingStatusFunc,
	}
}

// registerOnTelemetrySamplingModeChange sets up the callback for when the
// telemetry sampling mode is changed. When switching from txn to stmt, we
// clear the txns we are currently tracking for logging.
func (t *TelemetryLoggingMetrics) registerOnTelemetrySamplingModeChange(
	settings *cluster.Settings,
) {
	telemetrySamplingMode.SetOnChange(&settings.SV, func(ctx context.Context) {
		mode := telemetrySamplingMode.Get(&settings.SV)
		t.mu.Lock()
		defer t.mu.Unlock()
		if mode == telemetryModeStatement {
			// Clear currently observed txns.
			t.mu.observedTxnExecutions = make(map[telemetryTransactionID]int64)
		}
	})
}

func (t *TelemetryLoggingMetrics) onTxnFinish(txnID telemetryTransactionID) {
	if telemetrySamplingMode.Get(&t.st.SV) != telemetryModeTransaction {
		return
	}
	// Check if txn exec id exists in the map.
	exists := false
	func() {
		t.mu.RLock()
		defer t.mu.RUnlock()
		_, exists = t.mu.observedTxnExecutions[txnID]
	}()

	if !exists {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.mu.observedTxnExecutions, txnID)
}

func (t *TelemetryLoggingMetrics) getTrackedTxnsCount() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.mu.observedTxnExecutions)
}

// ModuleTestingKnobs implements base.ModuleTestingKnobs interface.
func (*TelemetryLoggingTestingKnobs) ModuleTestingKnobs() {}

func (t *TelemetryLoggingMetrics) timeNow() time.Time {
	if t.Knobs != nil && t.Knobs.getTimeNow != nil {
		return t.Knobs.getTimeNow()
	}
	return timeutil.Now()
}

// shouldEmitStatementLog returns true if the stmt should be logged to telemetry. The last emitted time
// tracked by telemetry logging metrics will be updated to the given time if any of the following
// are met:
//   - The telemetry mode is set to "transaction" AND the stmt is the first in
//     the txn AND the txn is not already being tracked AND the required amount
//     of time has elapsed.
//   - The telemetry mode is set to "statement" AND the required amount of time has elapsed
//   - The txn is not being tracked and the stmt is being forced to log.
func (t *TelemetryLoggingMetrics) shouldEmitStatementLog(
	newTime time.Time, txnExecutionID telemetryTransactionID, force bool, isFirstStmt bool,
) bool {
	isTxnMode := telemetrySamplingMode.Get(&t.st.SV) == telemetryModeTransaction
	maxEventFrequency := TelemetryMaxStatementEventFrequency.Get(&t.st.SV)

	if isTxnMode {
		// Use the max transaction event frequency since we are deciding
		// whether or not to track a transaction.
		maxEventFrequency = telemetryTransactionSamplingFrequency.Get(&t.st.SV)
	}

	requiredTimeElapsed := time.Second / time.Duration(maxEventFrequency)
	txnsLimit := int(telemetryTrackedTxnsLimit.Get(&t.st.SV))

	var enoughTimeElapsed, txnIsTracked, startTrackingTxn bool
	var stmtsLogged int64
	// Avoid taking the full lock if we don't have to.
	func() {
		t.mu.RLock()
		defer t.mu.RUnlock()

		enoughTimeElapsed = newTime.Sub(t.mu.lastEmittedTime) >= requiredTimeElapsed
		startTrackingTxn = isTxnMode && isFirstStmt && len(t.mu.observedTxnExecutions) < txnsLimit
		stmtsLogged, txnIsTracked = t.mu.observedTxnExecutions[txnExecutionID]
	}()

	if txnIsTracked {
		if stmtsLogged >= telemetryQueriesPerTransactionMax.Get(&t.st.SV) {
			// We are at the limit for the number of statements logged
			// for this transaction.
			return false
		}

		// We don't want to update the last emitted time if the transaction is already tracked.
		t.mu.Lock()
		defer t.mu.Unlock()
		t.mu.observedTxnExecutions[txnExecutionID] = stmtsLogged + 1
		return true
	}

	if !force && (!enoughTimeElapsed || (isTxnMode && !startTrackingTxn)) {
		// We can also early exit if we aren't forcing the log and we don't meed the required
		// elapsed time or can't start tracking the txn due to not having received the first stmt.
		return false
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	// The lastEmittedTime and tracked txns may have changed since releasing the Rlock.
	// The tracked transaction count could have changed as well so we recheck these values.
	txnLimitReached := len(t.mu.observedTxnExecutions) == txnsLimit
	if !force &&
		(newTime.Sub(t.mu.lastEmittedTime) < requiredTimeElapsed || (startTrackingTxn && txnLimitReached)) {
		return false
	}

	// We could be forcing the log so we should only track its txn if it meets the criteria.
	if startTrackingTxn {
		t.mu.observedTxnExecutions[txnExecutionID] = 1
	}

	t.mu.lastEmittedTime = newTime
	return true
}

// shouldEmitTransactionLog returns true if we should emit transaction event
// to telemetry. At this point in execution we should have already started
// to emit the statement events for the given transaction execution if it
// is being sampled.
func (t *TelemetryLoggingMetrics) shouldEmitTransactionLog(
	txnExecutionID telemetryTransactionID, force bool,
) bool {
	if telemetrySamplingMode.Get(&t.st.SV) != telemetryModeTransaction {
		return false
	}

	if force {
		return true
	}

	t.mu.RLock()
	defer t.mu.RUnlock()

	_, ok := t.mu.observedTxnExecutions[txnExecutionID]

	return ok
}

func (t *TelemetryLoggingMetrics) getQueryLevelStats(
	queryLevelStats execstats.QueryLevelStats,
) execstats.QueryLevelStats {
	if t.Knobs != nil && t.Knobs.getQueryLevelStats != nil {
		return t.Knobs.getQueryLevelStats()
	}
	return queryLevelStats
}

func (t *TelemetryLoggingMetrics) isTracing(_ *tracing.Span, tracingEnabled bool) bool {
	if t.Knobs != nil && t.Knobs.getTracingStatus != nil {
		return t.Knobs.getTracingStatus()
	}
	return tracingEnabled
}

func (t *TelemetryLoggingMetrics) resetSkippedQueryCount() (res uint64) {
	return t.skippedQueryCount.Swap(0)
}

func (t *TelemetryLoggingMetrics) incSkippedQueryCount() {
	t.skippedQueryCount.Add(1)
}

func (t *TelemetryLoggingMetrics) resetSkippedTransactionCount() (res uint64) {
	return t.skippedTransactionCount.Swap(0)
}

func (t *TelemetryLoggingMetrics) incSkippedTransactionCount() {
	t.skippedTransactionCount.Add(1)
}

func (t *TelemetryLoggingMetrics) ResetLastEmittedTime() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.mu.lastEmittedTime = time.Time{}
}
