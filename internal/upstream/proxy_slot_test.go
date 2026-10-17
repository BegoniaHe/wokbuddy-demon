package upstream

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

func TestChatHTTPForAccountUsesPerSlotClient(t *testing.T) {
	c := New()
	dial := func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("unused") }
	c.AccountRoute = func(a *auth.Auth) (string, func(context.Context, string, string) (net.Conn, error)) {
		return a.UID, dial
	}
	a1 := c.chatHTTPForAccount(&auth.Auth{UID: "slot-a"}, 0)
	a2 := c.chatHTTPForAccount(&auth.Auth{UID: "slot-b"}, 0)
	if a1 == a2 {
		t.Fatal("different slots must not share a client")
	}
	again := c.chatHTTPForAccount(&auth.Auth{UID: "slot-a"}, 0)
	if again != a1 {
		t.Fatal("same slot should reuse cached client")
	}
}
