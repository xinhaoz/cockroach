// Copyright 2021 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package cli

import (
	"bufio"
	"bytes"
	"context"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cockroachdb/cockroach/pkg/build"
	"github.com/cockroachdb/cockroach/pkg/testutils/skip"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
	"github.com/cockroachdb/errors"
	"github.com/stretchr/testify/assert"
)

func Example_node() {
	c := NewCLITest(TestCLIParams{})
	defer c.Cleanup()

	// Refresh time series data, which is required to retrieve stats.
	if err := c.Server.WriteSummaries(); err != nil {
		log.Fatalf(context.Background(), "Couldn't write stats summaries: %s", err)
	}

	c.Run("node ls")
	c.Run("node ls --format=table")
	c.Run("node status 10000")
	c.RunWithArgs([]string{"sql", "-e", "drop database defaultdb"})
	c.Run("node ls")

	// Output:
	// node ls
	// id
	// 1
	// node ls --format=table
	//   id
	// ------
	//    1
	// (1 row)
	// node status 10000
	// ERROR: node 10000 doesn't exist
	// sql -e drop database defaultdb
	// DROP DATABASE
	// node ls
	// id
	// 1
}

func TestNodeStatus(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	// This test is prone to timing out when run using remote execution under the
	// {deadlock,race} detector.
	skip.UnderDeadlock(t)
	skip.UnderRace(t)

	start := timeutil.Now()
	c := NewCLITest(TestCLIParams{})
	defer c.Cleanup()

	// Refresh time series data, which is required to retrieve stats.
	if err := c.Server.WriteSummaries(); err != nil {
		t.Fatalf("couldn't write stats summaries: %s", err)
	}

	out, err := c.RunWithCapture("node status 1 --format=table")
	if err != nil {
		t.Fatal(err)
	}
	checkNodeStatus(t, c, out, start)

	out, err = c.RunWithCapture("node status --ranges --format=table")
	if err != nil {
		t.Fatal(err)
	}
	checkNodeStatus(t, c, out, start)

	out, err = c.RunWithCapture("node status --stats --format=table")
	if err != nil {
		t.Fatal(err)
	}
	checkNodeStatus(t, c, out, start)

	out, err = c.RunWithCapture("node status --ranges --stats --format=table")
	if err != nil {
		t.Fatal(err)
	}
	checkNodeStatus(t, c, out, start)

	out, err = c.RunWithCapture("node status --decommission --format=table")
	if err != nil {
		t.Fatal(err)
	}
	checkNodeStatus(t, c, out, start)

	out, err = c.RunWithCapture("node status --ranges --stats --decommission --format=table")
	if err != nil {
		t.Fatal(err)
	}
	checkNodeStatus(t, c, out, start)

	out, err = c.RunWithCapture("node status --all --format=table")
	if err != nil {
		t.Fatal(err)
	}
	checkNodeStatus(t, c, out, start)

	out, err = c.RunWithCapture("node status --format=table")
	if err != nil {
		t.Fatal(err)
	}
	checkNodeStatus(t, c, out, start)
}

