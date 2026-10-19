// Package benchmark measures the executable's process startup separately from
// provider or model latency.
package benchmark

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

var (
	binPath  string
	buildErr error
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "amgo-bench-")
	if err != nil {
		buildErr = err
		os.Exit(1)
	}
	binPath = filepath.Join(dir, "amgo")
	cmd := exec.Command("go", "build", "-trimpath", "-o", binPath, "amgo/cmd/amgo")
	if out, err := cmd.CombinedOutput(); err != nil {
		buildErr = &buildError{msg: string(out), err: err}
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

type buildError struct {
	msg string
	err error
}

func (e *buildError) Error() string { return e.err.Error() + ": " + e.msg }

func binary(tb testing.TB) string {
	tb.Helper()
	if buildErr != nil {
		tb.Fatal(buildErr)
	}
	return binPath
}

func BenchmarkVersion(b *testing.B) {
	bin := binary(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := exec.Command(bin, "version").Run(); err != nil {
			b.Fatal(err)
		}
	}
}

func TestStartupDistribution(t *testing.T) {
	bin := binary(t)
	const n = 50
	samples := make([]time.Duration, 0, n)
	for i := 0; i < n; i++ {
		start := time.Now()
		if err := exec.Command(bin, "version").Run(); err != nil {
			t.Fatal(err)
		}
		samples = append(samples, time.Since(start))
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	p50 := samples[len(samples)/2]
	p95 := samples[int(float64(len(samples)-1)*0.95)]
	t.Logf("version startup: p50=%s p95=%s max=%s", p50, p95, samples[len(samples)-1])
}
