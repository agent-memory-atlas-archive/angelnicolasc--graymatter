// testtiming renders go test -json output and summarizes top-level test times.
// The caller must use pipefail: a valid JSON stream can describe failing tests.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

type event struct {
	Action  string
	Package string
	Test    string
	Elapsed float64
	Output  string
}

type timing struct {
	name    string
	status  string
	elapsed float64
}

type checkedWriter struct {
	io.Writer
	err error
}

func (w *checkedWriter) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	n, err := w.Writer.Write(p)
	w.err = err
	return n, err
}

func report(input io.Reader, output, summary io.Writer) (result error) {
	readable, markdown := &checkedWriter{Writer: output}, &checkedWriter{Writer: summary}
	output, summary = readable, markdown
	defer func() { result = errors.Join(result, readable.err, markdown.err) }()
	tests := make(map[string]timing)
	packages := make(map[string]string)
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	var streamErr error
	for scanner.Scan() {
		var e event
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil || e.Action == "" {
			// Keep compiler diagnostics visible even if the stream is malformed.
			fmt.Fprintln(output, scanner.Text())
			streamErr = fmt.Errorf("invalid go test JSON event")
			continue
		}
		fmt.Fprint(output, e.Output)
		if e.Test == "" {
			switch e.Action {
			case "start", "pass", "fail", "skip":
				packages[e.Package] = e.Action
			}
			continue
		}
		// Parent elapsed already contains subtests; do not double-count them.
		if strings.Contains(e.Test, "/") {
			continue
		}
		key := e.Package + "/" + e.Test
		switch e.Action {
		case "run", "pass", "fail", "skip":
			tests[key] = timing{name: key, status: e.Action, elapsed: e.Elapsed}
		}
	}
	if err := scanner.Err(); err != nil {
		streamErr = fmt.Errorf("read go test JSON: %w", err)
	}
	rows := make([]timing, 0, len(tests))
	for _, test := range tests {
		rows = append(rows, test)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].elapsed == rows[j].elapsed {
			return rows[i].name < rows[j].name
		}
		return rows[i].elapsed > rows[j].elapsed
	})
	fmt.Fprintln(summary, "### CLI test timings\n\nTop-level tests only; elapsed times include their subtests.")
	fmt.Fprintln(summary, "\n| Test | Seconds | Result |\n| --- | ---: | --- |")
	for i, test := range rows {
		if i < 15 || test.status == "run" || test.status == "fail" {
			elapsed, status := fmt.Sprintf("%.3f", test.elapsed), test.status
			if status == "run" {
				elapsed, status = "unknown", "incomplete"
			}
			fmt.Fprintf(summary, "| `%s` | %s | %s |\n", test.name, elapsed, status)
		}
	}
	if streamErr != nil {
		fmt.Fprintln(summary, "\nThe JSON stream was malformed or could not be read completely.")
		return streamErr
	}
	if len(packages) == 0 {
		return fmt.Errorf("no package result in go test JSON")
	}
	for name, status := range packages {
		if status != "pass" && status != "skip" {
			return fmt.Errorf("package %s ended with %s", name, status)
		}
	}
	for _, test := range tests {
		if test.status == "run" || test.status == "fail" {
			return fmt.Errorf("test %s ended with %s", test.name, test.status)
		}
	}
	return nil
}

func main() {
	summaryPath := flag.String("summary", "", "append Markdown timings to this file")
	flag.Parse()
	summary := io.Writer(os.Stdout)
	if *summaryPath != "" {
		file, err := os.OpenFile(*summaryPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		defer file.Close()
		summary = file
	}
	if err := report(os.Stdin, os.Stdout, summary); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
