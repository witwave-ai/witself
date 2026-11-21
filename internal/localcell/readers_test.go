package localcell

import (
	"context"
	"errors"
	"testing"
	"time"
)

// readerJournalLine is one complete journal record that opens an operation.
const readerJournalLine = `{"operation":"install","step":"operation","state":"started","at":"2026-01-01T00:00:00Z"}` + "\n"

// readerRows are the readers that must never wait on what they open: the name
// each reads in a reader cell, a read that also proves the content arrived, and
// the reader's existing refusal of a non-regular file.
var readerRows = []struct {
	name, refusal string
	path          func(c *Cell) string
	read          func(c *Cell) error
}{
	{"readJSON", "local cell record must be a regular file", func(c *Cell) string { return c.path("manifest.json") }, func(c *Cell) error {
		m, err := readManifest(context.Background(), c.cfg.Root)
		if err == nil && m.InstallationID != "lc_test" {
			return errors.New("manifest content lost")
		}
		return err
	}},
	{"readJournal", "invalid local cell journal", readerJournalPath, func(c *Cell) error { return readerJournalRead(c.readJournal(context.Background())) }},
	{"readUpdateJournal", "invalid local cell journal", readerJournalPath, func(c *Cell) error { return readerJournalRead(c.readUpdateJournal()) }},
}

func readerJournalPath(c *Cell) string { return c.path("journal", "operations.jsonl") }

func readerJournalRead(progress journalProgress, err error) error {
	if err == nil && (progress.pending == nil || progress.durableBytes != int64(len(readerJournalLine))) {
		return errors.New("journal content lost")
	}
	return err
}

// readerCell is a unit cell whose manifest and journal are valid regular files.
func readerCell(t *testing.T) *Cell {
	t.Helper()
	c, _ := unitCell(t)
	if err := testWriteFile(readerJournalPath(c), []byte(readerJournalLine), 0o600); err != nil {
		t.Fatal("cannot prepare the reader journal")
	}
	return c
}

// readerWatchdog runs the read in a goroutine; one still running after ten
// seconds is unblocked, awaited again and fails the row (as drillRootWatchdog).
func readerWatchdog(t *testing.T, read func() error, unblock func()) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- read() }()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
	}
	unblock()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
	}
	t.Fatal("local cell reader blocked on what stands at its name")
	return nil
}

func TestReadersStillReadRegularFiles(t *testing.T) {
	for _, row := range readerRows {
		t.Run(row.name, func(t *testing.T) {
			c := readerCell(t)
			if err := readerWatchdog(t, func() error { return row.read(c) }, func() {}); err != nil {
				t.Fatalf("reader refused its regular file: %v", err)
			}
		})
	}
}
