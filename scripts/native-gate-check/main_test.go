package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestEvaluateRejectsSkippedRequiredGate(t *testing.T) {
	required := []string{"TestA", "TestB", "TestC"}
	results := map[string]string{"TestA": "pass", "TestC": "pass"}
	missing, skipped, failed := evaluate(required, results)
	if len(missing) != 1 || missing[0] != "TestB" || len(skipped) != 0 || len(failed) != 0 {
		t.Fatalf("missing=%v skipped=%v failed=%v", missing, skipped, failed)
	}
	results["TestB"] = "skip"
	missing, skipped, failed = evaluate(required, results)
	if len(missing) != 0 || len(skipped) != 1 || skipped[0] != "TestB" || len(failed) != 0 {
		t.Fatalf("missing=%v skipped=%v failed=%v", missing, skipped, failed)
	}
}

func TestEvaluateAcceptsFullPass(t *testing.T) {
	required := []string{"TestA", "TestB"}
	results := map[string]string{"TestA": "pass", "TestB": "pass"}
	missing, skipped, failed := evaluate(required, results)
	if len(missing)+len(skipped)+len(failed) != 0 {
		t.Fatalf("missing=%v skipped=%v failed=%v", missing, skipped, failed)
	}
}

func TestReadResultsIgnoresSubtestsAndRecordsSkips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "native.json")
	raw := `{"Action":"pass","Test":"TestA"}
{"Action":"skip","Test":"TestB"}
{"Action":"fail","Test":"TestC/sub"}
{"Action":"pass","Package":"amgo/tests/native"}
`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readResults(path)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"TestA": "pass", "TestB": "skip"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("results=%v want=%v", got, want)
	}
}

func TestReadRequiredRejectsEmptyList(t *testing.T) {
	path := filepath.Join(t.TempDir(), "required")
	if err := os.WriteFile(path, []byte("not-a-test\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readRequired(path); err == nil {
		t.Fatal("an empty required list must fail")
	}
}
