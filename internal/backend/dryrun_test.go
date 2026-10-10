package backend

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDryRunWritesJobs(t *testing.T) {
	p, job := label62(t)
	dir := filepath.Join(t.TempDir(), "out")
	b := NewDryRun(p, dir)
	for i := 0; i < 2; i++ {
		if err := b.Print(context.Background(), job); err != nil {
			t.Fatal(err)
		}
	}
	want := encode(t, p, job)
	for _, n := range []string{"1", "2"} {
		bin, err := os.ReadFile(filepath.Join(dir, "job-"+n+".bin"))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(bin, want) {
			t.Fatalf("job-%s.bin differs", n)
		}
		txt, err := os.ReadFile(filepath.Join(dir, "job-"+n+".txt"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(txt), "print-info flags=0x8e media=0x0a width=62 length=0 lines=151") {
			t.Fatalf("job-%s.txt:\n%s", n, txt)
		}
	}
	s, err := b.State(context.Background())
	if err != nil || s.Condition != Idle {
		t.Fatalf("%+v %v", s, err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if err := b.Print(context.Background(), job); !errors.Is(err, ErrClosed) {
		t.Fatalf("err %v", err)
	}
}

func TestDryRunContinuesNumbering(t *testing.T) {
	p, job := label62(t)
	dir := t.TempDir()
	for _, f := range []string{"job-7.bin", "job-3.bin", "job-x.bin", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(dir, f), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := NewDryRun(p, dir).Print(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "job-8.bin")); err != nil {
		t.Fatal(err)
	}
}

func TestDryRunEncodeError(t *testing.T) {
	p := profile(t)
	dir := t.TempDir()
	if err := NewDryRun(p, dir).Print(context.Background(), JobFor(p, media(t, p, "62"), nil, false)); err == nil {
		t.Fatal("no error")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("wrote %d files", len(entries))
	}
}
