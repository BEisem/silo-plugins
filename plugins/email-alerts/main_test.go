package main

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	claimSettle = 0 // tests share one process, so there is no rival to wait for
	os.Exit(m.Run())
}
