package bigqueryfront

import "net/http"

// noContentWriter answers a delete the emulator answered 200 with an empty
// body as 204 No Content, as BigQuery answers tables.delete,
// datasets.delete and models.delete (#1236). The goccy emulator writes 200
// and Content-Length: 0, and hashicorp/google 8.4.0's google_bigquery_table
// reads a 200 as a JSON body: destroying a table or view failed with "EOF".
// A 200 with a body, or any other status, passes unchanged.
type noContentWriter struct {
	http.ResponseWriter
	wrote bool
}

func (n *noContentWriter) WriteHeader(code int) {
	if !n.wrote && code == http.StatusOK && n.Header().Get("Content-Length") == "0" {
		n.Header().Del("Content-Length")
		code = http.StatusNoContent
	}
	n.wrote = true
	n.ResponseWriter.WriteHeader(code)
}

func (n *noContentWriter) Write(b []byte) (int, error) {
	n.wrote = true
	return n.ResponseWriter.Write(b)
}

// Flush passes a flush on, for a proxied response that streams.
func (n *noContentWriter) Flush() {
	if f, ok := n.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
