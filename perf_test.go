package router

import (
	"github.com/cloudfoundry/go_cfmessagebus/mock_cfmessagebus"
	"strconv"
	"testing"

	"github.com/cloudfoundry/gorouter/config"
	"github.com/cloudfoundry/gorouter/registry"
	"github.com/cloudfoundry/gorouter/route"
)

const (
	Host = "1.2.3.4"
	Port = 1234
)

func BenchmarkRegister(b *testing.B) {
	c := config.DefaultConfig()
	mbus := mock_cfmessagebus.NewMockMessageBus()
	r := registry.NewRegistry(c, mbus)
	p := NewProxy(c, r, NewVarz(r))

	for i := 0; i < b.N; i++ {
		str := strconv.Itoa(i)
		p.Register(&route.Endpoint{
			Host: "localhost",
			Port: uint16(i),
			Uris: []route.Uri{route.Uri("bench.vcap.me." + str)},
		})
	}
}
