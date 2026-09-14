package main

import (
	"bytes"
	"testing"
)

func TestPrintVersion(t *testing.T) {
	var out bytes.Buffer
	if err := printVersion(&out); err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), "foliot "+version+"\n"; got != want {
		t.Fatalf("printVersion wrote %q, want %q", got, want)
	}
}
