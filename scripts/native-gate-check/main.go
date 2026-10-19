// Command native-gate-check qualifies a `go test -json` native run. It
// requires an explicit PASS for every test named in the required list and
// rejects skips. A skipped provider or fault gate is not a release PASS.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
)

type event struct {
	Action string `json:"Action"`
	Test   string `json:"Test"`
}

func main() {
	requiredPath := flag.String("required", "", "file with one required test name per line")
	jsonPath := flag.String("json", "", "go test -json output")
	flag.Parse()
	if *requiredPath == "" || *jsonPath == "" {
		fmt.Fprintln(os.Stderr, "native-gate-check: -required and -json are mandatory")
		os.Exit(2)
	}
	required, err := readRequired(*requiredPath)
	if err != nil {
		fail(err)
	}
	results, err := readResults(*jsonPath)
	if err != nil {
		fail(err)
	}
	missing, skipped, failed := evaluate(required, results)
	if len(missing)+len(skipped)+len(failed) == 0 {
		fmt.Printf("native-gate-check: PASS (%d/%d)\n", len(required), len(required))
		return
	}
	if len(missing) > 0 {
		fmt.Fprintf(os.Stderr, "native-gate-check: required tests did not report a result: %s\n", strings.Join(missing, ", "))
	}
	if len(skipped) > 0 {
		fmt.Fprintf(os.Stderr, "native-gate-check: required tests skipped: %s\n", strings.Join(skipped, ", "))
	}
	if len(failed) > 0 {
		fmt.Fprintf(os.Stderr, "native-gate-check: required tests failed: %s\n", strings.Join(failed, ", "))
	}
	os.Exit(1)
}

func evaluate(required []string, results map[string]string) (missing, skipped, failed []string) {
	for _, name := range required {
		switch results[name] {
		case "pass":
		case "skip":
			skipped = append(skipped, name)
		case "fail":
			failed = append(failed, name)
		default:
			missing = append(missing, name)
		}
	}
	return missing, skipped, failed
}

func readRequired(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []string
	seen := map[string]bool{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		name := strings.TrimSpace(sc.Text())
		if name == "" || seen[name] || !strings.HasPrefix(name, "Test") {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no required tests were listed")
	}
	sort.Strings(out)
	return out, nil
}

func readResults(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
	for sc.Scan() {
		var ev event
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			// Build diagnostics can appear on the stream; they are not test
			// results, but malformed JSON from go test would hide a real one.
			continue
		}
		if ev.Test == "" || strings.Contains(ev.Test, "/") {
			continue
		}
		switch ev.Action {
		case "pass", "fail", "skip":
			out[ev.Test] = ev.Action
		}
	}
	return out, sc.Err()
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "native-gate-check:", err)
	os.Exit(1)
}
