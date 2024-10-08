// Copyright 2019 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package main

import (
	"io"
	"strings"
	"text/template"
)

type rowNumberTmplInfo struct {
	HasPartition bool
	String       string
}

const rowNumberTmpl = "pkg/sql/colexec/colexecwindow/row_number_tmpl.go"

func genRowNumberOp(inputFileContents string, wr io.Writer) error {
	s := strings.ReplaceAll(inputFileContents, "_ROW_NUMBER_STRING", "{{.String}}")

	computeRowNumberRe := makeFunctionRegex("_COMPUTE_ROW_NUMBER", 1)
	s = computeRowNumberRe.ReplaceAllString(s, `{{template "computeRowNumber" buildDict "HasPartition" .HasPartition "HasSel" $1}}`)

	// Now, generate the op, from the template.
	tmpl, err := template.New("row_number_op").Funcs(template.FuncMap{"buildDict": buildDict}).Parse(s)
	if err != nil {
		return err
	}

	rowNumberTmplInfos := []rowNumberTmplInfo{
		{HasPartition: false, String: "rowNumberNoPartition"},
		{HasPartition: true, String: "rowNumberWithPartition"},
	}
	return tmpl.Execute(wr, rowNumberTmplInfos)
}

func init() {
	registerGenerator(genRowNumberOp, "row_number.eg.go", rowNumberTmpl)
}
