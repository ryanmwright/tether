package daemon

import (
	"os"
	"testing"

	"github.com/ryanmwright/tether/internal/kubetest"
)

func TestMain(m *testing.M) {
	kubetest.RunHelper() // when run as the fake kubectl's port-forward
	os.Exit(m.Run())
}
