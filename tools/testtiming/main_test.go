package main

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestReportSurfacesOutputErrors(t *testing.T) {
	const input = `{"Action":"output","Package":"p","Output":"output"}
{"Action":"pass","Package":"p"}`
	for _, writers := range [][2]io.Writer{{failingWriter{}, io.Discard}, {io.Discard, failingWriter{}}} {
		if err := report(strings.NewReader(input), writers[0], writers[1]); !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("lost output error: %v", err)
		}
	}
}

func TestReportReadableOutputAndTopLevelTimes(t *testing.T) {
	input := `{"Action":"start","Package":"p"}
{"Action":"run","Package":"p","Test":"TestParent"}
{"Action":"output","Package":"p","Test":"TestParent","Output":"readable output\n"}
{"Action":"pass","Package":"p","Test":"TestParent/child","Elapsed":1}
{"Action":"pass","Package":"p","Test":"TestParent","Elapsed":3}
{"Action":"pass","Package":"p","Test":"TestOther","Elapsed":8}
{"Action":"pass","Package":"p","Elapsed":11}
`
	var output, summary bytes.Buffer
	if err := report(strings.NewReader(input), &output, &summary); err != nil {
		t.Fatal(err)
	}
	if output.String() != "readable output\n" {
		t.Fatalf("output=%q", output.String())
	}
	text := summary.String()
	if strings.Contains(text, "child") || strings.Index(text, "TestOther") > strings.Index(text, "TestParent") ||
		!strings.Contains(text, "3.000 | pass") || !strings.Contains(text, "8.000 | pass") {
		t.Fatalf("wrong ordering or double-counted subtest: %s", text)
	}
}

func TestReportRejectsFailureAndIncompleteStreams(t *testing.T) {
	for name, input := range map[string]string{
		"failed test": `{"Action":"fail","Package":"p","Test":"TestFailed","Elapsed":2}
{"Action":"fail","Package":"p","Elapsed":2}`,
		"package timeout": `{"Action":"start","Package":"p"}
{"Action":"run","Package":"p","Test":"TestStillRunning"}
{"Action":"output","Package":"p","Output":"panic: test timed out\n"}
{"Action":"fail","Package":"p","Elapsed":300}`,
		"truncated": `{"Action":"start","Package":"p"}
{"Action":"run","Package":"p","Test":"TestStillRunning"}`,
		"malformed": "compiler error\n{\"Action\":\"pass\",\"Package\":\"p\"}\n",
		"empty":     "",
	} {
		t.Run(name, func(t *testing.T) {
			var output, summary bytes.Buffer
			if err := report(strings.NewReader(input), &output, &summary); err == nil {
				t.Fatal("unsuccessful stream reported success")
			}
			if strings.Contains(input, "TestStillRunning") && !strings.Contains(summary.String(), "unknown | incomplete") {
				t.Fatalf("missing incomplete test: %s", summary.String())
			}
			if name == "malformed" && !strings.Contains(output.String(), "compiler error") {
				t.Fatal("dropped raw diagnostic")
			}
		})
	}
}
