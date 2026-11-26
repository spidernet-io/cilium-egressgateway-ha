//go:build e2e

package switchover_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/spidernet-io/cilium-egressgateway-ha/test/e2e/common"
)

var c client.Client

func TestSwitchover(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Switchover Suite")
}

var _ = BeforeSuite(func() {
	c = common.NewClient()
})
