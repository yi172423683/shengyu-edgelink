package agent

import (
	"context"
	"fmt"
	"github.com/shengyu/edgelink/internal/logstore"
	"testing"
	"time"
)

func TestReviewRestartMustNotLoseLogs(t *testing.T) {
	s, err := logstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	line := `{"node":"node-review","ver":1,"ci":"192.0.2.1","tsc":"--"}`
	NewIngestor(s, nil, "node-review").IngestLine(line)
	NewIngestor(s, nil, "node-review").IngestLine(line)
	rows, _, err := s.QueryConn(context.Background(), logstore.Query{From: time.Now().Add(-time.Minute), To: time.Now().Add(time.Minute), NodeID: "node-review"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("two separate connections across receiver restart: want 2 rows, got %d", len(rows))
	}
}

func TestReviewRecentlyEndedLongConnectionMustBeQueryable(t *testing.T) {
	s, err := logstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	line := fmt.Sprintf(`{"node":"node-review","ver":1,"ts":%q,"tsc":"--"}`, time.Now().Add(-3*time.Hour).UTC().Format(time.RFC3339Nano))
	NewIngestor(s, nil, "node-review").IngestLine(line)
	rows, _, err := s.QueryConn(context.Background(), logstore.Query{From: time.Now().Add(-time.Minute), To: time.Now().Add(time.Minute), NodeID: "node-review"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("connection ended now but started 3 hours ago: want 1 recent end-time row, got %d", len(rows))
	}
}
