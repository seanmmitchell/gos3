// filter_go_coverage removes only explicitly listed generated files from a
// Go coverage profile. Run it from the repository root after go test.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"
)

func main() {
	if err := filter(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func filter(args []string) error {
	if len(args) != 2 {
		return errors.New("usage: filter_go_coverage source destination")
	}
	data, err := os.ReadFile("coverage.generated.json")
	if err != nil {
		return err
	}
	var listed struct {
		Go []string `json:"go"`
	}
	if err := json.Unmarshal(data, &listed); err != nil {
		return err
	}
	excluded := make(map[string]bool, len(listed.Go))
	for _, item := range listed.Go {
		clean := path.Clean(item)
		if clean != item || strings.HasPrefix(item, "/") || strings.Contains(item, "\\") ||
			!(strings.HasPrefix(item, "cmd/") || strings.HasPrefix(item, "internal/")) ||
			!strings.HasSuffix(item, ".go") {
			return fmt.Errorf("invalid generated Go coverage path %q", item)
		}
		if info, err := os.Stat(item); err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("missing generated Go file %q", item)
		}
		excluded[item] = true
	}
	input, err := os.Open(args[0])
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.Create(args[1])
	if err != nil {
		return err
	}
	defer output.Close()
	scanner := bufio.NewScanner(input)
	if !scanner.Scan() || !strings.HasPrefix(scanner.Text(), "mode: ") {
		return errors.New("invalid Go coverage profile")
	}
	if _, err := fmt.Fprintln(output, scanner.Text()); err != nil {
		return err
	}
	for scanner.Scan() {
		line := scanner.Text()
		filename, _, _ := strings.Cut(line, ":")
		omit := false
		for item := range excluded {
			if strings.HasSuffix(filename, "/"+item) {
				omit = true
				break
			}
		}
		if !omit {
			if _, err := fmt.Fprintln(output, line); err != nil {
				return err
			}
		}
	}
	return scanner.Err()
}
