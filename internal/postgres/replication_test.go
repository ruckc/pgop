/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package postgres

import (
	"net"
	"testing"
	"time"
)

// TestDialAddressConnectsToFixedAddress checks that a DialAddress
// connection is made to that address, not to Host (which stays the name the
// server certificate is verified for).
func TestDialAddressConnectsToFixedAddress(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	accepted := make(chan struct{}, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		accepted <- struct{}{}
		_ = conn.Close() // not a PostgreSQL server: the client fails
	}()

	_, err = NewClient(ConnectionConfig{
		Host:        "primary.invalid",
		Port:        5432,
		User:        "u",
		Password:    "p",
		DialAddress: ln.Addr().String(),
	})
	if err == nil {
		t.Fatal("expected the connection to fail against a non-PostgreSQL listener")
	}
	select {
	case <-accepted:
	case <-time.After(5 * time.Second):
		t.Fatalf("no connection to %s; error: %v", ln.Addr(), err)
	}
}
