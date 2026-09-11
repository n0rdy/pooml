package ui

import (
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
