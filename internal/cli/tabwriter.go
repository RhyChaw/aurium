package cli

import (
	"io"
	"text/tabwriter"
)

type tabwriterFlusher struct{ *tabwriter.Writer }

func newTabWriter(w io.Writer) *tabwriterFlusher {
	return &tabwriterFlusher{tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)}
}

func (t *tabwriterFlusher) Flush() error { return t.Writer.Flush() }
