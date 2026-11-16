package scenario

import (
	"encoding/json"
	"lumina/parity/normalize"
	"testing"
)

func TestSplitIgnoreSeparatesSnapshotEntries(t *testing.T) {
	body, snaps, err := splitIgnore([]string{"$.files", "snapshot:memory_chunks", "snapshot:vault_files.metadata.title", "snapshot:memory_chunks.id"})
	if err != nil {
		t.Fatal(err)
	}
	if len(body) != 1 || body[0] != "$.files" {
		t.Fatalf("body = %v", body)
	}
	if !snaps.skips("memory_chunks") {
		t.Fatal("memory_chunks should be skipped whole, even with a column entry after it")
	}
	if snaps.skips("vault_files") {
		t.Fatal("vault_files only loses a key")
	}
}

func TestStripRemovesKeysAndNullsEmptiedObjects(t *testing.T) {
	_, snaps, err := splitIgnore([]string{"snapshot:vault_files.metadata.title"})
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		`{"id":1,"metadata":{"title":"n1"}}`:              `{"id":1,"metadata":null}`,
		`{"id":1,"metadata":{"title":"n1","tags":["a"]}}`: `{"id":1,"metadata":{"tags":["a"]}}`,
		`{"id":1,"metadata":null}`:                        `{"id":1,"metadata":null}`,
		`{"id":1}`:                                        `{"id":1}`,
	}
	for in, want := range cases {
		got, err := snaps.strip("vault_files", []json.RawMessage{json.RawMessage(in)})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || string(got[0]) != want {
			t.Errorf("strip(%s) = %s, want %s", in, got, want)
		}
	}
	same, _ := snaps.strip("memories", []json.RawMessage{json.RawMessage(`{"metadata":{"title":"kept"}}`)})
	if string(same[0]) != `{"metadata":{"title":"kept"}}` {
		t.Errorf("another table changed: %s", same)
	}
}

func TestDiffAcceptsAnIgnoredFieldTheOtherSideLacks(t *testing.T) {
	swift := Transcript{Steps: []StepResult{{Name: "s", Body: map[string]any{"imported": 1}}}}
	goT := Transcript{Steps: []StepResult{{Name: "s", Body: map[string]any{"imported": 1, "files": normalize.Ignored}}}}
	if d := Diff(swift, goT); len(d) != 0 {
		t.Fatalf("diff = %+v", d)
	}
	goT.Steps[0].Body = map[string]any{"imported": 1, "files": []any{}}
	if d := Diff(swift, goT); len(d) != 1 {
		t.Fatalf("a field that is not ignored must still differ, got %+v", d)
	}
	if d := Diff(goT2(normalize.Ignored), Transcript{Steps: []StepResult{{Name: "s", Body: map[string]any{"imported": 1}}}}); len(d) != 1 {
		t.Fatalf("Go leaving out an ignored field Swift sends must differ, got %+v", d)
	}
}

func TestStripDropsMatchingRowsAndKeepsNumbersExact(t *testing.T) {
	_, snaps, err := splitIgnore([]string{`snapshot:memory_chunks[source_path~^inbox/real\.md$]`})
	if err != nil {
		t.Fatal(err)
	}
	rows := []json.RawMessage{
		json.RawMessage(`{"source_path":"inbox/real.md","ordinal":0}`),
		json.RawMessage(`{"source_path":"notes/real.md","ordinal":9007199254740993}`),
		json.RawMessage(`{"source_path":null,"ordinal":1}`),
	}
	got, err := snaps.strip("memory_chunks", rows)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || string(got[0]) != `{"ordinal":9007199254740993,"source_path":"notes/real.md"}` {
		t.Fatalf("got %s", got)
	}
	if _, _, err := splitIgnore([]string{"snapshot:t[c~(]"}); err == nil {
		t.Fatal("a bad regex must fail the scenario")
	}
}

func goT2(files any) Transcript {
	return Transcript{Steps: []StepResult{{Name: "s", Body: map[string]any{"imported": 1, "files": files}}}}
}

func TestDiffAcceptsAGoOnlyObjectOfIgnoredFields(t *testing.T) {
	swift := Transcript{Steps: []StepResult{{Name: "s", Body: map[string]any{"path": "a.md"}}}}
	goT := Transcript{Steps: []StepResult{{Name: "s", Body: map[string]any{"path": "a.md", "metadata": map[string]any{"title": normalize.Ignored}}}}}
	if d := Diff(swift, goT); len(d) != 0 {
		t.Fatalf("diff = %+v", d)
	}
	goT.Steps[0].Body = map[string]any{"path": "a.md", "metadata": map[string]any{"title": normalize.Ignored, "tags": []any{}}}
	if d := Diff(swift, goT); len(d) != 1 {
		t.Fatalf("an object with a compared field must differ, got %+v", d)
	}
}
