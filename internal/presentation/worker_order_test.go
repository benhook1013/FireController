package presentation_test

import (
	"net/url"
	"reflect"
	"testing"
)

func TestWorkerOrderAcrossSelectorsDeliveryPaginationAndClear(t *testing.T) {
	f := setup(t)
	base := "/api/v1/projects/" + f.project
	create := func(name string) string {
		w := f.call("POST", base+"/workers", f.admin, "worker-order-create-"+name, map[string]string{"name": name})
		expect(t, w, 200)
		var value struct {
			ID string `json:"id"`
		}
		decode(t, w, &value)
		return value.ID
	}
	alpha, beta, overseer := create("alpha"), create("Beta"), create("Overseer")
	config := map[string]any{"name": "one", "worker_order": []string{overseer, beta}}
	request := map[string]any{"expected_revision": 0, "presentation": config}
	expect(t, f.call("PUT", f.base(), f.admin, "worker-order-set", request), 200)
	before := f.counts()
	expect(t, f.call("PUT", f.base(), f.admin, "worker-order-set", request), 200)
	if !reflect.DeepEqual(before, f.counts()) {
		t.Fatal("replay changed state")
	}
	for _, delivery := range []bool{false, true} {
		var got []string
		cursor := ""
		for {
			path := base + "/workers?limit=2"
			if delivery {
				path = base + "/delivery?worker_limit=2&job_limit=1"
			}
			if cursor != "" {
				key := "cursor"
				if delivery {
					key = "worker_cursor"
				}
				path += "&" + key + "=" + url.QueryEscape(cursor)
			}
			w := f.call("GET", path, f.admin, "", nil)
			expect(t, w, 200)
			type page struct {
				Items []struct {
					ID string `json:"id"`
				} `json:"items"`
				Cursor  string `json:"cursor"`
				HasMore bool   `json:"has_more"`
			}
			var p page
			if delivery {
				var v struct {
					Workers page `json:"workers"`
				}
				decode(t, w, &v)
				p = v.Workers
			} else {
				decode(t, w, &p)
			}
			for _, worker := range p.Items {
				got = append(got, worker.ID)
			}
			if !p.HasMore {
				break
			}
			if p.Cursor == "" || p.Cursor == cursor {
				t.Fatal("continuation did not advance")
			}
			cursor = p.Cursor
		}
		if want := []string{overseer, beta, alpha, f.worker}; !reflect.DeepEqual(got, want) {
			t.Fatalf("delivery=%v got=%v want=%v", delivery, got, want)
		}
	}
	if err := f.validate(); err != nil {
		t.Fatal("ordered snapshot invalid", err)
	}
	for name, ids := range map[string][]string{"duplicate": {overseer, overseer}, "foreign-or-missing": {"worker_missing"}} {
		expect(t, f.call("PUT", f.base(), f.admin, "worker-order-"+name, map[string]any{"expected_revision": 1, "presentation": map[string]any{"name": "one", "worker_order": ids}}), 400)
	}
	if !reflect.DeepEqual(before, f.counts()) {
		t.Fatal("reads or rejected writes changed state")
	}
	// Clearing order returns to alphabetical display without deleting workers.
	expect(t, f.call("PUT", f.base(), f.admin, "worker-order-clear", map[string]any{"expected_revision": 1, "presentation": map[string]any{"name": "one", "worker_order": []string{}}}), 200)
	w := f.call("GET", base+"/workers?limit=1", f.admin, "", nil)
	expect(t, w, 200)
	var first struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	decode(t, w, &first)
	if len(first.Items) != 1 || first.Items[0].ID != alpha {
		t.Fatal("clearing pins did not restore alphabetical order", w.Body.String())
	}
}
