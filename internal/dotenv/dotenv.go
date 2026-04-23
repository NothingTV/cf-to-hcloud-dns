// Package dotenv is a tiny KEY=VALUE loader. It intentionally does not try to
// be compatible with every .env dialect: no variable expansion, no multi-line
// values, no export prefix. If a key is already set in the real environment,
// the file value is ignored.
package dotenv

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

// Load reads path and sets any KEY=VALUE pairs into the process environment,
// skipping keys that are already set. A missing file is not an error when
// required is false.
func Load(path string, required bool) error {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) && !required {
			return nil
		}
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			return fmt.Errorf("%s:%d: expected KEY=VALUE", path, lineNo)
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		val = unquote(val)
		if _, already := os.LookupEnv(key); already {
			continue
		}
		if err := os.Setenv(key, val); err != nil {
			return err
		}
	}
	return sc.Err()
}

func unquote(s string) string {
	if len(s) >= 2 {
		first, last := s[0], s[len(s)-1]
		if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}
