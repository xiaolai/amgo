// Command notice-text-check verifies that THIRD-PARTY-NOTICES.md bundles the
// full text of each used module's license, not just a table row. It reads a
// manifest of "module<TAB>version<TAB>module directory" lines from stdin.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type module struct {
	path, version, dir string
}

func main() {
	noticesPath := flag.String("notices", "THIRD-PARTY-NOTICES.md", "notices file")
	flag.Parse()

	notices, err := os.ReadFile(*noticesPath)
	if err != nil {
		fail(err)
	}
	sections := parseSections(string(notices))

	manifest := bufio.NewScanner(os.Stdin)
	failures := 0
	checked := 0
	for manifest.Scan() {
		fields := strings.Split(manifest.Text(), "\t")
		if len(fields) != 3 || fields[0] == "" {
			continue
		}
		m := module{path: fields[0], version: fields[1], dir: fields[2]}
		checked++
		if err := verify(m, sections); err != nil {
			fmt.Fprintln(os.Stderr, "license-check:", err)
			failures++
		}
	}
	if err := manifest.Err(); err != nil {
		fail(err)
	}
	if checked == 0 {
		fail(fmt.Errorf("no modules were checked"))
	}
	if failures > 0 {
		fail(fmt.Errorf("%d module license text(s) are incomplete", failures))
	}
	fmt.Printf("notice-text-check: PASS (%d modules)\n", checked)
}

func parseSections(text string) map[string]string {
	out := map[string]string{}
	lines := strings.Split(text, "\n")
	for i := 0; i < len(lines); i++ {
		if !strings.HasPrefix(lines[i], "### ") {
			continue
		}
		header := strings.TrimPrefix(lines[i], "### ")
		body := strings.Builder{}
		j := i + 1
		for ; j < len(lines); j++ {
			if strings.HasPrefix(lines[j], "### ") || strings.HasPrefix(lines[j], "## ") {
				break
			}
			body.WriteString(lines[j])
			body.WriteString("\n")
		}
		// Header is "path version — license"; the first two fields identify
		// the section. Keep every header body in case of duplicate paths.
		parts := strings.Fields(header)
		if len(parts) >= 2 {
			key := parts[0] + "\x00" + parts[1]
			out[key] = body.String()
		}
		i = j - 1
	}
	return out
}

func verify(m module, sections map[string]string) error {
	section, ok := sections[m.path+"\x00"+m.version]
	if !ok {
		return fmt.Errorf("%s %s has no license section", m.path, m.version)
	}
	bundled := fencedText(section)
	if bundled == "" {
		return fmt.Errorf("%s %s has no fenced license text", m.path, m.version)
	}
	licensePath, err := licenseFile(m.dir)
	if err != nil {
		return fmt.Errorf("%s %s: %w", m.path, m.version, err)
	}
	raw, err := os.ReadFile(licensePath)
	if err != nil {
		return fmt.Errorf("%s %s: %w", m.path, m.version, err)
	}
	want := normalize(string(raw))
	got := normalize(bundled)
	if want == "" {
		return fmt.Errorf("%s %s: license file %s is empty", m.path, m.version, licensePath)
	}
	if !strings.Contains(got, want) {
		return fmt.Errorf("%s %s: bundled text does not contain the full text of %s",
			m.path, m.version, filepath.Base(licensePath))
	}
	return nil
}

func fencedText(section string) string {
	lines := strings.Split(section, "\n")
	var body strings.Builder
	inFence := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !inFence && strings.HasPrefix(trimmed, "```") {
			inFence = true
			continue
		}
		if inFence && trimmed == "```" {
			return body.String()
		}
		if inFence {
			body.WriteString(line)
			body.WriteString("\n")
		}
	}
	return ""
}

func licenseFile(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, "LICENSE") || strings.HasPrefix(name, "COPYING") || strings.HasPrefix(name, "NOTICE") {
			return filepath.Join(dir, name), nil
		}
	}
	return "", fmt.Errorf("no license file in %s", dir)
}

func normalize(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "license-check:", err)
	os.Exit(1)
}
