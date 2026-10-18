package upstream

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

var errUnused = errors.New("unused")

func TestChatHTTPForAccountUsesPerSlotClient(t *testing.T) {
	t.Parallel()

	client := New()
	dial := func(context.Context, string, string) (net.Conn, error) { return nil, errUnused }
	client.AccountRoute = func(a *auth.Auth) (string, func(context.Context, string, string) (net.Conn, error)) {
		return a.UID, dial
	}
	client2 := client.chatHTTPForAccount(&auth.Auth{UID: "slot-a"}, 0)

	a2 := client.chatHTTPForAccount(&auth.Auth{UID: "slot-b"}, 0)
	if client2 == a2 {
		t.Fatal("different slots must not share a client")
	}

	again := client.chatHTTPForAccount(&auth.Auth{UID: "slot-a"}, 0)
	if again != client2 {
		t.Fatal("same slot should reuse cached client")
	}
}
