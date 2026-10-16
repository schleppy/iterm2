package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/schleppy/iterm2"
)

// runWatch prints the topology each time it changes, until ctx is canceled.
func runWatch(ctx context.Context, opts iterm2.Options, p printer, verbose bool) {
	level := slog.LevelWarn
	if verbose {
		level = slog.LevelDebug
	}
	w := iterm2.NewWatcher(iterm2.WatcherOptions{
		Client: opts,
		Logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})),
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for snap := range w.Updates() {
			state := ""
			if snap.Stale {
				state = " (stale: disconnected, reconnecting)"
			}
			fmt.Fprintf(p.w, "--- %s%s\n", snap.UpdatedAt.Format(time.TimeOnly), state)
			p.print(&snap)
		}
	}()
	w.Run(ctx) // returns only when ctx is canceled
	<-done
}
