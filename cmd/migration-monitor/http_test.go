package main

import (
	"regexp"
	"testing"
)

// The page dies whole if a module it imports is missing: the embed directive
// lists files one by one, so adding a module to the page and forgetting the
// directive serves a 404 and a blank page. Dynamic imports are exempt -
// synthetic.js is review scaffolding, deliberately not shipped.
func TestEveryStaticImportIsEmbedded(t *testing.T) {
	page, err := uiFS.ReadFile("ui/index.html")
	if err != nil {
		t.Fatal(err)
	}
	imports := regexp.MustCompile(`(?m)^\s*import\s+[^;]*?from\s+'\./([\w.-]+)'`).FindAllStringSubmatch(string(page), -1)
	if len(imports) == 0 {
		t.Fatal("no static imports found: the pattern stopped matching the page")
	}
	for _, m := range imports {
		if _, err := uiFS.ReadFile("ui/" + m[1]); err != nil {
			t.Errorf("%s is imported by the page but not embedded: %v", m[1], err)
		}
	}
}
