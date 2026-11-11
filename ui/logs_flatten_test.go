package ui

import (
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/n0rdy/pooml/ui/templates"
)

func TestFlattenJSONObject(t *testing.T) {
	in := `{"@timestamp":"2026-09-11T15:21:13.650266+02:00","message":"wrapper failed","level":"ERROR",` +
		`"level_value":40000,"request.id":"req-123","error":{"type":"IllegalStateException","tags":["a","b"]},` +
		`"stack_trace":"java.lang.IllegalArgumentException: root\n\tat A.b(A.java:1)\n","empty":{},"flag":null}`
	fields, ok := flattenJSONObject(in)
	if !ok {
		t.Fatal("object not flattened")
	}
	want := []templates.DetailField{
		{Key: "@timestamp", Value: "2026-09-11T15:21:13.650266+02:00"},
		{Key: "message", Value: "wrapper failed"},
		{Key: "level", Value: "ERROR"},
		{Key: "level_value", Value: "40000"},
		{Key: "request.id", Value: "req-123"},
		{Key: "error.type", Value: "IllegalStateException"},
		{Key: "error.tags", Value: `["a","b"]`},
		{Key: "stack_trace", Value: "java.lang.IllegalArgumentException: root\n\tat A.b(A.java:1)\n"},
		{Key: "flag", Value: "null"},
	}
	if len(fields) != len(want) {
		t.Fatalf("fields = %+v", fields)
	}
	for i := range want {
		if fields[i] != want[i] {
			t.Errorf("field %d = %+v, want %+v", i, fields[i], want[i])
		}
	}

	for _, bad := range []string{`[1,2]`, `"str"`, `{}`, `{"a":`, `not json`} {
		if _, ok := flattenJSONObject(bad); ok {
			t.Errorf("%q flattened, want fallback", bad)
		}
	}
}

// Depth used to multiply memory (each level re-decodes what is below it): a
// 1 MB line nested 1,000 deep held 3.5 GB. Past the cap the subtree is one value.
func TestFlattenJSONObjectDepthCap(t *testing.T) {
	const depth = 5000
	in := strings.Repeat(`{"a":`, depth) + `"` + strings.Repeat("x", 256<<10) + `"` + strings.Repeat("}", depth)

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	fields, ok := flattenJSONObject(in)
	runtime.ReadMemStats(&after)
	if !ok || len(fields) != 1 {
		t.Fatalf("ok=%v fields=%d, want one verbatim field", ok, len(fields))
	}
	wantKey := strings.TrimSuffix(strings.Repeat("a.", maxFlattenDepth), ".")
	if fields[0].Key != wantKey || !strings.HasPrefix(fields[0].Value, `{"a":{"a":`) {
		t.Fatalf("key = %q (want %q), value starts %.20q", fields[0].Key, wantKey, fields[0].Value)
	}
	// uncapped this allocated ~depth x len(in) (5000x). TotalAlloc is
	// process-wide, so leave room for whatever else the test binary is doing
	if alloc, limit := after.TotalAlloc-before.TotalAlloc, uint64(256*len(in)); alloc > limit {
		t.Fatalf("flatten allocated %d MiB for a %d KiB line, want under %d MiB", alloc>>20, len(in)>>10, limit>>20)
	}
}

func TestFlattenJSONObjectFieldCap(t *testing.T) {
	var b strings.Builder
	b.WriteString("{")
	for i := range maxFlattenFields * 3 {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `"k%d":%d`, i, i)
	}
	b.WriteString("}")
	fields, ok := flattenJSONObject(b.String())
	if !ok || len(fields) != maxFlattenFields+1 {
		t.Fatalf("ok=%v fields=%d, want %d plus the marker", ok, len(fields), maxFlattenFields)
	}
	if last := fields[len(fields)-1]; last.Key != "…" {
		t.Fatalf("last field = %+v, want the truncation marker", last)
	}
}
