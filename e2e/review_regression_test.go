package e2e

import (
	"context"
	"github.com/shengyu/edgelink/internal/model"
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"
)

func TestReviewRemotePublishMustNotReplaceLocalListeners(t *testing.T) {
	h := newHarness(t)
	var c model.Customer
	h.mustJSON("POST", "/api/customers", map[string]any{"name": "review"}, true, &c)
	port := freePort(t)
	origin := startTCPEcho(t, "OK")
	h.mustJSON("POST", "/api/businesses", map[string]any{"customer_id": c.ID, "name": "local", "mode": "tcp_port", "primary_node_id": h.nodeID, "origin_host": "127.0.0.1", "origin_port": origin, "entry_addr": "127.0.0.1", "entry_port": port}, true, nil)
	h.mustJSON("POST", "/api/nodes/"+h.nodeID+"/publish", map[string]any{}, true, nil)
	caps, _ := h.Relay.Capabilities(context.Background())
	if err := h.Store.CreateNode(&model.Node{ID: "node-remote-review", Name: "remote", Enabled: true, Capabilities: caps}); err != nil {
		t.Fatal(err)
	}
	resp := h.do("POST", "/api/nodes/node-remote-review/publish", map[string]any{}, true)
	defer resp.Body.Close()
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), time.Second)
	if err == nil {
		conn.Close()
	}
	if resp.StatusCode == http.StatusOK && err != nil {
		t.Fatalf("remote publish returned 200 and closed existing LOCAL listener: %v", err)
	}
}
