package dataplane

import (
	"os"
	"testing"
	"time"
)

func TestParseDockerMem(t *testing.T) {
	used, limit, ok := parseDockerMem("45.5MiB / 2GiB")
	if !ok {
		t.Fatal("expected ok")
	}
	if used < 45 || used > 46 {
		t.Fatalf("used=%v want ~45.5", used)
	}
	if limit < 2047 || limit > 2049 {
		t.Fatalf("limit=%v want ~2048", limit)
	}
}

func TestParseDockerSizeMB(t *testing.T) {
	cases := []struct {
		in   string
		want float64
	}{
		{"1024KiB", 1},
		{"1MiB", 1},
		{"1GiB", 1024},
		{"100B", 100.0 / (1024 * 1024)},
	}
	for _, c := range cases {
		got, err := parseDockerSizeMB(c.in)
		if err != nil {
			t.Fatalf("%s: %v", c.in, err)
		}
		if got < c.want*0.99 || got > c.want*1.01 {
			t.Fatalf("%s: got %v want %v", c.in, got, c.want)
		}
	}
}

func TestAttachActivityMergesLogs(t *testing.T) {
	dir := t.TempDir()
	ledger := dir + "/ledger.jsonl"
	term := dir + "/term.jsonl"
	write := func(path, ws string, ts time.Time) {
		t.Helper()
		line := `{"workspace":"` + ws + `","time":"` + ts.Format(time.RFC3339Nano) + `"}` + "\n"
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString(line); err != nil {
			t.Fatal(err)
		}
		f.Close()
	}
	t1 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 1, 1, 13, 0, 0, 0, time.UTC)
	write(ledger, "ws1", t1)
	write(term, "ws1", t2)
	write(ledger, "ws2", t1)

	statuses := []WorkspaceStatus{{ID: "ws1"}, {ID: "ws2"}, {ID: "ws3"}}
	attachActivity(statuses, ledger, term)
	if statuses[0].LastActivityAt == nil || !statuses[0].LastActivityAt.Equal(t2) {
		t.Fatalf("ws1: got %v want %v", statuses[0].LastActivityAt, t2)
	}
	if statuses[1].LastActivityAt == nil || !statuses[1].LastActivityAt.Equal(t1) {
		t.Fatalf("ws2: got %v want %v", statuses[1].LastActivityAt, t1)
	}
	if statuses[2].LastActivityAt != nil {
		t.Fatalf("ws3: unexpected %v", statuses[2].LastActivityAt)
	}
}

func TestDirDiskUsage(t *testing.T) {
	root := t.TempDir()
	d := &DirStorage{Root: root}
	mount, err := d.EnsureWorkspace(t.Context(), "ws1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mount+"/hello.txt", []byte("hello world"), 0o644); err != nil {
		t.Fatal(err)
	}
	u, err := d.DiskUsage(t.Context(), "ws1")
	if err != nil {
		t.Fatal(err)
	}
	if u.UsedBytes == 0 {
		t.Fatal("expected non-zero used bytes")
	}
	if u.QuotaBytes != 0 {
		t.Fatalf("dir backend should report no quota, got %d", u.QuotaBytes)
	}
}
