// Copyright 2015 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package log

import (
	"context"
	"fmt"
	"strings"
)

// FormatWithContextTags formats the string and prepends the context
// tags.
//
// Redaction markers are *not* inserted. The resulting
// string is generally unsafe for reporting.
func FormatWithContextTags(ctx context.Context, format string, args ...interface{}) string {
	var buf strings.Builder
	formatTags(ctx, true /* brackets */, &buf)
	formatArgs(&buf, format, args...)
	return buf.String()
}

func formatOnlyArgs(format string, args ...interface{}) string {
	var buf strings.Builder
	formatArgs(&buf, format, args...)
	return buf.String()
}

func formatArgs(buf *strings.Builder, format string, args ...interface{}) {
	if len(args) == 0 {
		buf.WriteString(format)
	} else if len(format) == 0 {
		fmt.Fprint(buf, args...)
	} else {
		fmt.Fprintf(buf, format, args...)
	}
}
