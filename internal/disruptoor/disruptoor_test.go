package disruptoor

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

// A four-way split is one partition with four groups, in order; disruptoor
// echoes node-index back as strings, and Partitions reads either spelling.
func TestPartitionGroupsRoundTrip(t *testing.T) {
	var put map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(body, &put); err != nil {
				t.Fatal(err)
			}
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			io.WriteString(w, `{"partitions":[{"name":"p","groups":[{"node-index":["1"]},{"node-index":["2"]},{"node-index":[3,"4"]}]}],"shaping":[]}`)
		}
	}))
	defer srv.Close()
	d := New(srv.URL, time.Second)

	if err := d.Partition("p", []int{1}, []int{2}, []int{3, 4}); err != nil {
		t.Fatal(err)
	}
	parts := put["partitions"].([]any)[0].(map[string]any)
	groups := parts["groups"].([]any)
	if len(groups) != 3 || !reflect.DeepEqual(groups[2].(map[string]any)["node-index"], []any{3.0, 4.0}) {
		t.Fatalf("groups sent = %v, want three groups with [3 4] last", groups)
	}
	if err := d.Partition("p", []int{1}); err == nil {
		t.Fatal("a single-group partition was accepted")
	}

	applied, err := d.Partitions()
	if err != nil {
		t.Fatal(err)
	}
	want := []Applied{{Name: "p", Groups: [][]int{{1}, {2}, {3, 4}}}}
	if !reflect.DeepEqual(applied, want) {
		t.Fatalf("Partitions() = %+v, want %+v", applied, want)
	}
}

// An observer left out of every group stays peered with every island and joins
// them back into one network (the 2026-10-05 offline lap: no partition took).
func TestPartitionPutsObserversWithTheFirstGroup(t *testing.T) {
	var put map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &put); err != nil {
			t.Fatal(err)
		}
	}))
	defer srv.Close()
	d := New(srv.URL, time.Second)
	d.Observers = []int{6}

	if err := d.Partition("p", []int{1}, []int{2}, []int{3}); err != nil {
		t.Fatal(err)
	}
	groups := put["partitions"].([]any)[0].(map[string]any)["groups"].([]any)
	got := make([]any, len(groups))
	for i, g := range groups {
		got[i] = g.(map[string]any)["node-index"]
	}
	if want := []any{[]any{1.0, 6.0}, []any{2.0}, []any{3.0}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("groups sent = %v, want %v", got, want)
	}
}