func checkNodeStatus(t *testing.T, c TestCLI, output string, start time.Time) {
	buf := bytes.NewBufferString(output)
	s := bufio.NewScanner(buf)

	type testCase struct {
		name string
		idx  int
	}

	// Skip command line.
	if !s.Scan() {
		t.Fatalf("Couldn't skip command line: %s", s.Err())
	}

	// check column names.
	if !s.Scan() {
		t.Fatalf("Error reading column names: %s", s.Err())
	}
	cols, err := extractFields(s.Text())
	if err != nil {
		t.Fatalf("%s", err)
	}
	if !reflect.DeepEqual(cols, getStatusNodeHeaders()) {
		t.Fatalf("columns (%s) don't match expected (%s)", cols, getStatusNodeHeaders())
	}

	checkSeparatorLine(t, s)

	// Check node status.
	if !s.Scan() {
		t.Fatalf("error reading node status: %s", s.Err())
	}
	fields, err := extractFields(s.Text())
	if err != nil {
		t.Fatalf("%s", err)
	}

	nodeID := c.Server.NodeID()
	nodeIDStr := strconv.FormatInt(int64(nodeID), 10)
	if a, e := fields[0], nodeIDStr; a != e {
		t.Errorf("node id (%s) != expected (%s)", a, e)
	}

	nodeAddr := c.Server.AdvRPCAddr()
	if a, e := fields[1], nodeAddr; a != e {
		t.Errorf("node address (%s) != expected (%s)", a, e)
	}

	nodeSQLAddr := c.Server.AdvSQLAddr()
	if a, e := fields[2], nodeSQLAddr; a != e {
		t.Errorf("node SQL address (%s) != expected (%s)", a, e)
	}

	// Verify Build Tag.
	if a, e := fields[3], build.GetInfo().Tag; a != e {
		t.Errorf("build tag (%s) != expected (%s)", a, e)
	}

	// Verify that updated_at and started_at are reasonably recent.
	// CircleCI can be very slow. This was flaky at 5s.
	checkTimeElapsed(t, fields[4], 15*time.Second, start)
	checkTimeElapsed(t, fields[5], 15*time.Second, start)

	testcases := []testCase{}

	// We're skipping over the first 5 default fields such as node id and
	// address. They don't need closer checks.
	baseIdx := len(baseNodeColumnHeaders)

	// Adding fields that need verification for --ranges flag.
	// We have to allow up to 1 unavailable/underreplicated range because
	// sometimes we run the `node status` command before the server has fully
	// initialized itself and it doesn't consider itself live yet. In such cases,
	// there will only be one range covering the entire keyspace because it won't
	// have been able to do any splits yet.
	if nodeCtx.statusShowRanges || nodeCtx.statusShowAll {
		testcases = append(testcases,
			testCase{"leader_ranges", baseIdx},
			testCase{"leaseholder_ranges", baseIdx + 1},
			testCase{"ranges", baseIdx + 2},
			testCase{"unavailable_ranges", baseIdx + 3},
			testCase{"underreplicated_ranges", baseIdx + 4},
		)
		baseIdx += len(statusNodesColumnHeadersForRanges)
	}

	// Adding fields that need verification for --stats flag.
	if nodeCtx.statusShowStats || nodeCtx.statusShowAll {
		testcases = append(testcases,
			testCase{"live_bytes", baseIdx},
			testCase{"key_bytes", baseIdx + 1},
			testCase{"value_bytes", baseIdx + 2},
			testCase{"intent_bytes", baseIdx + 3},
			testCase{"system_bytes", baseIdx + 4},
		)
		baseIdx += len(statusNodesColumnHeadersForStats)
	}

	if nodeCtx.statusShowDecommission || nodeCtx.statusShowAll {
		testcases = append(testcases,
			testCase{"gossiped_replicas", baseIdx},
		)
		baseIdx++
	}

	for _, tc := range testcases {
		val, err := strconv.ParseInt(fields[tc.idx], 10, 64)
		if err != nil {
			t.Errorf("couldn't parse %s '%s': %v", tc.name, fields[tc.idx], err)
			continue
		}
		if val < 0 {
			t.Errorf("value for %s (%d) cannot be less than 0", tc.name, val)
			continue
		}
	}

	if nodeCtx.statusShowDecommission || nodeCtx.statusShowAll {
		var found int
		for i, name := range getStatusNodeHeaders() {
			switch name {
			case "is_decommissioning", "is_draining":
				found++
				assert.Equal(t, "false", fields[i], name)
			default:
			}
		}
		assert.Equal(t, 2, found, "missing column in node status output")
	}
}

var separatorLineExp = regexp.MustCompile(`[\+-]+$`)

func checkSeparatorLine(t *testing.T, s *bufio.Scanner) {
	if !s.Scan() {
		t.Fatalf("error reading separator line: %s", s.Err())
	}
	if !separatorLineExp.MatchString(s.Text()) {
		t.Fatalf("separator line not found: %s", s.Text())
	}
}

// checkRecentTime produces a test error if the time is not within the specified number
// of seconds of the given start time.
func checkTimeElapsed(t *testing.T, timeStr string, elapsed time.Duration, start time.Time) {
	// Truncate start time, because the CLI currently outputs times with a second-level
	// granularity.
	start = start.Truncate(time.Second)
	tm, err := time.ParseInLocation("2006-01-02 15:04:05.99999 -0700 MST", timeStr, start.Location())
	if err != nil {
		t.Errorf("couldn't parse time '%s': %s", timeStr, err)
		return
	}
	end := start.Add(elapsed)
	if tm.Before(start) || tm.After(end) {
		t.Errorf("time (%s) not within range [%s,%s]", tm, start, end)
	}
}

// extractFields extracts the fields from a pretty-printed row of SQL output,
// discarding excess whitespace and column separators.
func extractFields(line string) ([]string, error) {
	fields := strings.Split(line, "|")
	// fields has two extra entries, one for the empty token to the left of the first
	// |, and another empty one to the right of the final |. So, we need to take those
	// out.
	if a, e := len(fields), len(getStatusNodeHeaders()); a != e {
		return nil, errors.Errorf("can't extract fields: # of fields (%d) != expected (%d)", a, e)
	}
	var r []string
	for _, f := range fields {
		r = append(r, strings.TrimSpace(f))
	}
	return r, nil
}
