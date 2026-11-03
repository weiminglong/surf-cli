package cli

import (
	"os"
	"testing"

	"github.com/logrusorgru/aurora"
)

func TestMain(m *testing.M) {
	// au is the zero value until Init runs, which panics for any test that
	// logs before another test happens to initialize it. Big test files hid
	// this by ordering; running a single test exposed it.
	au = aurora.NewAurora(false)
	os.Exit(m.Run())
}
