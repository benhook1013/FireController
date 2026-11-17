package adoptionholds

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestReadCostBoundedWindow(t *testing.T) {
	c := new(costCollector)
	now := time.Unix(100, 0)
	if got := c.record(now, time.Millisecond, readCost{3}, errors.New("secret-token")); got != nil {
		t.Fatal(got)
	}
	if got := c.record(now.Add(59*time.Second), 50*time.Millisecond, readCost{2}, nil); got != nil {
		t.Fatal(got)
	}
	got := c.record(now.Add(time.Minute), 2*time.Second, readCost{7}, context.DeadlineExceeded)
	if got == nil || got.Reads != 3 || got.Records != 12 || got.Errors != 2 || got.Deadlines != 1 || got.MaxUS != 2000000 || got.ElapsedUS != 2051000 || got.Buckets != [5]uint64{1, 1, 2, 2, 3} {
		t.Fatalf("%+v", got)
	}
	encoded, _ := json.Marshal(got)
	if strings.Contains(string(encoded), "secret") {
		t.Fatal(string(encoded))
	}
	if got := c.record(now.Add(61*time.Second), time.Microsecond, readCost{}, nil); got != nil {
		t.Fatal(got)
	}
	next := c.record(now.Add(120*time.Second), time.Microsecond, readCost{}, nil)
	if next == nil || next.Reads != 2 || next.Records != 0 || next.Errors != 0 {
		t.Fatal(next)
	}
}
func TestReadCostConcurrent(t *testing.T) {
	c := new(costCollector)
	now := time.Unix(100, 0)
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); c.record(now, time.Millisecond, readCost{1}, nil) }()
	}
	wg.Wait()
	got := c.record(now.Add(time.Minute), time.Millisecond, readCost{1}, nil)
	if got == nil || got.Reads != 101 || got.Records != 101 {
		t.Fatal(got)
	}
}
