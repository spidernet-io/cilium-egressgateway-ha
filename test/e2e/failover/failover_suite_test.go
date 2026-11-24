//go:build e2e

package failover_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/spidernet-io/cilium-egressgateway-ha/test/e2e/common"
)

var c client.Client

func TestFailover(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Failover Suite")
}

var _ = BeforeSuite(func() {
	c = common.NewClient()
})
