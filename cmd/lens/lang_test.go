package main

import "testing"

func TestLangOfRecognisesDartPaths(t *testing.T) {
	cases := map[string]string{
		"lib/cart.dart:Cart.total": "dart",
		"lib/cart.dart":            "dart",
		"internal/a.go:Handle":     "go",
		"src/Handler.cs:Handle":    "",
	}
	for path, want := range cases {
		if got := langOf(path); got != want {
			t.Errorf("langOf(%q) = %q, want %q", path, got, want)
		}
	}
}
