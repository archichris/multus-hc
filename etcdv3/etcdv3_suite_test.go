package etcdv3
import (
	"testing"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

func TestEtcdv3(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Etcdv3 Suite")
}

