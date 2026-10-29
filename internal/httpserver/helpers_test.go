package httpserver

import (
	"compress/gzip"
	"io"
)

func newGzipWriter(w io.Writer) *gzip.Writer { return gzip.NewWriter(w) }
