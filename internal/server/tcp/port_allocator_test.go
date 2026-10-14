package tcp

import (
	"fmt"
	"net"
	"testing"
)

func TestPortAllocatorHoldsSocketUntilProxyTakesOwnership(t *testing.T) {
	p, err := NewPortAllocator(20000, 40000)
	if err != nil {
		t.Fatal(err)
	}
	port, err := p.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	defer p.Release(port)
	if other, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", port)); err == nil {
		other.Close()
		t.Fatal("allocated port could be stolen before proxy startup")
	}
	ln := p.TakeListener(port)
	if ln == nil {
		t.Fatal("reserved listener was lost")
	}
	defer ln.Close()
	if _, err := p.AllocateSpecific(port); err == nil {
		t.Fatal("taking listener released the reservation")
	}
	_ = ln.Close()
	p.Release(port)
	if _, err := p.AllocateSpecific(port); err != nil {
		t.Fatalf("released port could not be reused: %v", err)
	}
}
